//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// remoteJoinFixtures — state/auth_chain/join-event для ApplyRemoteJoin.
func remoteJoinFixtures(t *testing.T) (map[string]any, []map[string]any, []map[string]any) {
	t.Helper()

	create := map[string]any{
		"event_id": "$create:remote.test", "room_id": "!remote:remote.test", "type": "m.room.create",
		"sender": "@alice:remote.test", "state_key": "", "content": map[string]any{"creator": "@alice:remote.test"},
		"depth": float64(1), "origin_server_ts": float64(1), "prev_events": []any{}, "auth_events": []any{},
	}

	join := map[string]any{
		"event_id": "$join:ours.test", "room_id": "!remote:remote.test", "type": "m.room.member",
		"sender": "@bob:ours.test", "state_key": "@bob:ours.test", "content": map[string]any{"membership": "join"},
		"depth": float64(2), "origin_server_ts": float64(2), "prev_events": []any{"$create:remote.test"},
		"auth_events": []any{"$create:remote.test"},
	}

	return join, []map[string]any{create}, []map[string]any{create}
}

// TestApplyRemoteJoin — состояние и membership применяются одной транзакцией.
func TestApplyRemoteJoin(t *testing.T) {
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

	join, state, authChain := remoteJoinFixtures(t)

	if err := db.ApplyRemoteJoin(ctx, "!remote:remote.test", "11", "@bob:ours.test", "remote.test",
		join, state, authChain, func(map[string]any) error { return nil }); err != nil {
		t.Fatalf("apply remote join: %v", err)
	}

	// Membership и состояние.
	var membership string
	if err := db.Pool().QueryRow(ctx,
		"SELECT membership FROM room_memberships WHERE room_id=$1 AND user_id='@bob:ours.test'",
		"!remote:remote.test").Scan(&membership); err != nil {
		t.Fatalf("membership: %v", err)
	}

	if membership != "join" {
		t.Fatalf("membership = %q", membership)
	}

	var stateCount int
	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM room_state WHERE room_id=$1", "!remote:remote.test").Scan(&stateCount); err != nil {
		t.Fatalf("state count: %v", err)
	}

	if stateCount < 2 {
		t.Fatalf("room_state строк %d, ожидали ≥2 (create + join)", stateCount)
	}

	var roomVersion string
	if err := db.Pool().QueryRow(ctx, "SELECT room_version FROM rooms WHERE room_id='!remote:remote.test'").Scan(&roomVersion); err != nil {
		t.Fatalf("room version: %v", err)
	}

	if roomVersion != "11" {
		t.Fatalf("room_version в БД = %q, ожидали 11", roomVersion)
	}

	rooms, err := db.DestinationRooms(ctx, "remote.test")
	if err != nil || len(rooms) != 1 {
		t.Fatalf("destination_rooms = %v, err=%v", rooms, err)
	}

	// Повторный join идемпотентен.
	if err := db.ApplyRemoteJoin(ctx, "!remote:remote.test", "11", "@bob:ours.test", "remote.test",
		join, state, authChain, func(map[string]any) error { return nil }); err != nil {
		t.Fatalf("повторный apply: %v", err)
	}

	var events int
	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM events WHERE room_id=$1", "!remote:remote.test").Scan(&events); err != nil {
		t.Fatalf("events: %v", err)
	}

	if events != 2 {
		t.Fatalf("событий %d, ожидали 2 (create + join)", events)
	}

	json.Marshal(struct{}{})

	// Ошибка проверки подписи — ничего не пишется (транзакционность).
	join2, state2, auth2 := remoteJoinFixtures(t)

	join2["room_id"] = "!remote2:remote.test"
	join2["event_id"] = "$join2:ours.test"
	state2[0]["room_id"] = "!remote2:remote.test"

	err = db.ApplyRemoteJoin(ctx, "!remote2:remote.test", "10", "@bob:ours.test", "remote.test",
		join2, state2, auth2, func(map[string]any) error { return errors.New("bad signature") })
	if err == nil {
		t.Fatal("ожидали ошибку проверки подписи")
	}

	var rooms2 int
	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM rooms WHERE room_id='!remote2:remote.test'").Scan(&rooms2); err != nil {
		t.Fatalf("count rooms2: %v", err)
	}

	if rooms2 != 0 {
		t.Fatalf("после ошибки комната записана (%d) — нет транзакционности", rooms2)
	}
}

// TestApplyRemoteJoinStateOutsideTimeline - state и auth_chain из send_join -
// состояние до входа (outlier), а не новые события таймлайна: раньше /sync и
// /messages отдавали их клиенту как сообщения после join, prev_events нового
// события ссылались на них. Создатель комнаты - из m.room.create, а не
// вошедший; строка комнаты из входящего invite поправляется.
func TestApplyRemoteJoinStateOutsideTimeline(t *testing.T) {
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
	roomID := "!remote:remote.test"

	// Комнату завел входящий invite: создатель - пригласивший, версия по умолчанию.
	if _, err := db.Pool().Exec(ctx, `INSERT INTO rooms (room_id, creator) VALUES ($1, '@carol:remote.test')`, roomID); err != nil {
		t.Fatalf("invite room: %v", err)
	}

	join, state, authChain := remoteJoinFixtures(t)

	if err := db.ApplyRemoteJoin(ctx, roomID, "11", "@bob:ours.test", "remote.test",
		join, state, authChain, func(map[string]any) error { return nil }); err != nil {
		t.Fatalf("apply remote join: %v", err)
	}

	var creator, version string
	if err := db.Pool().QueryRow(ctx, `SELECT creator, room_version FROM rooms WHERE room_id = $1`, roomID).
		Scan(&creator, &version); err != nil {
		t.Fatalf("room: %v", err)
	}

	if creator != "@alice:remote.test" || version != "11" {
		t.Fatalf("creator = %q, room_version = %q, ожидали @alice:remote.test и 11", creator, version)
	}

	timeline, err := db.RoomEventsSince(ctx, roomID, 0, 100)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}

	if len(timeline) != 1 || timeline[0].EventID != "$join:ours.test" {
		t.Fatalf("таймлайн после join = %v, ожидали только join", eventIDs(timeline))
	}

	synced, err := store.NewSyncStore(db).EventsSince(ctx, []string{roomID}, 0, 10, nil)
	if err != nil {
		t.Fatalf("sync timeline: %v", err)
	}

	if got := synced[roomID]; len(got) != 1 || got[0].EventID != "$join:ours.test" {
		t.Fatalf("/sync timeline после join = %v, ожидали только join", eventIDs(got))
	}

	rooms := store.NewRoomStore(db)

	latest, err := rooms.LatestEvents(ctx, roomID, 20)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	if len(latest) != 1 || latest[0].EventID != "$join:ours.test" {
		t.Fatalf("prev_events нового события = %v, ожидали только join", eventIDs(latest))
	}

	page, err := rooms.RoomEvents(ctx, roomID, 1<<62, 100, true)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}

	if len(page) != 1 {
		t.Fatalf("/messages после join = %v, ожидали только join", eventIDs(page))
	}

	// Состояние при этом полное: create на месте.
	stateEvents, err := rooms.StateEvents(ctx, roomID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	var hasCreate bool

	for i := range stateEvents {
		hasCreate = hasCreate || stateEvents[i].Type == "m.room.create"
	}

	if !hasCreate {
		t.Fatalf("в состоянии нет create: %v", eventIDs(stateEvents))
	}
}

func eventIDs(events []core.Event) []string {
	out := make([]string, 0, len(events))
	for i := range events {
		out = append(out, events[i].EventID)
	}

	return out
}
