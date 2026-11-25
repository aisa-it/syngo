package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// PushRoomState - срез состояния комнаты для push-правил (тип домена).
type PushRoomState = core.PushRoomState

// PushRoomStates отдает для комнат число участников, power_levels, создателя и
// имена пользователей userIDs - двумя запросами независимо от числа комнат.
func (db *DB) PushRoomStates(ctx context.Context, roomIDs, userIDs []string) (map[string]*PushRoomState, error) {
	out := make(map[string]*PushRoomState, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	for _, roomID := range roomIDs {
		out[roomID] = &PushRoomState{DisplayNames: map[string]string{}}
	}

	rows, err := db.pool.Query(ctx, `
		SELECT rs.room_id, rs.type, rs.state_key, e.sender, e.content
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = ANY($1)
		  AND ((rs.type IN ('m.room.power_levels', 'm.room.create') AND rs.state_key = '')
		       OR (rs.type = 'm.room.member' AND rs.state_key = ANY($2)))`, roomIDs, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push room state: %w", err)
	}

	for rows.Next() {
		var (
			roomID, evType, stateKey, sender string
			content                          []byte
		)

		if err := rows.Scan(&roomID, &evType, &stateKey, &sender, &content); err != nil {
			rows.Close()

			return nil, fmt.Errorf("store: scan push room state: %w", err)
		}

		st := out[roomID]
		if st == nil {
			continue
		}

		switch evType {
		case "m.room.power_levels":
			st.PowerLevels = content
		case "m.room.create":
			st.Creator = sender
		case "m.room.member":
			var member struct {
				DisplayName string `json:"displayname"`
			}

			if json.Unmarshal(content, &member) == nil {
				st.DisplayNames[stateKey] = member.DisplayName
			}
		}
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: push room state: %w", err)
	}

	counts, err := db.pool.Query(ctx, `
		SELECT room_id, count(*) FROM room_memberships
		WHERE room_id = ANY($1) AND membership = 'join'
		GROUP BY room_id`, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push member counts: %w", err)
	}

	defer counts.Close()

	for counts.Next() {
		var (
			roomID string
			count  int
		)

		if err := counts.Scan(&roomID, &count); err != nil {
			return nil, fmt.Errorf("store: scan member count: %w", err)
		}

		if st := out[roomID]; st != nil {
			st.MemberCount = count
		}
	}

	return out, counts.Err()
}

// UnreadCandidates отдает события комнат после позиции low[room] и не выше
// upper - не больше perRoom последних на комнату, по возрастанию. Это
// кандидаты для счетчиков unread_notifications в /sync: правила вычисляются в
// коде. LATERAL с LIMIT останавливает скан индекса (room_id,
// stream_ordering) на perRoom строках в каждой комнате.
func (db *DB) UnreadCandidates(
	ctx context.Context, low map[string]int64, upper int64, perRoom int,
) (map[string][]core.Event, error) {
	out := make(map[string][]core.Event, len(low))
	if len(low) == 0 {
		return out, nil
	}

	roomIDs := make([]string, 0, len(low))
	lows := make([]int64, 0, len(low))

	for roomID, bound := range low {
		roomIDs = append(roomIDs, roomID)
		lows = append(lows, bound)
	}

	rows, err := db.pool.Query(ctx, `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by, '')
		FROM unnest($1::text[], $2::bigint[]) AS r(room_id, low)
		CROSS JOIN LATERAL (
			SELECT * FROM events ev
			WHERE ev.room_id = r.room_id AND ev.stream_ordering > r.low AND ev.stream_ordering <= $3
			ORDER BY ev.stream_ordering DESC
			LIMIT $4
		) e
		ORDER BY e.stream_ordering`, roomIDs, lows, upper, perRoom)
	if err != nil {
		return nil, fmt.Errorf("store: unread candidates: %w", err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	for i := range events {
		out[events[i].RoomID] = append(out[events[i].RoomID], events[i])
	}

	return out, nil
}

// NotificationCandidates отдает события комнат, где пользователь состоит,
// после его входа, не от него и не удаленные, с позицией меньше before
// (0 - без границы), от новых к старым. Какие из них - уведомления, решают
// push-правила; раньше /notifications отдавал все m.room.message.
func (db *DB) NotificationCandidates(ctx context.Context, userID string, before int64, limit int) ([]core.Event, error) {
	if before <= 0 {
		before = 1<<62 - 1
	}

	rows, err := db.pool.Query(ctx, `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by, '')
		FROM events e
		JOIN room_memberships m ON m.room_id = e.room_id AND m.user_id = $1 AND m.membership = 'join'
		LEFT JOIN events j ON j.event_id = m.event_id
		WHERE e.stream_ordering < $2 AND e.stream_ordering > coalesce(j.stream_ordering, 0)
		  AND e.sender <> $1 AND e.redacted_by IS NULL
		ORDER BY e.stream_ordering DESC
		LIMIT $3`, userID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("store: notification candidates: %w", err)
	}

	return scanEventRows(rows)
}
