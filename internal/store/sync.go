package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// SyncStore — батч-реализация core.SyncDataStore (ADR-0005 §5).
//
// Каждый метод — ровно один SQL-запрос: число запросов на /sync не зависит от числа
// комнат пользователя. Это проверяется тестом с трейсером (см. sync_integration_test.go).
type SyncStore struct {
	db *DB
}

var (
	_ core.SyncDataStore   = (*SyncStore)(nil)
	_ core.MembershipStore = (*SyncStore)(nil)
	_ core.SyncStateStore  = (*SyncStore)(nil)
)

// NewSyncStore создаёт хранилище данных /sync.
func NewSyncStore(db *DB) *SyncStore {
	return &SyncStore{db: db}
}

// MembershipRooms отдаёт комнаты пользователя с типом членства (один запрос).
func (s *SyncStore) MembershipRooms(ctx context.Context, userID string) ([]core.RoomMembership, error) {
	// Позиция события членства нужна, чтобы для leave/ban отдать события только
	// до выхода и один раз: иначе вышедший или забаненный бессрочно получает
	// переписку комнаты. ban отдаётся клиенту как leave.
	//
	// Позиция - скалярным подзапросом, а не LEFT JOIN: с JOIN планировщик
	// выбирал параллельный Seq Scan по room_memberships (60 мс у пользователя с
	// 1000 комнат против 30 мс по индексу user_id).
	const query = `
		SELECT rm.room_id, rm.membership,
		       coalesce((SELECT e.stream_ordering FROM events e WHERE e.event_id = rm.event_id), 0),
		       rm.forgotten, rm.sender
		FROM room_memberships rm
		WHERE rm.user_id = $1 AND rm.membership IN ('join', 'invite', 'leave', 'ban')
		ORDER BY rm.room_id`

	rows, err := s.db.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("store: sync membership rooms: %w", err)
	}

	defer rows.Close()

	var memberships []core.RoomMembership

	for rows.Next() {
		var membership core.RoomMembership
		if err := rows.Scan(&membership.RoomID, &membership.Membership, &membership.LeaveStream, &membership.Forgotten,
			&membership.Sender); err != nil {
			return nil, fmt.Errorf("store: scan membership: %w", err)
		}

		memberships = append(memberships, membership)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate memberships: %w", err)
	}

	return memberships, nil
}

// JoinedRoomIDs - комнаты с membership=join по room_id (GET /joined_rooms;
// SQL перенесен из обработчика).
func (s *SyncStore) JoinedRoomIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.pool.Query(ctx, `
		SELECT room_id FROM room_memberships
		WHERE user_id = $1 AND membership = 'join'
		ORDER BY room_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: joined rooms: %w", err)
	}

	defer rows.Close()

	rooms := make([]string, 0, 8)

	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			return nil, fmt.Errorf("store: scan joined room: %w", err)
		}

		rooms = append(rooms, roomID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate joined rooms: %w", err)
	}

	return rooms, nil
}

// EventsSince отдает события по списку комнат после позиции since: один запрос,
// не больше perRoom ПОСЛЕДНИХ событий на комнату (по возрастанию).
// Нумерация - от новых к старым: раньше брались самые старые perRoom событий, и при
// длинной истории initial sync отдавал начало комнаты, а свежие сообщения клиент
// не получал никогда (найдено проверкой в Element).
func (s *SyncStore) EventsSince(
	ctx context.Context,
	roomIDs []string,
	since int64,
	perRoom int,
	upper map[string]int64,
) (map[string][]core.Event, error) {
	if len(roomIDs) == 0 {
		return map[string][]core.Event{}, nil
	}

	// Верхние границы передаём двумя массивами и джойним через unnest: так
	// ограничение leave/ban живёт в SQL, а не только в хендлере.
	limitRooms := make([]string, 0, len(upper))
	limitStreams := make([]int64, 0, len(upper))

	for roomID, bound := range upper {
		limitRooms = append(limitRooms, roomID)
		limitStreams = append(limitStreams, bound)
	}

	// LATERAL с LIMIT по индексу (room_id, stream_ordering): на комнату читается не
	// больше perRoom строк. Раньше row_number() по всей истории комнат: initial sync
	// пользователя с 1000 комнат на 1 млн событий - Seq Scan, внешняя сортировка
	// и 2,5 с ради 21 тыс. строк; теперь 0,15 с.
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM unnest($1::text[]) AS r(room_id)
		LEFT JOIN unnest($4::text[], $5::bigint[]) AS lr(room_id, upper_stream)
		       ON lr.room_id = r.room_id
		CROSS JOIN LATERAL (
			SELECT * FROM events ev
			WHERE ev.room_id = r.room_id AND ev.stream_ordering > $2
			  AND ev.stream_ordering <= coalesce(lr.upper_stream, 9223372036854775807)
			  AND NOT ev.outlier
			ORDER BY ev.stream_ordering DESC
			LIMIT $3
		) e
		ORDER BY e.stream_ordering`

	rows, err := s.db.pool.Query(ctx, query, roomIDs, since, perRoom, limitRooms, limitStreams)
	if err != nil {
		return nil, fmt.Errorf("store: sync events since %d: %w", since, err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	byRoom := make(map[string][]core.Event, len(roomIDs))
	for i := range events {
		event := events[i]
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	return byRoom, nil
}

// StateForRooms отдаёт материализованное состояние комнат (один запрос).
func (s *SyncStore) StateForRooms(ctx context.Context, roomIDs []string) (map[string][]core.Event, error) {
	if len(roomIDs) == 0 {
		return map[string][]core.Event{}, nil
	}

	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = ANY($1)
		ORDER BY e.room_id, e.type, e.state_key`

	rows, err := s.db.pool.Query(ctx, query, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: sync state for rooms: %w", err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	byRoom := make(map[string][]core.Event, len(roomIDs))
	for i := range events {
		event := events[i]
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	return byRoom, nil
}

// StateDeltaForRooms отдает события текущего состояния комнат, записанные после
// since, и m.room.member пользователей members (один запрос). Первая
// часть идет по частичному индексу state-событий (room_id, stream_ordering)
// (миграция 0047: и в generic-плане pgx читаются только события после
// since), вторая - по room_state; полный снапшот комнаты не читается.
func (s *SyncStore) StateDeltaForRooms(
	ctx context.Context, roomIDs []string, since int64, members []string,
) (map[string][]core.Event, error) {
	if len(roomIDs) == 0 {
		return map[string][]core.Event{}, nil
	}

	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM events e
		WHERE e.event_id IN (
			SELECT rs.event_id
			FROM events changed
			JOIN room_state rs ON rs.event_id = changed.event_id
			WHERE changed.room_id = ANY($1) AND changed.stream_ordering > $2
			  AND changed.state_key IS NOT NULL
			UNION
			SELECT rs.event_id
			FROM room_state rs
			WHERE rs.room_id = ANY($1) AND rs.type = 'm.room.member' AND rs.state_key = ANY($3)
		)
		ORDER BY e.room_id, e.type, e.state_key`

	rows, err := s.db.pool.Query(ctx, query, roomIDs, since, members)
	if err != nil {
		return nil, fmt.Errorf("store: sync state delta: %w", err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	byRoom := make(map[string][]core.Event, len(roomIDs))
	for i := range events {
		event := events[i]
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	return byRoom, nil
}

// StateMembersForRooms отдает текущее состояние комнат без m.room.member, кроме
// членства members (один запрос): initial sync с lazy_load_members не
// читает и не разбирает членство всех участников. Список - подзапросом (hashed
// SubPlan): `= ANY($2)` в фильтре generic-плана, который pgx начинает
// использовать с шестого вызова, сравнивает каждую строку со всем массивом
// линейно (25 с на 1000 комнат по 50 участников).
func (s *SyncStore) StateMembersForRooms(
	ctx context.Context, roomIDs, members []string,
) (map[string][]core.Event, error) {
	if len(roomIDs) == 0 {
		return map[string][]core.Event{}, nil
	}

	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = ANY($1)
		  AND (rs.type <> 'm.room.member' OR rs.state_key IN (SELECT unnest($2::text[])))
		ORDER BY e.room_id, e.type, e.state_key`

	rows, err := s.db.pool.Query(ctx, query, roomIDs, members)
	if err != nil {
		return nil, fmt.Errorf("store: sync lazy state: %w", err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	byRoom := make(map[string][]core.Event, len(roomIDs))
	for i := range events {
		event := events[i]
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	return byRoom, nil
}

// StreamPosition возвращает текущую максимальную позицию stream_ordering (для next_batch).
func (s *SyncStore) StreamPosition(ctx context.Context) (int64, error) {
	const query = `SELECT coalesce(max(stream_ordering), 0) FROM events`

	var position int64
	if err := s.db.pool.QueryRow(ctx, query).Scan(&position); err != nil {
		return 0, fmt.Errorf("store: stream position: %w", err)
	}

	return position, nil
}

// ErrNotImplemented оставлен для явных заглушек в слое данных (account_data, to_device).
var ErrNotImplemented = errors.New("store: not implemented")

// StateAtRoomsStream отдаёт состояние комнат на их позиции stream_ordering одним
// запросом (контракт ADR-0005: фиксированное число запросов на /sync). Для каждого
// (room_id, type, state_key) берётся последнее state-событие не позже pos комнаты.
func (s *SyncStore) StateAtRoomsStream(
	ctx context.Context, positions map[string]int64,
) (map[string][]core.Event, error) {
	return s.StateAtRoomsStreamMembers(ctx, positions, nil)
}

// StateAtRoomsStreamMembers - StateAtRoomsStream, где m.room.member берется
// только для members (nil - все): lazy_load_members в initial sync.
func (s *SyncStore) StateAtRoomsStreamMembers(
	ctx context.Context, positions map[string]int64, members []string,
) (map[string][]core.Event, error) {
	if len(positions) == 0 {
		return map[string][]core.Event{}, nil
	}

	roomIDs := make([]string, 0, len(positions))
	streams := make([]int64, 0, len(positions))

	for roomID, pos := range positions {
		roomIDs = append(roomIDs, roomID)
		streams = append(streams, pos)
	}

	const query = `
		SELECT DISTINCT ON (e.room_id, e.type, e.state_key)
		       e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM unnest($1::text[], $2::bigint[]) AS p(room_id, pos)
		JOIN events e ON e.room_id = p.room_id
		             AND e.state_key IS NOT NULL
		             AND e.stream_ordering <= p.pos
		WHERE $3::text[] IS NULL OR e.type <> 'm.room.member'
		   OR e.state_key IN (SELECT unnest($3::text[]))
		ORDER BY e.room_id, e.type, e.state_key, e.stream_ordering DESC`

	rows, err := s.db.pool.Query(ctx, query, roomIDs, streams, members)
	if err != nil {
		return nil, fmt.Errorf("store: state at stream: %w", err)
	}

	defer rows.Close()

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	byRoom := make(map[string][]core.Event, len(positions))
	for i := range events {
		event := events[i]
		byRoom[event.RoomID] = append(byRoom[event.RoomID], event)
	}

	return byRoom, nil
}

// RoomMembershipsFor отдаёт членство пользователя в перечисленных комнатах
// (roomID → membership). Нужен для restricted join: allow-список join_rules
// ссылается на другие комнаты, состояние которых в auth.State не приходит.
func (s *SyncStore) RoomMembershipsFor(
	ctx context.Context, userID string, roomIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(roomIDs))

	if len(roomIDs) == 0 {
		return out, nil
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT room_id, membership FROM room_memberships
		WHERE user_id = $1 AND room_id = ANY($2)`, userID, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: memberships of %s: %w", userID, err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID     string
			membership string
		)

		if err := rows.Scan(&roomID, &membership); err != nil {
			return nil, fmt.Errorf("store: scan membership of %s: %w", userID, err)
		}

		out[roomID] = membership
	}

	return out, rows.Err()
}

// RoomSummary - сводка комнаты для summary в /sync (core.RoomSummary).
type RoomSummary = core.RoomSummary

// RoomSummaries - сводки комнат одним запросом: число участников с
// join и invite и до пяти героев (участники, кроме userID, по user_id) - по ним
// клиент с lazy_load_members называет комнату без имени.
func (s *SyncStore) RoomSummaries(ctx context.Context, userID string, roomIDs []string) (map[string]RoomSummary, error) {
	out := make(map[string]RoomSummary, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	const query = `
		SELECT room_id,
		       count(*) FILTER (WHERE membership = 'join'),
		       count(*) FILTER (WHERE membership = 'invite'),
		       coalesce((array_agg(user_id ORDER BY user_id) FILTER (WHERE user_id <> $2))[1:5], '{}')
		FROM room_memberships
		WHERE room_id = ANY($1::text[]) AND membership IN ('join', 'invite')
		GROUP BY room_id`

	rows, err := s.db.pool.Query(ctx, query, roomIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("store: room summaries: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID  string
			summary RoomSummary
		)

		if err := rows.Scan(&roomID, &summary.Joined, &summary.Invited, &summary.Heroes); err != nil {
			return nil, fmt.Errorf("store: scan room summary: %w", err)
		}

		out[roomID] = summary
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: room summaries rows: %w", err)
	}

	return out, nil
}
