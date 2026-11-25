package store

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

var _ core.ReadMarkerStore = (*DB)(nil)

// UpsertReceipt сохраняет отметку прочтения с позицией события:
// по ней обработчик push-действий не считает уже прочитанное.
func (db *DB) UpsertReceipt(ctx context.Context, r core.ReceiptEvent) error {
	const query = `
		INSERT INTO receipts (room_id, user_id, receipt_type, thread_id, event_id, ts, event_stream)
		VALUES ($1, $2, $3, $4, $5, $6,
		        (SELECT stream_ordering FROM events WHERE event_id = $5 AND room_id = $1))
		ON CONFLICT (room_id, user_id, receipt_type, thread_id)
		DO UPDATE SET event_id = EXCLUDED.event_id, ts = EXCLUDED.ts, event_stream = EXCLUDED.event_stream`

	if _, err := db.pool.Exec(ctx, query, r.RoomID, r.UserID, r.Type, r.ThreadID, r.EventID, r.TS); err != nil {
		return fmt.Errorf("store: upsert receipt: %w", err)
	}

	return nil
}

// AllReceipts возвращает все сохраненные отметки (загрузка при старте процесса).
func (db *DB) AllReceipts(ctx context.Context) ([]core.ReceiptEvent, error) {
	rows, err := db.pool.Query(ctx, `SELECT room_id, user_id, receipt_type, thread_id, event_id, ts FROM receipts`)
	if err != nil {
		return nil, fmt.Errorf("store: receipts: %w", err)
	}

	defer rows.Close()

	var out []core.ReceiptEvent

	for rows.Next() {
		var r core.ReceiptEvent
		if err := rows.Scan(&r.RoomID, &r.UserID, &r.Type, &r.ThreadID, &r.EventID, &r.TS); err != nil {
			return nil, fmt.Errorf("store: scan receipt: %w", err)
		}

		out = append(out, r)
	}

	return out, rows.Err()
}

// ApplyImportedReceipts - после импорта из Synapse: отметкам,
// импортированным раньше своих событий, проставляется позиция события, и сводка
// непрочитанного (event_push_summary), если уже посчитана, поднимается до
// отметок m.read и m.read.private: строки выше позиции пересчитываются, как в
// MarkPushRead. Отметка без треда покрывает все треды, "main" - основную ленту.
// Позиция прочтения только растет; повтор ничего не меняет. Возвращает число
// отметок, получивших позицию.
func (db *DB) ApplyImportedReceipts(ctx context.Context) (int64, error) {
	tag, err := db.pool.Exec(ctx, `
		UPDATE receipts r SET event_stream = e.stream_ordering
		FROM events e
		WHERE r.event_stream IS NULL AND e.event_id = r.event_id AND e.room_id = r.room_id`)
	if err != nil {
		return 0, fmt.Errorf("store: receipt positions: %w", err)
	}

	if _, err := db.pool.Exec(ctx, `
		WITH k AS (
			SELECT s.user_id, s.room_id, s.thread_id, max(r.event_stream) AS upto
			FROM event_push_summary s
			JOIN receipts r ON r.user_id = s.user_id AND r.room_id = s.room_id
			WHERE r.receipt_type IN ('m.read', 'm.read.private') AND r.event_stream IS NOT NULL
			  AND s.notif_count > 0
			  AND (r.thread_id = '' OR r.thread_id = s.thread_id OR (r.thread_id = 'main' AND s.thread_id = ''))
			GROUP BY s.user_id, s.room_id, s.thread_id
		)
		UPDATE event_push_summary s
		SET read_upto = k.upto,
		    (notif_count, highlight_count) = (
		        SELECT count(*), count(*) FILTER (WHERE a.highlight)
		        FROM event_push_actions a
		        WHERE a.user_id = s.user_id AND a.room_id = s.room_id AND a.thread_id = s.thread_id
		          AND a.stream_ordering > k.upto)
		FROM k
		WHERE s.user_id = k.user_id AND s.room_id = k.room_id AND s.thread_id = k.thread_id
		  AND s.read_upto < k.upto`); err != nil {
		return 0, fmt.Errorf("store: imported receipts summary: %w", err)
	}

	return tag.RowsAffected(), nil
}
