package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aisa-it/syngo/internal/store"
)

// generate создает профиль данных одной командой SQL на сущность (идемпотентно).
//
// Кроме сообщений в каждой комнате есть состояние (create, power_levels, name) и
// MembersPerRoom участников с member-событиями в room_state: без состояния стенд
// мерил /sync на пустом room_state и одном участнике.
func generate(ctx context.Context, db *store.DB, prof profile) error {
	userID := benchUserID(prof.ServerName)

	// Отправитель сообщения k: измеряемый пользователь или, с -foreign-senders,
	// участники bench1..bench(M-1) по кругу - тогда у измеряемого есть
	// непрочитанные и /sync считает unread_notifications.
	senderExpr := "$2::text"
	if prof.ForeignSenders && prof.MembersPerRoom > 1 {
		// $2 остается в выражении: иначе PostgreSQL не выводит тип параметра.
		senderExpr = fmt.Sprintf("CASE WHEN k < 0 THEN $2::text ELSE '@bench' || (1 + k %% %d) || ':' || $1 END",
			prof.MembersPerRoom-1)
	}

	// Участник i комнаты: 0 - измеряемый пользователь, 1..M-1 - bench1..bench(M-1).
	const memberExpr = `CASE WHEN i = 0 THEN $2 ELSE '@bench' || i || ':' || $1 END`

	statements := []struct {
		sql  string
		args []any
	}{
		{
			sql: `INSERT INTO users (user_id, localpart, password_hash)
			      SELECT '@bench' || i || ':' || $1, 'bench' || i, 'x'
			      FROM generate_series(1, $2) AS i
			      UNION ALL SELECT $3, $4, 'x'
			      ON CONFLICT (user_id) DO NOTHING`,
			args: []any{prof.ServerName, prof.Users, userID, benchLocalpart},
		},
		{
			sql: `INSERT INTO rooms (room_id, room_version, creator, is_public)
			      SELECT '!bench' || i || ':' || $1, '10', $2, false
			      FROM generate_series(1, $3) AS i
			      ON CONFLICT (room_id) DO NOTHING`,
			args: []any{prof.ServerName, userID, prof.Rooms},
		},
		{
			// Состояние комнаты без участников: create, power_levels, name.
			sql: `INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
			      SELECT '$st' || s.n || '_' || r || ':' || $1, '!bench' || r || ':' || $1, s.type, $2, '',
			             CASE s.n
			                  WHEN 1 THEN jsonb_build_object('creator', $2::text, 'room_version', '10')
			                  WHEN 2 THEN jsonb_build_object('users', jsonb_build_object($2::text, 100))
			                  ELSE jsonb_build_object('name', 'bench ' || r)
			             END,
			             s.n, 1699999990000 + s.n, $1
			      FROM generate_series(1, $3) AS r,
			           (VALUES (1, 'm.room.create'), (2, 'm.room.power_levels'), (3, 'm.room.name')) AS s(n, type)
			      ORDER BY r, s.n
			      ON CONFLICT (event_id) DO NOTHING`,
			args: []any{prof.ServerName, userID, prof.Rooms},
		},
		{
			sql: `INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
			      SELECT '$mem' || r || '_' || i || ':' || $1, '!bench' || r || ':' || $1, 'm.room.member',
			             ` + memberExpr + `, ` + memberExpr + `,
			             '{"membership":"join"}'::jsonb, 4 + i, 1699999995000 + i, $1
			      FROM generate_series(1, $3) AS r, generate_series(0, $4 - 1) AS i
			      ORDER BY r, i
			      ON CONFLICT (event_id) DO NOTHING`,
			args: []any{prof.ServerName, userID, prof.Rooms, prof.MembersPerRoom},
		},
		{
			sql: `INSERT INTO room_state (room_id, type, state_key, event_id)
			      SELECT e.room_id, e.type, e.state_key, e.event_id
			      FROM events e
			      WHERE e.room_id LIKE '!bench%:' || $1 AND e.state_key IS NOT NULL
			        AND (e.event_id LIKE '$st%' OR e.event_id LIKE '$mem%')
			      ON CONFLICT (room_id, type, state_key) DO NOTHING`,
			args: []any{prof.ServerName},
		},
		{
			sql: `INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
			      SELECT '$bench' || r || '_' || k || ':' || $1, '!bench' || r || ':' || $1,
			             'm.room.message', ` + senderExpr + `, '{"msgtype":"m.text","body":"load"}'::jsonb,
			             100 + k, 1700000000000 + k, $1
			      FROM generate_series(1, $3) AS r, generate_series(1, $4) AS k
			      ON CONFLICT (event_id) DO NOTHING`,
			args: []any{prof.ServerName, userID, prof.Rooms, prof.EventsPerRoom},
		},
		{
			sql: `INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			      SELECT '$mem' || r || '_' || i || ':' || $1, '!bench' || r || ':' || $1,
			             ` + memberExpr + `, ` + memberExpr + `, 'join'
			      FROM generate_series(1, $3) AS r, generate_series(0, $4 - 1) AS i
			      ON CONFLICT (room_id, user_id) DO UPDATE SET membership = 'join'`,
			args: []any{prof.ServerName, userID, prof.Rooms, prof.MembersPerRoom},
		},
	}

	for _, statement := range statements {
		if _, err := db.Pool().Exec(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("generate: %w", err)
		}
	}

	return nil
}

// insertDelta пишет новые события в count разных комнат (сценарий delta).
func insertDelta(ctx context.Context, db *store.DB, prof profile, count int) error {
	const query = `
		INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		SELECT '$delta' || r || '_' || ($4::bigint)::text || ':' || $1, '!bench' || r || ':' || $1,
		       'm.room.message', $2, '{"body":"delta"}'::jsonb, $4::bigint + 100,
		       $4::bigint + 1700000000000, $1
		FROM generate_series(1, $3) AS r
		ON CONFLICT (event_id) DO NOTHING`

	if _, err := db.Pool().Exec(ctx, query, prof.ServerName, benchUserID(prof.ServerName), count,
		time.Now().UnixNano()%1_000_000_000); err != nil {
		return fmt.Errorf("insert delta: %w", err)
	}

	return nil
}
