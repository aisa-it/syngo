package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/state"
)

// Доступ сервера к состоянию и истории комнаты.
//
// Чтение по федерации (/event, /state, /state_ids, /backfill,
// /get_missing_events, /event_auth) разрешено только серверу с участником
// комнаты (is_host_in_room в Synapse); события, которые сервер не мог видеть
// по history_visibility, отдаются отредактированными (filter_events_for_server).
//
// Состояние «на событии» - записанное состояние после его prev_events или их
// разрешение; для событий без записанного состояния (до миграции
// 0044) - по depth: для каждого ключа state-событие комнаты с наибольшей
// глубиной меньше глубины события.

// HostInRoom сообщает, есть ли у сервера host участник с membership=join в
// текущем состоянии комнаты.
func (db *DB) HostInRoom(ctx context.Context, roomID, host string) (bool, error) {
	var in bool

	err := db.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM room_state rs
			JOIN events e ON e.event_id = rs.event_id
			WHERE rs.room_id = $1 AND rs.type = 'm.room.member'
			  AND strpos(rs.state_key, ':') > 0
			  AND substr(rs.state_key, strpos(rs.state_key, ':') + 1) = $2
			  AND e.content->>'membership' = 'join')`, roomID, host).Scan(&in)
	if err != nil {
		return false, fmt.Errorf("store: host in room %s: %w", roomID, err)
	}

	return in, nil
}

// JoinedHosts - серверы с joined-участниками по текущему состоянию комнаты
// (servers_in_room ответа send_join), по алфавиту.
func (db *DB) JoinedHosts(ctx context.Context, roomID string) ([]string, error) {
	rows, err := db.pool.Query(ctx, joinedHostsQuery, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: joined hosts %s: %w", roomID, err)
	}

	defer rows.Close()

	hosts := []string{}

	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, fmt.Errorf("store: scan joined host: %w", err)
		}

		hosts = append(hosts, host)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate joined hosts: %w", err)
	}

	slices.Sort(hosts)

	return hosts, nil
}

// StateIDsBefore - состояние комнаты до события eventID (как /state_ids в
// Synapse: само событие в ответ не входит). Нет события в комнате - found=false.
//
// Состояние до события - записанное состояние после его prev_events:
// один prev - его состояние, несколько - разрешение их наборов (state
// resolution v2). prev без записанного состояния (события до миграции 0044) -
// приближение по depth: последнее state-событие ключа глубже события.
func (db *DB) StateIDsBefore(ctx context.Context, roomID, eventID string) (ids []string, found bool, err error) {
	var (
		depth int64
		prev  []string
	)

	err = db.pool.QueryRow(ctx,
		`SELECT depth, prev_events FROM events WHERE event_id = $1 AND room_id = $2`, eventID, roomID).Scan(&depth, &prev)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("store: state before %s: %w", eventID, err)
	}

	before, ok, err := db.stateBeforeByGroups(ctx, roomID, prev)
	if err != nil {
		return nil, false, err
	}

	if ok {
		ids = make([]string, 0, len(before))

		keys := make([]state.Key, 0, len(before))
		for key := range before {
			keys = append(keys, key)
		}

		slices.SortFunc(keys, func(a, b state.Key) int {
			if c := strings.Compare(a.Type, b.Type); c != 0 {
				return c
			}

			return strings.Compare(a.StateKey, b.StateKey)
		})

		for _, key := range keys {
			ids = append(ids, before[key])
		}

		return ids, true, nil
	}

	return db.stateIDsBeforeByDepth(ctx, roomID, eventID, depth)
}

// stateBeforeByGroups - состояние до события по записанному состоянию после его
// prev_events; false - ни для одного prev состояние не записано.
func (db *DB) stateBeforeByGroups(
	ctx context.Context, roomID string, prev []string,
) (before state.StateMap, found bool, err error) {
	if len(prev) == 0 {
		return nil, false, nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("store: state before begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	roomState := newRoomStateTx(tx, roomID)

	after, err := roomState.StateAfter(ctx, prev)
	if err != nil {
		return nil, false, err
	}

	sets := make([]state.StateMap, 0, len(after))

	for _, id := range prev {
		if st, ok := after[id]; ok {
			sets = append(sets, st)
		}
	}

	switch len(sets) {
	case 0:
		return nil, false, nil
	case 1:
		return sets[0], true, nil
	}

	resolved, err := core.ResolveRoomStateSets(ctx, roomState, roomID, sets)
	if err != nil {
		return nil, false, fmt.Errorf("store: state before: %w", err)
	}

	return resolved, true, nil
}

// stateIDsBeforeByDepth - приближение состояния до события без записанного
// состояния: последнее state-событие ключа с меньшей глубиной.
func (db *DB) stateIDsBeforeByDepth(
	ctx context.Context, roomID, eventID string, depth int64,
) (ids []string, found bool, err error) {
	rows, err := db.pool.Query(ctx, `
		SELECT event_id FROM (
			SELECT DISTINCT ON (type, state_key) event_id, type, state_key
			FROM events
			WHERE room_id = $1 AND state_key IS NOT NULL AND depth < $2
			  AND rejected_reason IS NULL
			ORDER BY type, state_key, depth DESC, stream_ordering DESC
		) s ORDER BY type, state_key`, roomID, depth)
	if err != nil {
		return nil, false, fmt.Errorf("store: state before %s: %w", eventID, err)
	}

	defer rows.Close()

	ids = []string{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("store: scan state before: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: iterate state before: %w", err)
	}

	return ids, true, nil
}

// AuthChainIDs - рекурсивная auth-цепочка событий (без них самих, если они не
// входят в цепочку друг друга), по алфавиту. Ссылки на неизвестные события
// пропускаются; цикл в битых данных не зацикливает запрос (UNION). В комнате v12
// create входит в auth-события каждого события неявно (его id - room_id с «$»,
// auth_event_ids() в Synapse): без него пир не авторизует state и auth_chain
// send_join, /state и /event_auth.
func (db *DB) AuthChainIDs(ctx context.Context, eventIDs []string) ([]string, error) {
	if len(eventIDs) == 0 {
		return []string{}, nil
	}

	rows, err := db.pool.Query(ctx, `
		WITH RECURSIVE chain(id) AS (
			SELECT a.id FROM events e CROSS JOIN LATERAL unnest(e.auth_events || `+implicitCreateSQL+`) AS a(id)
			WHERE e.event_id = ANY($1)
			UNION
			SELECT a.id FROM chain c
			JOIN events e ON e.event_id = c.id
			CROSS JOIN LATERAL unnest(e.auth_events || `+implicitCreateSQL+`) AS a(id)
		)
		SELECT c.id FROM chain c WHERE EXISTS (SELECT 1 FROM events e WHERE e.event_id = c.id)
		ORDER BY c.id`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("store: auth chain: %w", err)
	}

	defer rows.Close()

	ids := []string{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan auth chain: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// implicitCreateSQL - неявное auth-событие события e комнаты v12: ее create
// (room_id без домена, «!» -> «$»); у самого create и в ранних версиях - пусто.
const implicitCreateSQL = `(CASE WHEN NOT (e.type = 'm.room.create' AND COALESCE(e.state_key, '-') = '') AND e.room_id LIKE '!%' AND strpos(e.room_id, ':') = 0
	THEN ARRAY['$' || substr(e.room_id, 2)] ELSE ARRAY[]::text[] END)`

// DAGEvent - узел DAG комнаты для обхода (backfill, get_missing_events).
type DAGEvent struct {
	EventID        string
	Prev           []string
	Depth          int64
	StreamOrdering int64
}

// DAGEvents возвращает узлы DAG комнаты по id; чужие и неизвестные id
// пропускаются.
func (db *DB) DAGEvents(ctx context.Context, roomID string, eventIDs []string) (map[string]DAGEvent, error) {
	out := make(map[string]DAGEvent, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT event_id, prev_events, depth, stream_ordering FROM events
		WHERE room_id = $1 AND event_id = ANY($2)`, roomID, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("store: dag events: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var node DAGEvent
		if err := rows.Scan(&node.EventID, &node.Prev, &node.Depth, &node.StreamOrdering); err != nil {
			return nil, fmt.Errorf("store: scan dag event: %w", err)
		}

		out[node.EventID] = node
	}

	return out, rows.Err()
}

// EventsVisibleToServer сообщает для каждого события комнаты, мог ли сервер
// host его видеть (filter_events_for_server в Synapse): при history_visibility
// world_readable/shared (и по умолчанию) - да, при joined - если у сервера был
// joined-участник, при invited - joined или invited. Видимость и членство
// берутся на момент события (по depth; членство с учетом самого события).
// Неизвестные id в ответ не попадают.
func (db *DB) EventsVisibleToServer(ctx context.Context, roomID, host string, eventIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT e.event_id,
		       coalesce((
		           SELECT hv.content->>'history_visibility' FROM events hv
		           WHERE hv.room_id = e.room_id AND hv.type = 'm.room.history_visibility'
		             AND hv.state_key = '' AND hv.depth < e.depth AND hv.rejected_reason IS NULL
		           ORDER BY hv.depth DESC, hv.stream_ordering DESC LIMIT 1), 'shared'),
		       coalesce((
		           SELECT array_agg(m.membership) FROM (
		               SELECT DISTINCT ON (mm.state_key) mm.content->>'membership' AS membership
		               FROM events mm
		               WHERE mm.room_id = e.room_id AND mm.type = 'm.room.member'
		                 AND strpos(mm.state_key, ':') > 0
		                 AND substr(mm.state_key, strpos(mm.state_key, ':') + 1) = $3
		                 AND (mm.depth < e.depth OR mm.event_id = e.event_id)
		                 AND mm.rejected_reason IS NULL
		               ORDER BY mm.state_key, (mm.event_id = e.event_id) DESC, mm.depth DESC, mm.stream_ordering DESC
		           ) m), '{}')
		FROM events e WHERE e.room_id = $1 AND e.event_id = ANY($2)`, roomID, eventIDs, host)
	if err != nil {
		return nil, fmt.Errorf("store: events visible to %s: %w", host, err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			id          string
			visibility  string
			memberships []string
		)

		if err := rows.Scan(&id, &visibility, &memberships); err != nil {
			return nil, fmt.Errorf("store: scan visibility: %w", err)
		}

		out[id] = serverMaySee(visibility, memberships)
	}

	return out, rows.Err()
}

// serverMaySee - правило видимости события серверу по history_visibility и
// членству его участников на момент события.
func serverMaySee(visibility string, memberships []string) bool {
	switch visibility {
	case "joined":
		return slices.Contains(memberships, "join")
	case "invited":
		return slices.Contains(memberships, "join") || slices.Contains(memberships, "invite")
	default:
		// world_readable, shared и неизвестные значения - как shared (спека).
		return true
	}
}
