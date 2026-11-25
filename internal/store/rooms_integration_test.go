//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// freshRoomStore поднимает схему с нуля и возвращает хранилище комнат.
func freshRoomStore(t *testing.T) *store.RoomStore {
	t.Helper()

	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	return store.NewRoomStore(openDB(t))
}

// TestRoomStoreLifecycle — комната, события, состояние, членство, txn и /messages.
func TestRoomStoreLifecycle(t *testing.T) {
	roomStore := freshRoomStore(t)
	ctx := context.Background()

	service := core.NewRoomService(roomStore, "test.local").WithClock(func() time.Time {
		return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	})

	roomID, err := service.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: "@alice:test.local",
		Name:    "Интеграционная",
	})
	if err != nil {
		t.Fatalf("CreateRoom() error: %v", err)
	}

	room, err := roomStore.RoomByID(ctx, roomID)
	if err != nil {
		t.Fatalf("RoomByID() error: %v", err)
	}

	if room.Creator != "@alice:test.local" || room.RoomVersion != core.DefaultRoomVersion {
		t.Fatalf("RoomByID() = %+v", room)
	}

	membership, err := roomStore.Membership(ctx, roomID, "@alice:test.local")
	if err != nil {
		t.Fatalf("Membership(creator) error: %v", err)
	}

	if membership != "join" {
		t.Fatalf("membership создателя = %q, ожидали join", membership)
	}

	if _, err := roomStore.Membership(ctx, roomID, "@bob:test.local"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Membership(посторонний) = %v, ожидали ErrNotFound", err)
	}

	for i, body := range []string{"первое", "второе", "третье"} {
		content, marshalErr := json.Marshal(map[string]string{"body": body})
		if marshalErr != nil {
			t.Fatalf("marshal: %v", marshalErr)
		}

		eventID, duplicate, sendErr := service.SendMessage(
			ctx, "@alice:test.local", "DEV1", roomID, "m.room.message",
			"txn-"+body, content,
		)
		if sendErr != nil {
			t.Fatalf("SendMessage(%s) error: %v", body, sendErr)
		}

		if duplicate || eventID == "" {
			t.Fatalf("SendMessage(%s) = %q dup=%t", body, eventID, duplicate)
		}

		// Повтор того же txn возвращает то же событие и не пишет новое.
		again, dupAgain, errAgain := service.SendMessage(
			ctx, "@alice:test.local", "DEV1", roomID, "m.room.message",
			"txn-"+body, content,
		)
		if errAgain != nil || !dupAgain || again != eventID {
			t.Fatalf("повтор txn-%s: id=%q dup=%t err=%v", body, again, dupAgain, errAgain)
		}

		if i == 2 {
			break
		}
	}

	result, err := service.Messages(ctx, "@alice:test.local", roomID, "", "b", 50)
	if err != nil {
		t.Fatalf("Messages() error: %v", err)
	}

	var bodies []string

	for _, event := range result.Events {
		if event.Type != "m.room.message" {
			continue
		}

		var content struct {
			Body string `json:"body"`
		}

		if unmarshalErr := json.Unmarshal(event.Content, &content); unmarshalErr != nil {
			t.Fatalf("unmarshal content: %v", unmarshalErr)
		}

		bodies = append(bodies, content.Body)
	}

	if len(bodies) != 3 || bodies[0] != "третье" || bodies[2] != "первое" {
		t.Fatalf("история в обратном порядке = %v", bodies)
	}

	// Событие читается по id.
	first := result.Events[len(result.Events)-4].EventID
	if _, err := roomStore.EventByID(ctx, roomID, first); err != nil {
		t.Fatalf("EventByID(%s) error: %v", first, err)
	}

	if _, err := roomStore.EventByID(ctx, roomID, "$missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("EventByID($missing) = %v, ожидали ErrNotFound", err)
	}

	// Членство: приглашение и вход второго пользователя.
	if _, err := service.Invite(ctx, "@alice:test.local", roomID, "@bob:test.local"); err != nil {
		t.Fatalf("Invite() error: %v", err)
	}

	if _, err := service.Join(ctx, "@bob:test.local", roomID); err != nil {
		t.Fatalf("Join() error: %v", err)
	}

	if _, _, err := service.SendMessage(ctx, "@bob:test.local", "DEV2", roomID,
		"m.room.message", "bob-1", json.RawMessage(`{"body":"от боба"}`)); err != nil {
		t.Fatalf("SendMessage(bob) error: %v", err)
	}
}
