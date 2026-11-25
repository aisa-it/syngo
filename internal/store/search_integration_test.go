//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"

	"github.com/aisa-it/syngo/internal/store"
)

// TestSearchMessagesRedactedAndVisibility - удаленное сообщение не находится
// по исходному тексту, хотя content в БД сохраняется; событие вне границ видимости не находится.
func TestSearchMessagesRedactedAndVisibility(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	const (
		roomID = "!search:test.local"
		sender = "@searchalice:test.local"
	)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,'searchalice','x')`, sender); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ($1,'10',$2,true)`, roomID, sender); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	seedMessage(t, db, roomID, "$gone:test.local", sender, `{"msgtype":"m.text","body":"secretpassword123"}`)
	seedMessage(t, db, roomID, "$early:test.local", sender, `{"msgtype":"m.text","body":"earlyword"}`)
	seedMessage(t, db, roomID, "$late:test.local", sender, `{"msgtype":"m.text","body":"lateword"}`)

	// Redaction применяется в транзакции записи события m.room.redaction.
	rooms := store.NewRoomStore(db)

	prev, err := rooms.LatestEvents(ctx, roomID, 1)
	if err != nil || len(prev) == 0 {
		t.Fatalf("LatestEvents: %v", err)
	}

	redaction := core.Event{
		EventID: "$redaction:test.local", RoomID: roomID, Type: "m.room.redaction", Sender: sender,
		Content:    json.RawMessage(`{"redacts":"$gone:test.local"}`),
		PrevEvents: []string{prev[0].EventID}, Depth: prev[0].Depth + 1, OriginServerTS: 1, Origin: "test.local",
	}

	if _, _, err := rooms.AppendEvent(ctx, redaction, nil); err != nil {
		t.Fatalf("redact: %v", err)
	}

	all := map[string]store.Visibility{roomID: {}}

	found, err := db.SearchMessages(ctx, all, "secretpassword123", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(found) != 0 {
		t.Fatalf("поиск нашел удаленное сообщение: %+v", found)
	}

	var late int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT stream_ordering FROM events WHERE event_id = '$late:test.local'`).Scan(&late); err != nil {
		t.Fatalf("stream_ordering: %v", err)
	}

	// Видно только с позиции lateword (как joined с входом прямо перед ним).
	restricted := map[string]store.Visibility{roomID: {From: late}}

	for term, want := range map[string]int{"earlyword": 0, "lateword": 1} {
		found, err := db.SearchMessages(ctx, restricted, term, 10)
		if err != nil {
			t.Fatalf("search %s: %v", term, err)
		}

		if len(found) != want {
			t.Fatalf("поиск %q при границе видимости: %d результатов, ожидали %d", term, len(found), want)
		}
	}

	found, err = db.SearchMessages(ctx, all, "earlyword", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("поиск без ограничений: %d %v", len(found), err)
	}
}
