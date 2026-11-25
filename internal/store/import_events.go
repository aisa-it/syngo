package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// ExcludedEventRow - событие источника, которое не входит в timeline и
// состояние: отклоненное, soft-failed или outlier.
type ExcludedEventRow struct {
	EventID        string
	RoomID         string
	Type           string
	Sender         string
	StateKey       *string
	Reason         string // rejected | soft_failed | outlier
	RejectedReason *string
	Depth          int64
	OriginServerTS int64
	StreamOrdering int64 // 0 - нет
	PDUJSON        string
}

// InsertExcludedEvents пишет события в excluded_events (идемпотентно по event_id).
// Событие, которое уже лежит в events (например, после импорта старым
// инструментом), не трогается: снять его из timeline - решение оператора.
func (db *DB) InsertExcludedEvents(ctx context.Context, rows []ExcludedEventRow) (int64, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	const query = `
		INSERT INTO excluded_events (event_id, room_id, type, sender, state_key, reason, rejected_reason,
		                             depth, origin_server_ts, stream_ordering, pdu_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (event_id) DO NOTHING`

	batch := &pgx.Batch{}

	for i := range rows {
		row := &rows[i]

		var stream any
		if row.StreamOrdering != 0 {
			stream = row.StreamOrdering
		}

		batch.Queue(query, row.EventID, row.RoomID, row.Type, row.Sender, row.StateKey, row.Reason,
			row.RejectedReason, row.Depth, row.OriginServerTS, stream, nullText(row.PDUJSON))
	}

	results := db.pool.SendBatch(ctx, batch)

	var inserted int64

	for range rows {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()

			return inserted, fmt.Errorf("store: insert excluded events: %w", err)
		}

		inserted += tag.RowsAffected()
	}

	if err := results.Close(); err != nil {
		return inserted, fmt.Errorf("store: close excluded batch: %w", err)
	}

	return inserted, nil
}

// ReplaceRoomState заменяет room_state комнаты текущим состоянием источника
// (current_state_events Synapse) - вместо эвристики "последнее по
// stream_ordering", которая ошибается на форках и отклоненных событиях.
// Строки, чье событие не импортировано в events этой комнаты,
// не пишутся и возвращаются для отчета.
func (db *DB) ReplaceRoomState(ctx context.Context, roomID string, refs []core.StateRef) ([]core.StateRef, error) {
	if db == nil || db.pool == nil {
		return nil, ErrNotConfigured
	}

	types := make([]string, len(refs))
	keys := make([]string, len(refs))
	ids := make([]string, len(refs))

	for i := range refs {
		types[i], keys[i], ids[i] = refs[i].Type, refs[i].StateKey, refs[i].EventID
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin replace state: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM room_state WHERE room_id = $1`, roomID); err != nil {
		return nil, fmt.Errorf("store: clear room_state %s: %w", roomID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT $1, s.type, s.state_key, s.event_id
		FROM unnest($2::text[], $3::text[], $4::text[]) AS s(type, state_key, event_id)
		JOIN events e ON e.event_id = s.event_id AND e.room_id = $1
		ON CONFLICT (room_id, type, state_key) DO UPDATE SET event_id = EXCLUDED.event_id, updated_at = now()`,
		roomID, types, keys, ids); err != nil {
		return nil, fmt.Errorf("store: write room_state %s: %w", roomID, err)
	}

	rows, err := tx.Query(ctx, `
		SELECT s.type, s.state_key, s.event_id
		FROM unnest($2::text[], $3::text[], $4::text[]) AS s(type, state_key, event_id)
		WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.event_id = s.event_id AND e.room_id = $1)`,
		roomID, types, keys, ids)
	if err != nil {
		return nil, fmt.Errorf("store: missing state events %s: %w", roomID, err)
	}

	missing, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.StateRef, error) {
		ref := core.StateRef{RoomID: roomID}
		err := row.Scan(&ref.Type, &ref.StateKey, &ref.EventID)

		return ref, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: missing state events %s: %w", roomID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit room_state %s: %w", roomID, err)
	}

	return missing, nil
}

// ApplyPendingRedactions применяет записанные импортом redaction:
// цель помечается redacted_by, ее content и pdu_json вырезаются по правилам
// версии комнаты, оригинал уходит в redactions.original_content (если задан
// срок хранения, SetRedactionRetention). Правило применения - то же, что для
// пришедших по федерации (applyRedactionTx). redaction без цели остается
// ожидать ее; неприменимая (нет прав) помечается обработанной. Возвращает
// число удаленных целей.
func (db *DB) ApplyPendingRedactions(ctx context.Context) (int64, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	var total int64

	for {
		n, processed, err := db.applyRedactionsBatch(ctx, 500)
		if err != nil {
			return total, err
		}

		total += n

		if processed == 0 {
			return total, nil
		}
	}
}

func (db *DB) applyRedactionsBatch(ctx context.Context, limit int) (applied, processed int64, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin redactions: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT r.event_id, r.room_id, r.redacts, e.sender
		FROM redactions r
		JOIN events e ON e.event_id = r.event_id
		JOIN events t ON t.event_id = r.redacts AND t.room_id = r.room_id
		WHERE NOT r.pruned
		ORDER BY e.stream_ordering
		LIMIT $1
		FOR UPDATE OF r`, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("store: pending redactions: %w", err)
	}

	type item struct{ id, room, target, sender string }

	var items []item

	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.room, &it.target, &it.sender); err != nil {
			rows.Close()

			return 0, 0, fmt.Errorf("store: scan pending redaction: %w", err)
		}

		items = append(items, it)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: pending redactions: %w", err)
	}

	for _, it := range items {
		if err := lockRedactionKey(ctx, tx, it.target); err != nil {
			return 0, 0, err
		}

		var before string

		if err := tx.QueryRow(ctx, `SELECT coalesce(redacted_by, '') FROM events WHERE event_id = $1`, it.target).
			Scan(&before); err != nil {
			return 0, 0, fmt.Errorf("store: redaction target %s: %w", it.target, err)
		}

		ok, err := db.applyRedactionTx(ctx, tx, it.room, it.target, it.id, it.sender)
		if err != nil {
			return 0, 0, err
		}

		if ok && before == "" {
			applied++
		}

		// Обработана: применена, цель уже удалена другой redaction или прав нет.
		if _, err := tx.Exec(ctx, `UPDATE redactions SET pruned = true WHERE event_id = $1`, it.id); err != nil {
			return 0, 0, fmt.Errorf("store: mark redaction %s: %w", it.id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit redactions: %w", err)
	}

	return applied, int64(len(items)), nil
}

// RoomVersions - версии всех комнат (для проверки event_id при импорте).
func (db *DB) RoomVersions(ctx context.Context) (map[string]string, error) {
	if db == nil || db.pool == nil {
		return nil, ErrNotConfigured
	}

	rows, err := db.pool.Query(ctx, `SELECT room_id, room_version FROM rooms`)
	if err != nil {
		return nil, fmt.Errorf("store: room versions: %w", err)
	}

	defer rows.Close()

	out := make(map[string]string, 1024)

	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("store: scan room version: %w", err)
		}

		out[id] = version
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: room versions: %w", err)
	}

	return out, nil
}

// prunePDUContent подменяет content в исходном PDU вырезанным: иначе удаленный
// текст оставался бы в pdu_json. Неразбираемый PDU стирается.
func prunePDUContent(pdu *string, pruned []byte) any {
	if pdu == nil {
		return nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*pdu), &object); err != nil {
		return nil
	}

	object["content"] = pruned

	// redacted_because и прочие unsigned-поля клиента в PDU не нужны.
	delete(object, "unsigned")

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(object); err != nil {
		return nil
	}

	return strings.TrimRight(buf.String(), "\n")
}
