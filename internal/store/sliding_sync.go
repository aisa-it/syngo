package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Данные simplified sliding sync (MSC4186). Как и у обычного /sync,
// каждый метод - один SQL-запрос на все комнаты: число запросов не зависит от
// числа комнат пользователя (ADR-0005).

var _ core.SlidingSyncStore = (*SyncStore)(nil)

// slidingEventColumns - колонки событий в порядке scanEventRows.
const slidingEventColumns = `e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')`

// LoadSlidingConnection - состояние соединения на позицию. Позиция должна
// принадлежать этому соединению (pos присылает клиент); остальные позиции
// соединения удаляются - клиент подтвердил получение этой.
func (s *SyncStore) LoadSlidingConnection(
	ctx context.Context, userID, deviceID, connID string, position int64,
) (*core.SlidingConnState, error) {
	var (
		raw []byte
		key int64
	)

	err := s.db.pool.QueryRow(ctx, `
		SELECT p.state, c.connection_key
		FROM sliding_sync_connection_positions p
		JOIN sliding_sync_connections c ON c.connection_key = p.connection_key
		WHERE p.connection_position = $1 AND c.user_id = $2 AND c.device_id = $3 AND c.conn_id = $4`,
		position, userID, deviceID, connID).Scan(&raw, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrUnknownPos
	}

	if err != nil {
		return nil, fmt.Errorf("store: load sliding sync connection: %w", err)
	}

	// Отметка использования - не чаще раза в минуту: это не запись на каждый
	// long-poll.
	if _, err := s.db.pool.Exec(ctx, `
		WITH cleared AS (
			DELETE FROM sliding_sync_connection_positions
			WHERE connection_key = $1 AND connection_position <> $2
		)
		UPDATE sliding_sync_connections SET last_used_at = now()
		WHERE connection_key = $1 AND last_used_at < now() - interval '1 minute'`, key, position); err != nil {
		return nil, fmt.Errorf("store: touch sliding sync connection: %w", err)
	}

	state := core.NewSlidingConnState()
	if err := json.Unmarshal(raw, state); err != nil {
		// Нечитаемое состояние - как неизвестная позиция: клиент начнет заново.
		return nil, core.ErrUnknownPos
	}

	return state, nil
}

// SaveSlidingConnection сохраняет состояние новой позицией. prev == 0 - начало
// соединения: прежнее с тем же conn_id сбрасывается, заодно удаляются соединения
// без запросов 7 дней. prev > 0 должен быть позицией этого соединения, иначе
// ErrUnknownPos (соединение начали заново параллельным запросом).
func (s *SyncStore) SaveSlidingConnection(
	ctx context.Context, userID, deviceID, connID string, prev int64, state *core.SlidingConnState,
) (int64, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("store: encode sliding sync state: %w", err)
	}

	var position int64

	if prev > 0 {
		err = s.db.pool.QueryRow(ctx, `
			INSERT INTO sliding_sync_connection_positions (connection_key, state)
			SELECT c.connection_key, $5::jsonb
			FROM sliding_sync_connections c
			JOIN sliding_sync_connection_positions p ON p.connection_key = c.connection_key
			WHERE p.connection_position = $1 AND c.user_id = $2 AND c.device_id = $3 AND c.conn_id = $4
			RETURNING connection_position`, prev, userID, deviceID, connID, raw).Scan(&position)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, core.ErrUnknownPos
		}
	} else {
		// Одним запросом: подзапросы видят один снимок, поэтому удаление старых
		// позиций не трогает новую.
		err = s.db.pool.QueryRow(ctx, `
			WITH expired AS (
				DELETE FROM sliding_sync_connections
				WHERE last_used_at < now() - interval '7 days'
				  AND NOT (user_id = $1 AND device_id = $2 AND conn_id = $3)
			), conn AS (
				INSERT INTO sliding_sync_connections (user_id, device_id, conn_id) VALUES ($1, $2, $3)
				ON CONFLICT (user_id, device_id, conn_id)
				DO UPDATE SET created_at = now(), last_used_at = now()
				RETURNING connection_key
			), cleared AS (
				DELETE FROM sliding_sync_connection_positions p USING conn
				WHERE p.connection_key = conn.connection_key
			)
			INSERT INTO sliding_sync_connection_positions (connection_key, state)
			SELECT connection_key, $4::jsonb FROM conn
			RETURNING connection_position`, userID, deviceID, connID, raw).Scan(&position)
	}

	if err != nil {
		return 0, fmt.Errorf("store: save sliding sync connection: %w", err)
	}

	return position, nil
}

// SlidingRoomActivity - последнее событие и последнее событие из
// SlidingBumpEventTypes по комнатам не позже upper (по индексу (room_id,
// stream_ordering), одна строка на комнату).
func (s *SyncStore) SlidingRoomActivity(
	ctx context.Context, roomIDs []string, upper int64,
) (map[string]core.SlidingActivity, error) {
	out := make(map[string]core.SlidingActivity, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT r.room_id,
		       coalesce((SELECT ev.stream_ordering FROM events ev
		                 WHERE ev.room_id = r.room_id AND ev.stream_ordering <= $2 AND NOT ev.outlier
		                 ORDER BY ev.stream_ordering DESC LIMIT 1), 0),
		       coalesce((SELECT ev.stream_ordering FROM events ev
		                 WHERE ev.room_id = r.room_id AND ev.stream_ordering <= $2 AND NOT ev.outlier
		                   AND ev.type = ANY($3::text[])
		                 ORDER BY ev.stream_ordering DESC LIMIT 1), 0)
		FROM unnest($1::text[]) AS r(room_id)`, roomIDs, upper, core.SlidingBumpEventTypes)
	if err != nil {
		return nil, fmt.Errorf("store: sliding room activity: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID   string
			activity core.SlidingActivity
		)

		if err := rows.Scan(&roomID, &activity.LastEvent, &activity.LastBump); err != nil {
			return nil, fmt.Errorf("store: scan sliding room activity: %w", err)
		}

		out[roomID] = activity
	}

	return out, rows.Err()
}

// SlidingRoomInfo - тип комнаты (m.room.create) и шифрование (m.room.encryption)
// для фильтров списков.
func (s *SyncStore) SlidingRoomInfo(ctx context.Context, roomIDs []string) (map[string]core.SlidingRoomInfo, error) {
	out := make(map[string]core.SlidingRoomInfo, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT rs.room_id, rs.type, e.content
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = ANY($1::text[]) AND rs.state_key = ''
		  AND rs.type IN ('m.room.create', 'm.room.encryption')`, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: sliding room info: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, eventType string
			content           []byte
		)

		if err := rows.Scan(&roomID, &eventType, &content); err != nil {
			return nil, fmt.Errorf("store: scan sliding room info: %w", err)
		}

		info := out[roomID]

		var body struct {
			Type      any `json:"type"`
			Algorithm any `json:"algorithm"`
		}

		_ = json.Unmarshal(content, &body)

		if eventType == "m.room.create" {
			info.Known = true

			if roomType, ok := body.Type.(string); ok {
				info.RoomType = &roomType
			}
		} else if algorithm, ok := body.Algorithm.(string); ok && algorithm != "" {
			info.Encrypted = true
		}

		out[roomID] = info
	}

	return out, rows.Err()
}

// SpaceChildren - дочерние комнаты пространств: m.space.child с непустым via.
func (s *SyncStore) SpaceChildren(ctx context.Context, spaceIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(spaceIDs))
	if len(spaceIDs) == 0 {
		return out, nil
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT rs.room_id, rs.state_key
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = ANY($1::text[]) AND rs.type = 'm.space.child'
		  AND jsonb_typeof(e.content->'via') = 'array' AND jsonb_array_length(e.content->'via') > 0`, spaceIDs)
	if err != nil {
		return nil, fmt.Errorf("store: space children: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var spaceID, child string
		if err := rows.Scan(&spaceID, &child); err != nil {
			return nil, fmt.Errorf("store: scan space child: %w", err)
		}

		out[spaceID] = append(out[spaceID], child)
	}

	return out, rows.Err()
}

// TimelineWindows - последние Limit событий каждой комнаты из (Since, Upper] по
// возрастанию позиции: LATERAL с LIMIT по индексу (room_id, stream_ordering), как
// EventsSince, но с границами и лимитом на комнату.
func (s *SyncStore) TimelineWindows(ctx context.Context, windows []core.TimelineWindow) (map[string][]core.Event, error) {
	if len(windows) == 0 {
		return map[string][]core.Event{}, nil
	}

	rooms := make([]string, 0, len(windows))
	since := make([]int64, 0, len(windows))
	upper := make([]int64, 0, len(windows))
	limits := make([]int32, 0, len(windows))

	for _, w := range windows {
		rooms = append(rooms, w.RoomID)
		since = append(since, w.Since)
		upper = append(upper, w.Upper)
		limits = append(limits, int32(min(max(w.Limit, 0), core.SlidingMaxTimelineLimit+1))) //nolint:gosec // ограничено выше
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT `+slidingEventColumns+`
		FROM unnest($1::text[], $2::bigint[], $3::bigint[], $4::int[]) AS w(room_id, since, upper, lim)
		CROSS JOIN LATERAL (
			SELECT * FROM events ev
			WHERE ev.room_id = w.room_id AND ev.stream_ordering > w.since AND ev.stream_ordering <= w.upper
			  AND NOT ev.outlier
			ORDER BY ev.stream_ordering DESC
			LIMIT w.lim
		) e
		ORDER BY e.stream_ordering`, rooms, since, upper, limits)
	if err != nil {
		return nil, fmt.Errorf("store: timeline windows: %w", err)
	}

	return groupEvents(rows)
}

// StateChangedSince - события текущего состояния комнат, записанные после since
// комнаты: по индексу (room_id, stream_ordering) читаются только новые события,
// полный снимок комнаты не читается (как StateDeltaForRooms).
func (s *SyncStore) StateChangedSince(ctx context.Context, since map[string]int64) (map[string][]core.Event, error) {
	if len(since) == 0 {
		return map[string][]core.Event{}, nil
	}

	rooms := make([]string, 0, len(since))
	positions := make([]int64, 0, len(since))

	for roomID, pos := range since {
		rooms = append(rooms, roomID)
		positions = append(positions, pos)
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT `+slidingEventColumns+`
		FROM unnest($1::text[], $2::bigint[]) AS p(room_id, since)
		CROSS JOIN LATERAL (
			SELECT ev.event_id FROM events ev
			WHERE ev.room_id = p.room_id AND ev.stream_ordering > p.since AND ev.state_key IS NOT NULL
		) ch
		JOIN room_state rs ON rs.event_id = ch.event_id
		JOIN events e ON e.event_id = rs.event_id
		ORDER BY e.room_id, e.type, e.state_key`, rooms, positions)
	if err != nil {
		return nil, fmt.Errorf("store: state changed since: %w", err)
	}

	return groupEvents(rows)
}

// StateByKeys - текущее состояние комнат по точным ключам, по типам целиком и по
// комнатам целиком, одним запросом (по первичному ключу room_state).
func (s *SyncStore) StateByKeys(ctx context.Context, req core.StateKeysRequest) (map[string][]core.Event, error) {
	exactRooms := make([]string, 0, len(req.Exact))
	exactTypes := make([]string, 0, len(req.Exact))
	exactKeys := make([]string, 0, len(req.Exact))

	for _, ref := range req.Exact {
		exactRooms = append(exactRooms, ref.RoomID)
		exactTypes = append(exactTypes, ref.Type)
		exactKeys = append(exactKeys, ref.StateKey)
	}

	typeRooms := make([]string, 0, len(req.Types))
	types := make([]string, 0, len(req.Types))

	for _, ref := range req.Types {
		typeRooms = append(typeRooms, ref.RoomID)
		types = append(types, ref.Type)
	}

	all := req.All
	if all == nil {
		all = []string{}
	}

	rows, err := s.db.pool.Query(ctx, `
		SELECT `+slidingEventColumns+`
		FROM (
			SELECT rs.event_id
			FROM unnest($1::text[], $2::text[], $3::text[]) AS k(room_id, type, state_key)
			JOIN room_state rs ON rs.room_id = k.room_id AND rs.type = k.type AND rs.state_key = k.state_key
			UNION
			SELECT rs.event_id
			FROM unnest($4::text[], $5::text[]) AS t(room_id, type)
			JOIN room_state rs ON rs.room_id = t.room_id AND rs.type = t.type
			UNION
			SELECT rs.event_id FROM room_state rs WHERE rs.room_id IN (SELECT unnest($6::text[]))
		) x
		JOIN events e ON e.event_id = x.event_id
		ORDER BY e.room_id, e.type, e.state_key`, exactRooms, exactTypes, exactKeys, typeRooms, types, all)
	if err != nil {
		return nil, fmt.Errorf("store: state by keys: %w", err)
	}

	return groupEvents(rows)
}

// groupEvents раскладывает события по комнатам.
func groupEvents(rows pgx.Rows) (map[string][]core.Event, error) {
	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	out := map[string][]core.Event{}

	for i := range events {
		out[events[i].RoomID] = append(out[events[i].RoomID], events[i])
	}

	return out, nil
}

// DeleteToDeviceUpTo удаляет сообщения устройства с позицией не больше upTo:
// расширение to_device sliding sync подтверждает доставку закладкой since
// (MSC3885, как Synapse).
func (db *DB) DeleteToDeviceUpTo(ctx context.Context, userID, deviceID string, upTo int64) error {
	if _, err := db.pool.Exec(ctx, `
		DELETE FROM device_inbox WHERE user_id = $1 AND device_id = $2 AND stream_ordering <= $3`,
		userID, deviceID, upTo); err != nil {
		return fmt.Errorf("store: delete delivered to-device: %w", err)
	}

	return nil
}
