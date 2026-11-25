package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// Visibility — какие события комнаты видит пользователь (тип - в core).
type Visibility = core.Visibility

var _ core.VisibilityStore = (*DB)(nil)

// VisibilityCutoffs возвращает для каждой комнаты границы видимости событий для
// пользователя по history_visibility. From:
//   - world_readable, shared - вся история (0);
//   - invited - с начала текущего членства (приглашение или вход);
//   - joined - с текущего входа.
//
// Упрощение относительно Synapse: значение history_visibility на момент события
// различается только как «до текущего значения» (видно, как при shared по
// умолчанию) и «после». Начало членства - первое invite/join после последнего
// leave/ban пользователя.
func (db *DB) VisibilityCutoffs(ctx context.Context, userID string, roomIDs []string) (map[string]Visibility, error) {
	out := make(map[string]Visibility, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	visibility := map[string]string{}
	setAt := map[string]int64{}

	rows, err := db.pool.Query(ctx, `
		SELECT s.room_id, e.content, e.stream_ordering
		FROM room_state s JOIN events e ON e.event_id = s.event_id
		WHERE s.room_id = ANY($1) AND s.type = 'm.room.history_visibility' AND s.state_key = ''`, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: history visibility: %w", err)
	}

	for rows.Next() {
		var (
			roomID  string
			content []byte
			pos     int64
		)

		if err := rows.Scan(&roomID, &content, &pos); err != nil {
			rows.Close()

			return nil, fmt.Errorf("store: scan history visibility: %w", err)
		}

		var body struct {
			HistoryVisibility string `json:"history_visibility"`
		}

		_ = json.Unmarshal(content, &body)
		visibility[roomID] = body.HistoryVisibility
		setAt[roomID] = pos
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate history visibility: %w", err)
	}

	restricted := make([]string, 0, len(roomIDs))

	for _, roomID := range roomIDs {
		switch visibility[roomID] {
		case "invited", "joined":
			restricted = append(restricted, roomID)
		default:
			// shared по умолчанию (спецификация), world_readable.
			out[roomID] = Visibility{}
		}
	}

	if len(restricted) == 0 {
		return out, nil
	}

	// left - последнее членство leave/ban: окно видимости до выхода сохраняется
	// (вышедший читает историю до выхода, SPEC-216; граница выхода - в
	// RoomService.readBound), новое invite/join начинает окно заново.
	type streak struct {
		invite, join int64
		left         bool
	}

	streaks := make(map[string]*streak, len(restricted))

	rows, err = db.pool.Query(ctx, `
		SELECT room_id, stream_ordering, coalesce(content->>'membership', '')
		FROM events
		WHERE type = 'm.room.member' AND state_key = $1 AND room_id = ANY($2)
		ORDER BY room_id, stream_ordering`, userID, restricted)
	if err != nil {
		return nil, fmt.Errorf("store: membership history: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, membership string
			pos                int64
		)

		if err := rows.Scan(&roomID, &pos, &membership); err != nil {
			return nil, fmt.Errorf("store: scan membership history: %w", err)
		}

		st := streaks[roomID]
		if st == nil {
			st = &streak{}
			streaks[roomID] = st
		}

		if st.left && (membership == "invite" || membership == "join") {
			*st = streak{}
		}

		switch membership {
		case "invite":
			if st.invite == 0 && st.join == 0 {
				st.invite = pos
			}
		case "join":
			if st.join == 0 {
				st.join = pos
			}

			if st.invite == 0 {
				st.invite = pos
			}
		case "leave", "ban":
			st.left = true
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate membership history: %w", err)
	}

	for _, roomID := range restricted {
		cutoff := int64(1 << 62) // не член комнаты - после смены видимости ничего не видно

		if st := streaks[roomID]; st != nil {
			from := st.join
			if visibility[roomID] == "invited" {
				from = st.invite
			}

			if from != 0 {
				cutoff = from
			}
		}

		out[roomID] = Visibility{Before: setAt[roomID], From: cutoff}
	}

	return out, nil
}
