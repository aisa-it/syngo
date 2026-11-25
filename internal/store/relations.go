package store

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// Relations отдаёт события комнаты, относящиеся к eventID через m.relates_to:
// relType фильтрует m.relates_to.rel_type (пусто — любой), eventType —
// тип события (пусто — любой). Удаленные события связями не считаются: redaction
// снимает m.relates_to, а исходный content в БД остается, поэтому без
// фильтра удаленная реакция или ответ в треде продолжали отдаваться.
//
// dir задаёт направление пагинации как в спеке: "b" (по умолчанию) —
// назад от from, самые новые первыми, `stream_ordering < from ORDER BY DESC`;
// "f" — вперёд, `stream_ordering > from ORDER BY ASC`. Курсор эксклюзивный,
// иначе первое событие предыдущей страницы приходит повторно.
func (s *RoomStore) Relations(
	ctx context.Context, roomID, eventID, relType, eventType string, from int64, dir string, limit int,
) ([]core.Event, error) {
	if limit <= 0 {
		limit = 10
	}

	// 13 колонок: scanEventRows ждёт ещё redacted_by последним. Раньше здесь было
	// 12 — любой непустой результат /relations падал на Scan.
	// Направление в SQL, а не в Go: иначе пришлось бы тянуть всю ленту связей.
	// from=0 означает «с края»: для dir=b — с самых новых, для f — с самых старых.
	forward := dir == "f"

	comparison := "<"
	order := "DESC"

	if forward {
		comparison = ">"
		order = "ASC"
	}

	bound := from
	if bound == 0 {
		// Сентинел вместо NULL: -1 меньше любого stream_ordering (dir=b берёт всё),
		// для f ограничение сверху снимаем отдельным условием.
		if forward {
			bound = -1
		} else {
			bound = 1 << 62
		}
	}

	query := fmt.Sprintf(`
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(e.redacted_by, '')
		FROM events e
		WHERE room_id = $1
		  AND content->'m.relates_to'->>'event_id' = $2
		  AND ($3 = '' OR content->'m.relates_to'->>'rel_type' = $3)
		  AND ($4 = '' OR type = $4)
		  AND e.redacted_by IS NULL
		  AND e.stream_ordering %s $5
		ORDER BY stream_ordering %s
		LIMIT $6`, comparison, order)

	rows, err := s.db.pool.Query(ctx, query, roomID, eventID, relType, eventType, bound, limit)
	if err != nil {
		return nil, fmt.Errorf("store: relations %s/%s: %w", roomID, eventID, err)
	}

	return scanEventRows(rows)
}
