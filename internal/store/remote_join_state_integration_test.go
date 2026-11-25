//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestApplyRemoteJoinResolvesState — send_join не перезаписывает
// room_state «как пришло», а разрешает состояние (state resolution v2).
//
// Локально есть topic от участника, в присланном state — topic от не-участника.
// После разрешения должен остаться локальный topic: чужое неавторизованное событие
// не проходит auth-проверку и не попадает в материализованное состояние.
func TestApplyRemoteJoinResolvesState(t *testing.T) {
	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db := openDB(t)
	ctx := context.Background()

	const roomID = "!resolve-join:remote.test"

	if _, err := db.Pool().Exec(ctx, `INSERT INTO rooms (room_id, creator, room_version) VALUES ($1,'@alice:remote.test','11')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	seed := []struct {
		id, typ, sender, stateKey, content string
		depth                              int
	}{
		{"$create:remote.test", "m.room.create", "@alice:remote.test", "", `{"creator":"@alice:remote.test"}`, 1},
		{"$pl:remote.test", "m.room.power_levels", "@alice:remote.test", "", `{"users":{"@alice:remote.test":100}}`, 2},
		{"$alice:remote.test", "m.room.member", "@alice:remote.test", "@alice:remote.test", `{"membership":"join"}`, 3},
		{"$topic-local:remote.test", "m.room.topic", "@alice:remote.test", "", `{"topic":"local"}`, 4},
	}

	for _, ev := range seed {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
			                    depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,$3,$4,$5,$6,'{}','{}',$7,$7,'remote.test',$7)`,
			ev.id, roomID, ev.typ, ev.sender, ev.stateKey, ev.content, ev.depth); err != nil {
			t.Fatalf("seed %s: %v", ev.id, err)
		}

		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,$2,$3,$4)
			ON CONFLICT (room_id, type, state_key) DO UPDATE SET event_id = EXCLUDED.event_id`,
			roomID, ev.typ, ev.stateKey, ev.id); err != nil {
			t.Fatalf("seed state %s: %v", ev.id, err)
		}
	}

	join := map[string]any{
		"event_id": "$join:ours.test", "room_id": roomID, "type": "m.room.member",
		"sender": "@bob:ours.test", "state_key": "@bob:ours.test", "content": map[string]any{"membership": "join"},
		"depth": float64(5), "origin_server_ts": float64(50), "prev_events": []any{"$topic-local:remote.test"},
		"auth_events": []any{"$create:remote.test", "$pl:remote.test"},
	}

	// Присланный state: тот же набор + topic от не-участника (должен быть отброшен).
	state := []map[string]any{
		{
			"event_id": "$create:remote.test", "room_id": roomID, "type": "m.room.create",
			"sender": "@alice:remote.test", "state_key": "", "content": map[string]any{"creator": "@alice:remote.test"},
			"depth": float64(1), "origin_server_ts": float64(1),
		},
		{
			"event_id": "$topic-evil:remote.test", "room_id": roomID, "type": "m.room.topic",
			"sender": "@eve:remote.test", "state_key": "", "content": map[string]any{"topic": "evil"},
			"depth": float64(9), "origin_server_ts": float64(90),
			"prev_events": []any{"$topic-local:remote.test"},
			"auth_events": []any{"$create:remote.test", "$pl:remote.test"},
		},
	}

	if err := db.ApplyRemoteJoin(ctx, roomID, "11", "@bob:ours.test", "ours.test",
		join, state, state, func(map[string]any) error { return nil }); err != nil {
		t.Fatalf("apply remote join: %v", err)
	}

	var topic string
	if err := db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id=$1 AND type='m.room.topic' AND state_key=''", roomID).Scan(&topic); err != nil {
		t.Fatalf("room_state topic: %v", err)
	}

	if topic != "$topic-local:remote.test" {
		t.Fatalf("room_state topic = %q, ожидали локальный $topic-local (неавторизованное не должно победить)", topic)
	}

	var bob string
	if err := db.Pool().QueryRow(ctx,
		"SELECT membership FROM room_memberships WHERE room_id=$1 AND user_id='@bob:ours.test'", roomID).Scan(&bob); err != nil {
		t.Fatalf("membership bob: %v", err)
	}

	if bob != "join" {
		t.Fatalf("membership bob = %q", bob)
	}
}

// TestApplyRemoteJoinRejectsUnknownAuthRefs — событие с auth_events, которых нет
// ни в присланном state/auth_chain, ни в БД, отвергается: иначе в комнате
// появляется битая ссылка и пир не может её разрешить.
func TestApplyRemoteJoinRejectsUnknownAuthRefs(t *testing.T) {
	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db := openDB(t)
	ctx := context.Background()

	const roomID = "!unknown-auth:remote.test"

	join := map[string]any{
		"event_id": "$join:ours.test", "room_id": roomID, "type": "m.room.member",
		"sender": "@bob:ours.test", "state_key": "@bob:ours.test",
		"content": map[string]any{"membership": "join"},
		"depth":   float64(2), "origin_server_ts": float64(20),
		"prev_events": []any{}, "auth_events": []any{"$missing:remote.test"},
	}

	err := db.ApplyRemoteJoin(ctx, roomID, "10", "@bob:ours.test", "ours.test", join, nil, nil, nil)
	if err == nil {
		t.Fatal("ожидали отказ: auth_events ссылается на несуществующее событие")
	}
}
