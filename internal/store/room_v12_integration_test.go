//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestRoomIDFormatV12 - миграция 0036: CHECK rooms_room_id_format
// пропускает room_id комнаты v12 («!» и 43 символа хеша m.room.create без домена)
// и по-прежнему отвергает прочий мусор; комната v12, созданная сервисом,
// сохраняется и читается. Раньше CHECK требовал «!...:сервер».
func TestRoomIDFormatV12(t *testing.T) {
	roomStore := freshRoomStore(t)
	ctx := context.Background()
	pool := openDB(t).Pool()

	for roomID, ok := range map[string]bool{
		"!" + strings.Repeat("a", 42) + "_": true,
		"!opaque:test.local":                true,
		"!short":                            false,
		"!" + strings.Repeat("a", 43) + "+": false,
		"opaque:test.local":                 false,
	} {
		_, err := pool.Exec(ctx, `INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')`, roomID)
		if ok && err != nil {
			t.Fatalf("%s: %v", roomID, err)
		}

		if !ok && (err == nil || !strings.Contains(err.Error(), "rooms_room_id_format")) {
			t.Fatalf("%s: ожидали отказ CHECK, получили %v", roomID, err)
		}
	}

	service := core.NewRoomService(roomStore, "test.local")

	roomID, err := service.CreateRoom(ctx, core.CreateRoomRequest{Creator: "@alice:test.local", RoomVersion: 12})
	if err != nil {
		t.Fatalf("CreateRoom v12: %v", err)
	}

	room, err := roomStore.RoomByID(ctx, roomID)
	if err != nil || room.RoomVersion != 12 || strings.Contains(room.RoomID, ":") {
		t.Fatalf("RoomByID v12: %+v, %v", room, err)
	}

	if _, _, err := service.SendMessage(ctx, "@alice:test.local", "D", roomID, "m.room.message", "t1",
		[]byte(`{"body":"x"}`)); err != nil {
		t.Fatalf("сообщение в v12: %v", err)
	}
}
