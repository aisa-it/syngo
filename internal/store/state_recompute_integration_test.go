//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/store"
)

// TestRecomputeRoomState — room_state заполняется из events последним
// по stream_ordering state-событием, повторный прогон идемпотентен, расхождений с membership нет.
func TestRecomputeRoomState(t *testing.T) {
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

	const roomID = "!state:test.local"

	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	// Два state-события одного типа: победить должно последнее по stream_ordering.
	events := []struct {
		id     string
		typ    string
		key    string
		stream int64
		body   string
	}{
		{"$create", "m.room.create", "", 1, `{}`},
		{"$topic1", "m.room.topic", "", 2, `{"topic":"old"}`},
		{"$topic2", "m.room.topic", "", 5, `{"topic":"new"}`},
		{"$member", "m.room.member", "@alice:test.local", 3, `{"membership":"join"}`},
	}

	for _, e := range events {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,$3,'@alice:test.local',$4,$5,$6,1700000000000,'test.local',$6)`,
			e.id, roomID, e.typ, e.key, []byte(e.body), e.stream); err != nil {
			t.Fatalf("insert event %s: %v", e.id, err)
		}
	}

	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		VALUES ('$member', $1, '@alice:test.local', '@alice:test.local', 'join')`, roomID); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	report, err := db.RecomputeRoomState(ctx, 10)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}

	if report.Rooms != 1 || report.StateRows != 3 {
		t.Fatalf("отчёт: %+v (ожидали 1 комнату и 3 state-строки)", report)
	}

	if report.HasMismatches() {
		t.Fatalf("расхождения с membership: %v", report.Mismatches)
	}

	var topicEvent string
	if err := db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id=$1 AND type='m.room.topic' AND state_key=''", roomID).Scan(&topicEvent); err != nil {
		t.Fatalf("room_state topic: %v", err)
	}

	if topicEvent != "$topic2" {
		t.Fatalf("в room_state попало %s, ожидали последнее $topic2", topicEvent)
	}

	// Повторный прогон идемпотентен.
	report2, err := db.RecomputeRoomState(ctx, 10)
	if err != nil {
		t.Fatalf("recompute 2: %v", err)
	}

	if report2.HasMismatches() {
		t.Fatalf("расхождения на повторном прогоне: %v", report2.Mismatches)
	}

	var count int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM room_state WHERE room_id=$1", roomID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}

	if count != 3 {
		t.Fatalf("state-строк %d, ожидали 3", count)
	}

	// StateForRooms (его использует /sync) теперь отдаёт состояние.
	syncStore := store.NewSyncStore(db)

	state, err := syncStore.StateForRooms(ctx, []string{roomID})
	if err != nil {
		t.Fatalf("state for rooms: %v", err)
	}

	if len(state[roomID]) != 3 {
		t.Fatalf("/sync state: %d событий, ожидали 3", len(state[roomID]))
	}

	_ = fmt.Sprintf("%v", state)
}
