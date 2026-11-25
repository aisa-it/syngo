package store

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushers"
)

// ErrTooManyPushers - у пользователя уже MaxPushersPerUser pushers.
// Объявлена в core: store реализует core.PushersStore.
var ErrTooManyPushers = core.ErrTooManyPushers

var _ core.PushersStore = (*DB)(nil)

// UpsertPusher добавляет или обновляет pusher пользователя.
//
// pusher привязан к устройству (device_id); без append (по умолчанию)
// тот же app_id+pushkey у других пользователей удаляется - после смены
// пользователя на телефоне push прежнему не идет (спецификация, append);
// число pushers пользователя ограничено maxPerUser (0 - без лимита).
func (db *DB) UpsertPusher(ctx context.Context, pusher pushers.Pusher, maxPerUser int) error {
	const query = `
		INSERT INTO pushers (user_id, app_id, pushkey, kind, app_display_name,
		                     device_display_name, profile_tag, lang, data, device_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (user_id, app_id, pushkey) DO UPDATE
		SET kind = EXCLUDED.kind, app_display_name = EXCLUDED.app_display_name,
		    device_display_name = EXCLUDED.device_display_name,
		    profile_tag = EXCLUDED.profile_tag, lang = EXCLUDED.lang,
		    data = EXCLUDED.data, device_id = EXCLUDED.device_id, updated_at = now()`

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: upsert pusher begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Сериализация по пользователю: параллельные /pushers/set не обходят лимит.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('pushers:' || $1, 0))`,
		pusher.UserID); err != nil {
		return fmt.Errorf("store: upsert pusher lock: %w", err)
	}

	if !pusher.Append {
		if _, err := tx.Exec(ctx,
			`DELETE FROM pushers WHERE app_id = $1 AND pushkey = $2 AND user_id <> $3`,
			pusher.AppID, pusher.Pushkey, pusher.UserID); err != nil {
			return fmt.Errorf("store: upsert pusher replace: %w", err)
		}
	}

	if maxPerUser > 0 {
		var others int

		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pushers
			WHERE user_id = $1 AND NOT (app_id = $2 AND pushkey = $3)`,
			pusher.UserID, pusher.AppID, pusher.Pushkey).Scan(&others); err != nil {
			return fmt.Errorf("store: upsert pusher count: %w", err)
		}

		if others >= maxPerUser {
			return ErrTooManyPushers
		}
	}

	if _, err := tx.Exec(ctx, query,
		pusher.UserID, pusher.AppID, pusher.Pushkey, pusher.Kind,
		pusher.AppDisplayName, pusher.DeviceDisplayName, pusher.ProfileTag,
		pusher.Lang, []byte(pusher.Data), nullable(pusher.DeviceID),
	); err != nil {
		return fmt.Errorf("store: upsert pusher: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: upsert pusher commit: %w", err)
	}

	return nil
}

// DeletePusher удаляет pusher по ключу (kind=null).
func (db *DB) DeletePusher(ctx context.Context, userID, appID, pushkey string) error {
	if _, err := db.pool.Exec(ctx,
		`DELETE FROM pushers WHERE user_id = $1 AND app_id = $2 AND pushkey = $3`,
		userID, appID, pushkey); err != nil {
		return fmt.Errorf("store: delete pusher: %w", err)
	}

	return nil
}

// Pushers возвращает все pushers пользователя.
func (db *DB) Pushers(ctx context.Context, userID string) ([]pushers.Pusher, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT app_id, pushkey, kind, app_display_name, device_display_name, profile_tag, lang, data
		FROM pushers WHERE user_id = $1 ORDER BY app_id, pushkey`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: pushers: %w", err)
	}

	defer rows.Close()

	var out []pushers.Pusher

	for rows.Next() {
		var (
			p    pushers.Pusher
			data []byte
		)

		if err := rows.Scan(&p.AppID, &p.Pushkey, &p.Kind, &p.AppDisplayName,
			&p.DeviceDisplayName, &p.ProfileTag, &p.Lang, &data); err != nil {
			return nil, fmt.Errorf("store: scan pusher: %w", err)
		}

		p.UserID = userID
		p.Data = data
		out = append(out, p)
	}

	return out, rows.Err()
}

// RoomPushers возвращает http-pushers участников комнаты, кроме отправителя
// события. extra - пользователи вне join, которым событие адресовано
// (приглашенный): правило .m.rule.invite_for_me без них не срабатывало.
func (db *DB) RoomPushers(ctx context.Context, roomID, exceptUser string, extra ...string) ([]pushers.Pusher, error) {
	if extra == nil {
		extra = []string{}
	}

	rows, err := db.pool.Query(ctx, `
		SELECT p.user_id, p.app_id, p.pushkey, p.kind, p.app_display_name,
		       p.device_display_name, p.profile_tag, p.lang, p.data
		FROM pushers p
		WHERE p.kind = 'http' AND p.user_id <> $2
		  AND (p.user_id = ANY($3::text[]) OR EXISTS (
		      SELECT 1 FROM room_memberships m
		      WHERE m.room_id = $1 AND m.user_id = p.user_id AND m.membership = 'join'))
		ORDER BY p.user_id, p.app_id, p.pushkey`, roomID, exceptUser, extra)
	if err != nil {
		return nil, fmt.Errorf("store: room pushers: %w", err)
	}

	defer rows.Close()

	var out []pushers.Pusher

	for rows.Next() {
		var (
			p    pushers.Pusher
			data []byte
		)

		if err := rows.Scan(&p.UserID, &p.AppID, &p.Pushkey, &p.Kind, &p.AppDisplayName,
			&p.DeviceDisplayName, &p.ProfileTag, &p.Lang, &data); err != nil {
			return nil, fmt.Errorf("store: scan room pusher: %w", err)
		}

		p.Data = data
		out = append(out, p)
	}

	return out, rows.Err()
}
