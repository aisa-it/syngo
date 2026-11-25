package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
)

var _ core.SearchStore = (*DB)(nil)

// SearchMessages ищет m.room.message по body в указанных комнатах.
// Комнаты передаёт вызывающий — только те, где пользователь состоит, с границами
// видимости по history_visibility: событие вне Visibility не
// находится. Удаленные сообщения не находятся: исходный content (и body_tsv)
// остается в БД после redaction, и поиск находил их по тексту.
// Используется полнотекстовый индекс body_tsv (GIN, конфигурация simple), а не ILIKE.
func (db *DB) SearchMessages(ctx context.Context, rooms map[string]Visibility, term string, limit int) ([]core.Event, error) {
	events, _, err := db.SearchMessagesPage(ctx, rooms, term, core.SearchPage{Limit: limit})

	return events, err
}

// SearchMessagesPage - SearchMessages со смещением, порядком (rank или recent) и
// общим числом совпадений (count в ответе /search). Два запроса: число и
// страница.
func (db *DB) SearchMessagesPage(
	ctx context.Context, rooms map[string]Visibility, term string, page core.SearchPage,
) ([]core.Event, int, error) {
	if len(rooms) == 0 || strings.TrimSpace(term) == "" {
		return nil, 0, nil
	}

	limit := page.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	offset := max(page.Offset, 0)

	roomIDs := make([]string, 0, len(rooms))
	before := make([]int64, 0, len(rooms))
	from := make([]int64, 0, len(rooms))

	for roomID, vis := range rooms {
		roomIDs = append(roomIDs, roomID)
		before = append(before, vis.Before)
		from = append(from, vis.From)
	}

	// websearch_to_tsquery безопасен к пользовательскому вводу (без 500 на кавычках/минусах).
	queryText := strings.TrimSpace(term)

	// Условие видимости то же, что Visibility.Visible: pos < before OR pos >= from.
	const where = `
		FROM events e
		JOIN unnest($1::text[], $3::bigint[], $4::bigint[]) AS v(room_id, before_pos, from_pos)
		  ON v.room_id = e.room_id
		WHERE e.type = 'm.room.message' AND e.redacted_by IS NULL
		  AND (e.stream_ordering < v.before_pos OR e.stream_ordering >= v.from_pos)
		  AND e.body_tsv @@ websearch_to_tsquery('simple', $2)`

	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) `+where, roomIDs, queryText, before, from).Scan(&count); err != nil {
		return nil, 0, fmt.Errorf("store: count search messages: %w", err)
	}

	order := `ts_rank_cd(e.body_tsv, websearch_to_tsquery('simple', $2)) DESC, e.stream_ordering DESC`
	if page.Recent {
		order = `e.stream_ordering DESC`
	}

	query := `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events,
		       e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by,'')` + where + `
		ORDER BY ` + order + `
		LIMIT $5 OFFSET $6`

	rows, err := db.pool.Query(ctx, query, roomIDs, queryText, before, from, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: search messages: %w", err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, 0, err
	}

	return events, count, nil
}
