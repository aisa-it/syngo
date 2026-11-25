package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// writeAccountDataAt выполняет запись account data с позицией, выданной заранее и
// зарегистрированной как незакоммиченная. /sync отдает account data с позицией из
// (since, next_batch]: без регистрации запись, закоммиченная позже
// события с большим номером, оказалась бы ниже уже выданного next_batch и не
// дошла бы до клиента никогда (как события).
func (db *DB) writeAccountDataAt(ctx context.Context, query string, args ...any) (int64, error) {
	var position int64

	if err := db.pool.QueryRow(ctx, `SELECT nextval('events_stream_ordering_seq')`).Scan(&position); err != nil {
		return 0, fmt.Errorf("store: account data position: %w", err)
	}

	db.registerPendingPosition(position)
	defer db.forgetPendingPosition(position)

	tag, err := db.pool.Exec(ctx, query, append(args, position)...)
	if err != nil {
		return 0, err
	}

	return tag.RowsAffected(), nil
}

// SetAccountData сохраняет account data пользователя (roomID == "" — глобальные).
func (db *DB) SetAccountData(ctx context.Context, userID, roomID, eventType string, content []byte) error {
	const query = `
		INSERT INTO account_data (user_id, room_id, type, content, stream_ordering)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (user_id, room_id, type) DO UPDATE
		SET content = EXCLUDED.content, updated_at = now(),
		    stream_ordering = EXCLUDED.stream_ordering`

	if _, err := db.writeAccountDataAt(ctx, query, userID, roomID, eventType, content); err != nil {
		return fmt.Errorf("store: set account data: %w", err)
	}

	return nil
}

// AccountData возвращает account data по типу (roomID == "" — глобальные). Нет
// записи - core.ErrNotFound; остальные ошибки - сбой хранилища (раньше
// API отдавал на любую ошибку 404).
func (db *DB) AccountData(ctx context.Context, userID, roomID, eventType string) ([]byte, error) {
	var content []byte

	// content {} - удаленная запись (MSC3391): для GET ее нет.
	err := db.pool.QueryRow(ctx,
		`SELECT content FROM account_data WHERE user_id=$1 AND room_id=$2 AND type=$3 AND content <> '{}'::jsonb`,
		userID, roomID, eventType).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: account data %s: %w", eventType, core.ErrNotFound)
	}

	if err != nil {
		return nil, fmt.Errorf("store: account data %s: %w", eventType, err)
	}

	return content, nil
}

// AccountDataEvents возвращает все account data пользователя: глобальные и по комнатам.
func (db *DB) AccountDataEvents(ctx context.Context, userID string) (global map[string][]byte, perRoom map[string]map[string][]byte, err error) {
	rows, err := db.pool.Query(ctx,
		`SELECT room_id, type, content FROM account_data WHERE user_id = $1`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: account data events: %w", err)
	}

	return scanAccountData(rows)
}

// AccountDataChanges - account data для /sync: записи с позицией из
// (since, upper]; при since == 0 (initial sync) и для комнат fullRooms (вход после
// since) - все записи до upper, кроме удаленных. Удаления после since приходят как
// content {} (MSC3391). Раньше каждый /sync отдавал все account data заново.
func (db *DB) AccountDataChanges(
	ctx context.Context, userID string, since, upper int64, fullRooms []string,
) (global map[string][]byte, perRoom map[string]map[string][]byte, err error) {
	if fullRooms == nil {
		fullRooms = []string{}
	}

	rows, err := db.pool.Query(ctx, `
		SELECT room_id, type, content FROM account_data
		WHERE user_id = $1 AND stream_ordering <= $3
		  AND (($2 > 0 AND stream_ordering > $2)
		       OR (($2 = 0 OR room_id = ANY($4)) AND content <> '{}'::jsonb))`,
		userID, since, upper, fullRooms)
	if err != nil {
		return nil, nil, fmt.Errorf("store: account data changes: %w", err)
	}

	return scanAccountData(rows)
}

// RoomAccountDataOfType - комнатная account data одного типа по комнатам (без
// удаленных): например, m.fully_read всех комнат пользователя.
func (db *DB) RoomAccountDataOfType(ctx context.Context, userID, eventType string) (map[string][]byte, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT room_id, content FROM account_data
		WHERE user_id = $1 AND type = $2 AND room_id <> '' AND content <> '{}'::jsonb`,
		userID, eventType)
	if err != nil {
		return nil, fmt.Errorf("store: room account data %s: %w", eventType, err)
	}

	defer rows.Close()

	out := map[string][]byte{}

	for rows.Next() {
		var (
			roomID  string
			content []byte
		)

		if err := rows.Scan(&roomID, &content); err != nil {
			return nil, fmt.Errorf("store: scan room account data: %w", err)
		}

		out[roomID] = content
	}

	return out, rows.Err()
}

// scanAccountData раскладывает строки (room_id, type, content) на глобальные и
// комнатные.
func scanAccountData(rows pgx.Rows) (global map[string][]byte, perRoom map[string]map[string][]byte, err error) {
	defer rows.Close()

	global = map[string][]byte{}
	perRoom = map[string]map[string][]byte{}

	for rows.Next() {
		var (
			roomID, eventType string
			content           []byte
		)

		if err := rows.Scan(&roomID, &eventType, &content); err != nil {
			return nil, nil, fmt.Errorf("store: scan account data: %w", err)
		}

		if roomID == "" {
			global[eventType] = content

			continue
		}

		if perRoom[roomID] == nil {
			perRoom[roomID] = map[string][]byte{}
		}

		perRoom[roomID][eventType] = content
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate account data: %w", err)
	}

	return global, perRoom, nil
}

// DeleteAccountData удаляет account data по типу (roomID == "" — глобальные). Как в
// MSC3391 и Synapse, запись остается с content {} и новой позицией: так удаление
// доходит до клиентов через /sync.
func (db *DB) DeleteAccountData(ctx context.Context, userID, roomID, eventType string) error {
	affected, err := db.writeAccountDataAt(ctx, `
		UPDATE account_data SET content = '{}'::jsonb, updated_at = now(),
		       stream_ordering = $4
		WHERE user_id=$1 AND room_id=$2 AND type=$3 AND content <> '{}'::jsonb`,
		userID, roomID, eventType)
	if err != nil {
		return fmt.Errorf("store: delete account data: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("store: account data %s: %w", eventType, core.ErrNotFound)
	}

	return nil
}

// accountDataTag - тип комнатной account data с тегами комнаты.
const accountDataTag = core.AccountDataTag

var _ core.AccountDataStore = (*DB)(nil)

// SetRoomTag добавляет или заменяет тег комнаты одним запросом. Раньше
// теги менялись чтением-изменением-записью в API, и параллельный PUT соседнего тега
// затирал этот: ON CONFLICT DO UPDATE берет блокировку строки и меняет актуальное
// содержимое.
func (db *DB) SetRoomTag(ctx context.Context, userID, roomID, tag string, content []byte) error {
	const query = `
		INSERT INTO account_data (user_id, room_id, type, content, stream_ordering)
		VALUES ($1, $2, $3, jsonb_build_object('tags', jsonb_build_object($4::text, $5::jsonb)), $6)
		ON CONFLICT (user_id, room_id, type) DO UPDATE
		SET content = jsonb_build_object('tags',
		        CASE WHEN jsonb_typeof(account_data.content->'tags') = 'object'
		             THEN account_data.content->'tags' ELSE '{}'::jsonb END
		        || jsonb_build_object($4::text, $5::jsonb)),
		    updated_at = now(), stream_ordering = EXCLUDED.stream_ordering`

	if _, err := db.writeAccountDataAt(ctx, query, userID, roomID, accountDataTag, tag, string(content)); err != nil {
		return fmt.Errorf("store: set room tag: %w", err)
	}

	return nil
}

// DeleteRoomTag удаляет тег комнаты одним запросом. Отсутствующий тег -
// не ошибка (как Synapse) и не сдвигает позицию.
func (db *DB) DeleteRoomTag(ctx context.Context, userID, roomID, tag string) error {
	const query = `
		UPDATE account_data
		SET content = jsonb_build_object('tags', (content->'tags') - $4::text),
		    updated_at = now(), stream_ordering = $5
		WHERE user_id = $1 AND room_id = $2 AND type = $3
		  AND jsonb_typeof(content->'tags') = 'object' AND (content->'tags') ? $4::text`

	if _, err := db.writeAccountDataAt(ctx, query, userID, roomID, accountDataTag, tag); err != nil {
		return fmt.Errorf("store: delete room tag: %w", err)
	}

	return nil
}
