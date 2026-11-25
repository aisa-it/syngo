package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestBlockedRoomRejectsWrites - заблокированная комната не принимает от
// пользователей сообщения, состояние, приглашения, join и redaction; выход и kick
// разрешены, admin-обход пишет служебные события, разблокировка возвращает запись.
// Раньше блокировка проверялась только в HTTP /join.
func TestBlockedRoomRejectsWrites(t *testing.T) {
	t.Parallel()

	svc, mem := newRoomService(t)
	ctx := context.Background()

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, Invite: []string{bob}})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("Join bob: %v", err)
	}

	msgID, _, err := svc.SendMessage(ctx, alice, "DEV1", roomID, "m.room.message", "txn-before",
		[]byte(`{"msgtype":"m.text","body":"before"}`))
	if err != nil {
		t.Fatalf("SendMessage до блокировки: %v", err)
	}

	mem.SetBlocked(roomID, true)

	checks := map[string]error{}

	_, _, checks["send"] = svc.SendMessage(ctx, alice, "DEV1", roomID, "m.room.message", "txn-1",
		[]byte(`{"msgtype":"m.text","body":"hi"}`))
	_, _, checks["state"] = svc.SendState(ctx, alice, "DEV1", roomID, "m.room.topic", "", "",
		[]byte(`{"topic":"x"}`))
	_, checks["invite"] = svc.Invite(ctx, alice, roomID, carol)
	_, checks["join"] = svc.Join(ctx, carol, roomID)
	_, _, checks["redact"] = svc.Redact(ctx, alice, "DEV1", roomID, msgID, "", "txn-r")
	_, checks["upgrade"] = svc.UpgradeRoom(ctx, alice, "DEV1", roomID, core.DefaultRoomVersion)

	for name, err := range checks {
		if !errors.Is(err, core.ErrRoomBlocked) || !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%s в заблокированной комнате: %v, ожидали ErrRoomBlocked", name, err)
		}
	}

	// Служебные события администратора (tombstone перед purge) проходят.
	if _, _, err := svc.SendState(core.WithRoomBlockBypass(ctx), alice, "ADMIN", roomID,
		"m.room.tombstone", "", "", []byte(`{"body":"closed","replacement_room":""}`)); err != nil {
		t.Fatalf("tombstone администратора: %v", err)
	}

	// Выйти из заблокированной комнаты можно.
	if _, err := svc.Leave(ctx, bob, roomID); err != nil {
		t.Fatalf("Leave из заблокированной комнаты: %v", err)
	}

	mem.SetBlocked(roomID, false)

	if _, _, err := svc.SendMessage(ctx, alice, "DEV1", roomID, "m.room.message", "txn-2",
		[]byte(`{"msgtype":"m.text","body":"again"}`)); err != nil {
		t.Fatalf("SendMessage после разблокировки: %v", err)
	}
}
