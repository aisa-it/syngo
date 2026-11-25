package store

import (
	"context"
	"errors"
	"fmt"
)

// StateRecomputeReport — итоги материализации room_state.
type StateRecomputeReport struct {
	Rooms          int64
	StateRows      int64
	MembershipRows int64
	Mismatches     []string
}

// HasMismatches сообщает о расхождениях room_state и room_memberships.
func (r StateRecomputeReport) HasMismatches() bool { return len(r.Mismatches) > 0 }

// RecomputeRoomState материализует room_state из events: для каждой комнаты берётся
// последнее по stream_ordering state-событие по каждому (type, state_key).
//
// Идемпотентно: повторный прогон даёт тот же результат (UPSERT). Работает порциями комнат,
// поэтому годится и для уже импортированных баз (команда syngo-recompute-state), и как
// завершающий шаг импорта.
func (db *DB) RecomputeRoomState(ctx context.Context, batchSize int) (StateRecomputeReport, error) {
	report := StateRecomputeReport{}

	if db == nil || db.pool == nil {
		return report, ErrNotConfigured
	}

	if batchSize <= 0 {
		batchSize = 100
	}

	var lastRoomID string

	for {
		roomIDs, err := db.roomIDsAfter(ctx, lastRoomID, batchSize)
		if err != nil {
			return report, err
		}

		if len(roomIDs) == 0 {
			break
		}

		const upsert = `
			WITH latest AS (
				SELECT DISTINCT ON (room_id, type, state_key)
				       room_id, type, state_key, event_id
				FROM events
				WHERE state_key IS NOT NULL AND room_id = ANY($1)
				ORDER BY room_id, type, state_key, stream_ordering DESC
			)
			INSERT INTO room_state (room_id, type, state_key, event_id)
			SELECT room_id, type, state_key, event_id FROM latest
			ON CONFLICT (room_id, type, state_key)
			DO UPDATE SET event_id = EXCLUDED.event_id, updated_at = now()`

		tag, err := db.pool.Exec(ctx, upsert, roomIDs)
		if err != nil {
			return report, fmt.Errorf("store: recompute room_state: %w", err)
		}

		const membershipCount = `
			SELECT count(*) FROM room_memberships WHERE room_id = ANY($1) AND membership = 'join'`

		var memberships int64
		if err := db.pool.QueryRow(ctx, membershipCount, roomIDs).Scan(&memberships); err != nil {
			return report, fmt.Errorf("store: count memberships: %w", err)
		}

		report.Rooms += int64(len(roomIDs))
		report.StateRows += tag.RowsAffected()
		report.MembershipRows += memberships

		mismatches, err := db.stateMembershipMismatches(ctx, roomIDs)
		if err != nil {
			return report, err
		}

		report.Mismatches = append(report.Mismatches, mismatches...)

		lastRoomID = roomIDs[len(roomIDs)-1]
	}

	return report, nil
}

// roomIDsAfter возвращает следующие room_id после lastRoomID (keyset-пагинация).
func (db *DB) roomIDsAfter(ctx context.Context, lastRoomID string, limit int) ([]string, error) {
	const query = `
		SELECT room_id FROM rooms
		WHERE room_id > $1
		ORDER BY room_id
		LIMIT $2`

	rows, err := db.pool.Query(ctx, query, lastRoomID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: rooms page: %w", err)
	}

	defer rows.Close()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan room id: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate rooms: %w", err)
	}

	return ids, nil
}

// stateMembershipMismatches ищет расхождения: у join-членства в room_state должен быть
// m.room.member с тем же state_key и membership=join.
func (db *DB) stateMembershipMismatches(ctx context.Context, roomIDs []string) ([]string, error) {
	const query = `
		SELECT m.room_id, m.user_id
		FROM room_memberships m
		LEFT JOIN room_state rs
		       ON rs.room_id = m.room_id AND rs.type = 'm.room.member' AND rs.state_key = m.user_id
		LEFT JOIN events e ON e.event_id = rs.event_id
		WHERE m.room_id = ANY($1)
		  AND m.membership = 'join'
		  AND (rs.event_id IS NULL OR coalesce(e.content->>'membership', '') <> 'join')
		LIMIT 20`

	rows, err := db.pool.Query(ctx, query, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: membership mismatches: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var roomID, userID string
		if err := rows.Scan(&roomID, &userID); err != nil {
			return nil, fmt.Errorf("store: scan mismatch: %w", err)
		}

		out = append(out, roomID+" "+userID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate mismatches: %w", err)
	}

	return out, nil
}

// ErrNoState — комната без state-событий (для отчёта, не ошибка).
var ErrNoState = errors.New("store: room has no state events")
