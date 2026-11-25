package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// MembersPage отдаёт страницу m.room.member-событий, упорядоченных по state_key.
func (s *RoomStore) MembersPage(ctx context.Context, roomID, at string, limit int) ([]core.Event, string, error) {
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = 'm.room.member' AND rs.state_key > $2
		ORDER BY rs.state_key
		LIMIT $3`

	rows, err := s.db.pool.Query(ctx, query, roomID, at, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: members page: %w", err)
	}

	defer rows.Close()

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, "", err
	}

	next := ""
	if len(events) > limit {
		events = events[:limit]
	}

	if len(events) == limit && events[len(events)-1].StateKey != nil {
		next = *events[len(events)-1].StateKey
	}

	return events, next, nil
}

// JoinedMembers отдаёт участников комнаты с membership=join и их профили.
func (s *RoomStore) JoinedMembers(ctx context.Context, roomID string) (map[string]core.Profile, error) {
	const query = `
		SELECT rs.state_key,
		       coalesce(e.content->>'displayname',''),
		       coalesce(e.content->>'avatar_url','')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = 'm.room.member'
		  AND e.content->>'membership' = 'join'`

	rows, err := s.db.pool.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: joined members: %w", err)
	}

	defer rows.Close()

	out := map[string]core.Profile{}

	for rows.Next() {
		var (
			userID    string
			display   string
			avatarURL string
		)

		if err := rows.Scan(&userID, &display, &avatarURL); err != nil {
			return nil, fmt.Errorf("store: scan joined member: %w", err)
		}

		out[userID] = core.Profile{UserID: userID, DisplayName: display, AvatarURL: avatarURL}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate joined members: %w", err)
	}

	return out, nil
}

// AliasesByRoom возвращает алиасы комнаты.
func (s *RoomStore) AliasesByRoom(ctx context.Context, roomID string) ([]string, error) {
	const query = `SELECT room_alias FROM room_aliases WHERE room_id = $1 ORDER BY room_alias`

	rows, err := s.db.pool.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: aliases of room: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, fmt.Errorf("store: scan alias: %w", err)
		}

		out = append(out, alias)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate aliases: %w", err)
	}

	return out, nil
}

// ResolveAlias возвращает room_id по алиасу.
func (s *RoomStore) ResolveAlias(ctx context.Context, alias string) (string, error) {
	const query = `SELECT room_id FROM room_aliases WHERE room_alias = $1`

	var roomID string

	err := s.db.pool.QueryRow(ctx, query, alias).Scan(&roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("store: alias %s: %w", alias, core.ErrNotFound)
		}

		return "", fmt.Errorf("store: resolve alias %s: %w", alias, err)
	}

	return roomID, nil
}

// AliasOwner возвращает создателя алиаса и комнату.
func (s *RoomStore) AliasOwner(ctx context.Context, alias string) (creator, roomID string, err error) {
	const query = `SELECT creator, room_id FROM room_aliases WHERE room_alias = $1`

	err = s.db.pool.QueryRow(ctx, query, alias).Scan(&creator, &roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("store: alias %s: %w", alias, core.ErrNotFound)
		}

		return "", "", fmt.Errorf("store: alias owner %s: %w", alias, err)
	}

	return creator, roomID, nil
}

// CreateAlias добавляет алиас; занятый алиас — core.ErrForbidden.
func (s *RoomStore) CreateAlias(ctx context.Context, alias, roomID, creator string) error {
	const query = `
		INSERT INTO room_aliases (room_alias, room_id, creator) VALUES ($1, $2, $3)
		ON CONFLICT (room_alias) DO NOTHING`

	tag, err := s.db.pool.Exec(ctx, query, alias, roomID, creator)
	if err != nil {
		return fmt.Errorf("store: create alias %s: %w", alias, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: alias %s is taken: %w", alias, core.ErrForbidden)
	}

	return nil
}

// DeleteAlias удаляет алиас.
func (s *RoomStore) DeleteAlias(ctx context.Context, alias string) error {
	const query = `DELETE FROM room_aliases WHERE room_alias = $1`

	tag, err := s.db.pool.Exec(ctx, query, alias)
	if err != nil {
		return fmt.Errorf("store: delete alias %s: %w", alias, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: alias %s: %w", alias, core.ErrNotFound)
	}

	return nil
}
