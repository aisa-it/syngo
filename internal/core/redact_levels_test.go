package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestRedactRequiresSendLevel - m.room.redaction - обычное событие, и
// его отправка требует events["m.room.redaction"] (иначе events_default), как
// _can_send_event в Synapse. Раньше проверялся только уровень redact, и участник
// с уровнем 0 удалял свои сообщения, даже когда redaction была закрыта для него.
func TestRedactRequiresSendLevel(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")
	ctx := context.Background()

	joinUser(t, svc, roomID, bob)

	own, _, err := svc.SendMessage(ctx, bob, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"msgtype":"m.text","body":"свое"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	levels := `{"users":{"` + alice + `":100},"events":{"m.room.redaction":50},"redact":50}`
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.power_levels", "", "pl1",
		json.RawMessage(levels)); err != nil {
		t.Fatalf("power_levels: %v", err)
	}

	if _, _, err := svc.Redact(ctx, bob, "DEV", roomID, own, "", "r1"); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("redaction при events[m.room.redaction]=50: ожидали ErrNotAllowed, получили %v", err)
	}

	// Администратор удаляет чужое сообщение по уровню redact.
	if _, _, err := svc.Redact(ctx, alice, "DEV", roomID, own, "", "r2"); err != nil {
		t.Fatalf("redaction администратором: %v", err)
	}
}

// TestRedactOwnAlwaysAllowed - свое сообщение удаляется без уровня redact.
func TestRedactOwnAlwaysAllowed(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")
	ctx := context.Background()

	joinUser(t, svc, roomID, bob)

	own, _, err := svc.SendMessage(ctx, bob, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"msgtype":"m.text","body":"свое"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if _, _, err := svc.Redact(ctx, bob, "DEV", roomID, own, "", "r1"); err != nil {
		t.Fatalf("redaction своего: %v", err)
	}

	ev, err := svc.Event(ctx, bob, roomID, own)
	if err != nil {
		t.Fatalf("Event: %v", err)
	}

	if ev.RedactedBy == "" {
		t.Fatal("цель не помечена redaction")
	}
}
