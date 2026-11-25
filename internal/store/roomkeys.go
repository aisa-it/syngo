package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// ErrRoomKeysNotFound — нет бэкапа ключей, версии или сессии (404 M_NOT_FOUND).
// Ошибка и типы объявлены в core: store реализует core.KeyBackupStore.
var ErrRoomKeysNotFound = core.ErrRoomKeysNotFound

// RoomKeyVersion — версия серверного бэкапа ключей (E2EE).
type RoomKeyVersion = core.RoomKeyVersion

// RoomKeysStat — etag и число ключей версии (ответ PUT/DELETE /room_keys/keys).
type RoomKeysStat = core.RoomKeysStat

var _ core.KeyBackupStore = (*DB)(nil)

// CurrentRoomKeyVersion возвращает последнюю неудаленную версию бэкапа пользователя.
func (db *DB) CurrentRoomKeyVersion(ctx context.Context, userID string) (RoomKeyVersion, error) {
	return db.roomKeyVersion(ctx, userID, "")
}

// RoomKeyVersionByID — конкретная неудаленная версия бэкапа.
func (db *DB) RoomKeyVersionByID(ctx context.Context, userID, version string) (RoomKeyVersion, error) {
	if version == "" {
		return RoomKeyVersion{}, ErrRoomKeysNotFound
	}

	return db.roomKeyVersion(ctx, userID, version)
}

func (db *DB) roomKeyVersion(ctx context.Context, userID, version string) (RoomKeyVersion, error) {
	out := RoomKeyVersion{}

	var authData []byte

	err := db.pool.QueryRow(ctx, `
		SELECT version, algorithm, auth_data, etag FROM room_keys_versions
		WHERE user_id = $1 AND NOT deleted AND ($2 = '' OR version = $2)
		ORDER BY CASE WHEN version ~ '^[0-9]+$' THEN version::bigint ELSE 0 END DESC
		LIMIT 1`, userID, version).Scan(&out.Version, &out.Algorithm, &authData, &out.Etag)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RoomKeyVersion{}, ErrRoomKeysNotFound
		}

		return RoomKeyVersion{}, fmt.Errorf("store: room keys version: %w", err)
	}

	out.AuthData = authData

	if out.Etag == "" {
		out.Etag = "0"
	}

	if out.Count, err = db.roomKeysCount(ctx, db.pool, userID, out.Version); err != nil {
		return RoomKeyVersion{}, err
	}

	return out, nil
}

// CreateRoomKeyVersion создает новую версию бэкапа; она становится текущей.
// Номера удаленных версий повторно не выдаются.
func (db *DB) CreateRoomKeyVersion(ctx context.Context, userID, algorithm string, authData json.RawMessage) (RoomKeyVersion, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return RoomKeyVersion{}, err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Сериализация создания версий одного пользователя.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('room_keys:' || $1))`, userID); err != nil {
		return RoomKeyVersion{}, fmt.Errorf("store: lock room keys: %w", err)
	}

	var next int64

	if err := tx.QueryRow(ctx, `
		SELECT coalesce(max(version::bigint), 0) + 1 FROM room_keys_versions
		WHERE user_id = $1 AND version ~ '^[0-9]+$'`, userID).Scan(&next); err != nil {
		return RoomKeyVersion{}, fmt.Errorf("store: next room keys version: %w", err)
	}

	version := strconv.FormatInt(next, 10)

	if _, err := tx.Exec(ctx, `
		INSERT INTO room_keys_versions (user_id, version, algorithm, auth_data, etag)
		VALUES ($1,$2,$3,$4,'0')`, userID, version, algorithm, authData); err != nil {
		return RoomKeyVersion{}, fmt.Errorf("store: insert room keys version: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO room_keys_current (user_id, version) VALUES ($1,$2)
		ON CONFLICT (user_id) DO UPDATE SET version = EXCLUDED.version`, userID, version); err != nil {
		return RoomKeyVersion{}, fmt.Errorf("store: set current room keys version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RoomKeyVersion{}, err
	}

	return RoomKeyVersion{Version: version, Algorithm: algorithm, AuthData: authData, Etag: "0"}, nil
}

// UpdateRoomKeyVersion обновляет auth_data версии.
func (db *DB) UpdateRoomKeyVersion(ctx context.Context, userID, version, algorithm string, authData json.RawMessage) error {
	tag, err := db.pool.Exec(ctx, `
		UPDATE room_keys_versions SET algorithm = $3, auth_data = $4
		WHERE user_id = $1 AND version = $2 AND NOT deleted`, userID, version, algorithm, authData)
	if err != nil {
		return fmt.Errorf("store: update room keys version: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrRoomKeysNotFound
	}

	return nil
}

// DeleteRoomKeyVersion помечает версию удаленной и стирает ее ключи.
func (db *DB) DeleteRoomKeyVersion(ctx context.Context, userID, version string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE room_keys_versions SET deleted = true
		WHERE user_id = $1 AND version = $2 AND NOT deleted`, userID, version)
	if err != nil {
		return fmt.Errorf("store: delete room keys version: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrRoomKeysNotFound
	}

	if _, err := tx.Exec(ctx, `DELETE FROM room_keys WHERE user_id = $1 AND version = $2`, userID, version); err != nil {
		return fmt.Errorf("store: delete room keys: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM room_keys_current WHERE user_id = $1 AND version = $2`, userID, version); err != nil {
		return fmt.Errorf("store: reset current room keys version: %w", err)
	}

	return tx.Commit(ctx)
}

// keyRank — поля ключа сессии, по которым выбирается лучший (спецификация,
// «Storing keys»): проверенный лучше непроверенного, затем меньший
// first_message_index, затем меньший forwarded_count.
type keyRank struct {
	FirstMessageIndex int64 `json:"first_message_index"`
	ForwardedCount    int64 `json:"forwarded_count"`
	IsVerified        bool  `json:"is_verified"`
}

func betterKey(candidate, existing json.RawMessage) bool {
	var c, e keyRank
	if json.Unmarshal(candidate, &c) != nil {
		return false
	}

	if json.Unmarshal(existing, &e) != nil {
		return true
	}

	if c.IsVerified != e.IsVerified {
		return c.IsVerified
	}

	if c.FirstMessageIndex != e.FirstMessageIndex {
		return c.FirstMessageIndex < e.FirstMessageIndex
	}

	return c.ForwardedCount < e.ForwardedCount
}

// SaveRoomKeys сохраняет ключи в версию: существующий ключ сессии заменяется только
// лучшим. Если что-то изменилось, etag увеличивается. Версия должна существовать.
func (db *DB) SaveRoomKeys(ctx context.Context, userID, version string, keys map[string]map[string]json.RawMessage) (RoomKeysStat, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return RoomKeysStat{}, err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var etag string

	err = tx.QueryRow(ctx, `
		SELECT etag FROM room_keys_versions WHERE user_id = $1 AND version = $2 AND NOT deleted
		FOR UPDATE`, userID, version).Scan(&etag)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RoomKeysStat{}, ErrRoomKeysNotFound
		}

		return RoomKeysStat{}, fmt.Errorf("store: lock room keys version: %w", err)
	}

	changed := false

	for roomID, sessions := range keys {
		for sessionID, key := range sessions {
			var existing []byte

			err := tx.QueryRow(ctx, `
				SELECT key FROM room_keys WHERE user_id = $1 AND version = $2 AND room_id = $3 AND session_id = $4`,
				userID, version, roomID, sessionID).Scan(&existing)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return RoomKeysStat{}, fmt.Errorf("store: read room key: %w", err)
			}

			if existing != nil && !betterKey(key, existing) {
				continue
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO room_keys (user_id, version, room_id, session_id, key)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (user_id, version, room_id, session_id)
				DO UPDATE SET key = EXCLUDED.key`,
				userID, version, roomID, sessionID, []byte(key)); err != nil {
				return RoomKeysStat{}, fmt.Errorf("store: save room key: %w", err)
			}

			changed = true
		}
	}

	if changed {
		if etag, err = bumpEtag(ctx, tx, userID, version, etag); err != nil {
			return RoomKeysStat{}, err
		}
	}

	return db.commitStat(ctx, tx, userID, version, etag)
}

// DeleteRoomKeys удаляет ключи версии: все, комнаты или одной сессии.
func (db *DB) DeleteRoomKeys(ctx context.Context, userID, version, roomID, sessionID string) (RoomKeysStat, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return RoomKeysStat{}, err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var etag string

	err = tx.QueryRow(ctx, `
		SELECT etag FROM room_keys_versions WHERE user_id = $1 AND version = $2 AND NOT deleted
		FOR UPDATE`, userID, version).Scan(&etag)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RoomKeysStat{}, ErrRoomKeysNotFound
		}

		return RoomKeysStat{}, fmt.Errorf("store: lock room keys version: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM room_keys WHERE user_id = $1 AND version = $2
		  AND ($3 = '' OR room_id = $3) AND ($4 = '' OR session_id = $4)`, userID, version, roomID, sessionID)
	if err != nil {
		return RoomKeysStat{}, fmt.Errorf("store: delete room keys: %w", err)
	}

	if tag.RowsAffected() > 0 {
		if etag, err = bumpEtag(ctx, tx, userID, version, etag); err != nil {
			return RoomKeysStat{}, err
		}
	}

	return db.commitStat(ctx, tx, userID, version, etag)
}

func bumpEtag(ctx context.Context, tx pgx.Tx, userID, version, etag string) (string, error) {
	n, _ := strconv.ParseInt(etag, 10, 64)
	next := strconv.FormatInt(n+1, 10)

	if _, err := tx.Exec(ctx, `UPDATE room_keys_versions SET etag = $3 WHERE user_id = $1 AND version = $2`,
		userID, version, next); err != nil {
		return "", fmt.Errorf("store: bump room keys etag: %w", err)
	}

	return next, nil
}

func (db *DB) commitStat(ctx context.Context, tx pgx.Tx, userID, version, etag string) (RoomKeysStat, error) {
	count, err := db.roomKeysCount(ctx, tx, userID, version)
	if err != nil {
		return RoomKeysStat{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RoomKeysStat{}, err
	}

	if etag == "" {
		etag = "0"
	}

	return RoomKeysStat{Etag: etag, Count: count}, nil
}

// RoomKeys возвращает ключи версии: room_id -> session_id -> key. roomID и
// sessionID сужают выборку.
func (db *DB) RoomKeys(ctx context.Context, userID, version, roomID, sessionID string) (map[string]map[string]json.RawMessage, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT room_id, session_id, key FROM room_keys
		WHERE user_id = $1 AND version = $2 AND ($3 = '' OR room_id = $3) AND ($4 = '' OR session_id = $4)`,
		userID, version, roomID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: room keys: %w", err)
	}

	defer rows.Close()

	out := map[string]map[string]json.RawMessage{}

	for rows.Next() {
		var (
			rid, sid string
			key      []byte
		)

		if err := rows.Scan(&rid, &sid, &key); err != nil {
			return nil, err
		}

		if out[rid] == nil {
			out[rid] = map[string]json.RawMessage{}
		}

		out[rid][sid] = key
	}

	return out, rows.Err()
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (db *DB) roomKeysCount(ctx context.Context, q queryRower, userID, version string) (int, error) {
	var count int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM room_keys WHERE user_id = $1 AND version = $2`, userID, version).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count room keys: %w", err)
	}

	return count, nil
}
