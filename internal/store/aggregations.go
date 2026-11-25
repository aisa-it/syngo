package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/aisa-it/syngo/internal/core"
)

// Серверные агрегации связей (bundled aggregations): unsigned.m.relations
// у событий, которые сервер отдает клиенту. Считаются одним запросом на пачку
// событий, а не запросом на событие: /sync отдает таймлайны всех комнат сразу, и
// число SQL-запросов не должно расти с числом событий (ADR-0005).

// ThreadSummary — сводка треда для m.thread (тип домена).
type ThreadSummary = core.ThreadSummary

// Aggregations — агрегации пачки событий по id родителя (тип домена).
type Aggregations = core.Aggregations

var _ core.AggregationStore = (*DB)(nil)

// BundledAggregations собирает агрегации для parents одним запросом:
// сводки тредов, правки и ссылки, а последние ответы и правки - сразу целиком.
//
// Связь учитывается, если она не удалена (redaction снимает m.relates_to, а в
// старых записях content мог остаться) и видна пользователю по
// history_visibility: vis — границы видимости по комнатам, комнаты без записи
// видны целиком. Правка действует, только если у нее тот же отправитель, тип и
// комната, что у исходного события, и ни одно из двух не событие состояния
// (спека, «Validity of replacement events»); у незашифрованной правки должен быть
// m.new_content. Из нескольких правок берется последняя по origin_server_ts, при
// равенстве — с большим event_id, как в спеке.
func (db *DB) BundledAggregations(
	ctx context.Context, userID string, parents []string, vis map[string]Visibility,
) (Aggregations, error) {
	out := Aggregations{
		Threads:    map[string]ThreadSummary{},
		Edits:      map[string]string{},
		References: map[string][]string{},
		Events:     map[string]core.Event{},
	}

	if len(parents) == 0 {
		return out, nil
	}

	rooms, before, from := visibilityArrays(vis)

	// content ? 'm.relates_to' — предикат частичного индекса events_relates_to_idx
	// (0023): без него планировщик индекс не выбирает.
	const query = `
		WITH vis AS (
			SELECT * FROM unnest($3::text[], $4::bigint[], $5::bigint[]) AS v(room_id, before_pos, from_pos)
		), rel AS (
			SELECT c.event_id, c.sender, c.type, c.stream_ordering, c.origin_server_ts,
			       c.content->'m.relates_to'->>'rel_type' AS rel_type,
			       (c.type = 'm.room.encrypted' OR c.content ? 'm.new_content') AS has_new_content,
			       p.event_id AS parent, p.sender AS parent_sender, p.type AS parent_type,
			       p.state_key IS NULL AS parent_message
			FROM events c
			JOIN events p ON p.event_id = c.content->'m.relates_to'->>'event_id' AND p.room_id = c.room_id
			LEFT JOIN vis v ON v.room_id = c.room_id
			WHERE c.content ? 'm.relates_to'
			  AND c.content->'m.relates_to'->>'event_id' = ANY($1)
			  AND c.content->'m.relates_to'->>'rel_type' IN ('m.thread', 'm.replace', 'm.reference')
			  AND c.redacted_by IS NULL
			  AND c.state_key IS NULL
			  AND (v.room_id IS NULL OR c.stream_ordering < v.before_pos OR c.stream_ordering >= v.from_pos)
		), agg AS (
			SELECT 'm.thread' AS kind, parent, (array_agg(event_id ORDER BY stream_ordering DESC))[1] AS child,
			       count(*) AS n, bool_or(sender = $2) AS participated
			FROM rel WHERE rel_type = 'm.thread'
			GROUP BY parent
			UNION ALL
			SELECT * FROM (
				SELECT DISTINCT ON (parent) 'm.replace', parent, event_id, 0::bigint, false
				FROM rel
				WHERE rel_type = 'm.replace' AND parent_message AND has_new_content
				  AND sender = parent_sender AND type = parent_type
				ORDER BY parent, origin_server_ts DESC, event_id DESC
			) edits
			UNION ALL
			SELECT 'm.reference', parent, event_id, stream_ordering, false
			FROM rel WHERE rel_type = 'm.reference'
		)
		SELECT a.kind, a.parent, a.child, a.n, a.participated,
		       coalesce(e.room_id, ''), coalesce(e.type, ''), coalesce(e.sender, ''), e.content,
		       coalesce(e.origin_server_ts, 0), coalesce(e.stream_ordering, 0)
		FROM agg a
		LEFT JOIN events e ON e.event_id = a.child AND a.kind <> 'm.reference'`

	rows, err := db.pool.Query(ctx, query, parents, userID, rooms, before, from)
	if err != nil {
		return out, fmt.Errorf("store: bundled aggregations: %w", err)
	}

	defer rows.Close()

	type reference struct {
		eventID string
		pos     int64
	}

	refs := map[string][]reference{}

	for rows.Next() {
		var (
			kind, parent, eventID string
			n                     int64
			participated          bool
			child                 core.Event
		)

		if err := rows.Scan(&kind, &parent, &eventID, &n, &participated,
			&child.RoomID, &child.Type, &child.Sender, &child.Content, &child.OriginServerTS, &child.StreamOrdering,
		); err != nil {
			return out, fmt.Errorf("store: scan bundled aggregations: %w", err)
		}

		if child.Type != "" {
			child.EventID = eventID
			out.Events[eventID] = child
		}

		switch kind {
		case "m.thread":
			out.Threads[parent] = ThreadSummary{Count: n, LatestEventID: eventID, Participated: participated}
		case "m.replace":
			out.Edits[parent] = eventID
		case "m.reference":
			refs[parent] = append(refs[parent], reference{eventID: eventID, pos: n})
		}
	}

	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: iterate bundled aggregations: %w", err)
	}

	for parent, list := range refs {
		sort.Slice(list, func(i, j int) bool { return list[i].pos < list[j].pos })

		ids := make([]string, len(list))
		for i := range list {
			ids[i] = list[i].eventID
		}

		out.References[parent] = ids
	}

	return out, nil
}

// visibilityArrays раскладывает границы видимости в параллельные массивы для unnest.
func visibilityArrays(vis map[string]Visibility) (rooms []string, before, from []int64) {
	rooms = make([]string, 0, len(vis))
	before = make([]int64, 0, len(vis))
	from = make([]int64, 0, len(vis))

	for roomID, v := range vis {
		rooms = append(rooms, roomID)
		before = append(before, v.Before)
		from = append(from, v.From)
	}

	return rooms, before, from
}

// Threads отдает корни тредов комнаты, самые недавно обновленные первыми,
// и позицию последнего ответа каждого треда — курсор следующей
// страницы. Удаленные ответы и ответы из скрытого по history_visibility
// промежутка не учитываются; корень должен быть виден. Выборка идет по
// частичному индексу events_thread_replies_idx (0038).
func (s *RoomStore) Threads(ctx context.Context, q core.ThreadsQuery) ([]core.Event, []int64, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}

	cursor := q.From
	if cursor <= 0 {
		cursor = 1 << 62
	}

	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events,
		       e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by, ''), t.latest
		FROM (
			SELECT r.event_id AS root, max(c.stream_ordering) AS latest
			FROM events c
			JOIN events r ON r.event_id = c.content->'m.relates_to'->>'event_id' AND r.room_id = c.room_id
			WHERE c.room_id = $1
			  AND c.content->'m.relates_to'->>'rel_type' = 'm.thread'
			  AND c.redacted_by IS NULL
			  AND c.state_key IS NULL
			  AND r.state_key IS NULL
			  AND (c.stream_ordering < $2 OR c.stream_ordering >= $3)
			  AND (r.stream_ordering < $2 OR r.stream_ordering >= $3)
			GROUP BY r.event_id, r.sender
			HAVING max(c.stream_ordering) < $4
			   AND (NOT $5 OR r.sender = $6 OR bool_or(c.sender = $6))
			ORDER BY latest DESC
			LIMIT $7
		) t
		JOIN events e ON e.event_id = t.root
		ORDER BY t.latest DESC`

	rows, err := s.db.pool.Query(ctx, query, q.RoomID, q.VisibleBefore, q.VisibleFrom,
		cursor, q.Participated, q.UserID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("store: threads %s: %w", q.RoomID, err)
	}

	defer rows.Close()

	var (
		events []core.Event
		latest []int64
	)

	for rows.Next() {
		var (
			event core.Event
			pos   int64
		)

		if err := rows.Scan(
			&event.EventID, &event.RoomID, &event.Type, &event.Sender, &event.StateKey, &event.Content,
			&event.PrevEvents, &event.AuthEvents, &event.Depth, &event.OriginServerTS,
			&event.Origin, &event.StreamOrdering, &event.RedactedBy, &pos,
		); err != nil {
			return nil, nil, fmt.Errorf("store: scan threads: %w", err)
		}

		events = append(events, event)
		latest = append(latest, pos)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate threads: %w", err)
	}

	return events, latest, nil
}
