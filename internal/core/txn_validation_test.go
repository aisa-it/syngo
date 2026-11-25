package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

func createPublicRoom(t *testing.T, svc *core.RoomService) string {
	t.Helper()

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{Creator: alice, Name: "r", IsPublic: true})
	if err != nil {
		t.Fatal(err)
	}

	return roomID
}

func topicOf(t *testing.T, svc *core.RoomService, roomID string) string {
	t.Helper()

	ev, err := svc.StateEvent(context.Background(), alice, roomID, "m.room.topic", "")
	if err != nil {
		t.Fatalf("topic: %v", err)
	}

	var body struct {
		Topic string `json:"topic"`
	}

	_ = json.Unmarshal(ev.Content, &body)

	return body.Topic
}

// TestStateAndModerationAreNotDeduplicatedByBody — PUT /state и модерация не
// дедуплицируются по телу: возврат к прежнему значению и повторный
// kick - новые события.
func TestStateAndModerationAreNotDeduplicatedByBody(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRoomService(t)
	roomID := createPublicRoom(t, svc)

	for _, topic := range []string{"A", "B", "A"} {
		if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.topic", "", "",
			json.RawMessage(`{"topic":"`+topic+`"}`)); err != nil {
			t.Fatal(err)
		}
	}

	if got := topicOf(t, svc, roomID); got != "A" {
		t.Fatalf("topic после A -> B -> A = %q", got)
	}

	for round := 0; round < 2; round++ {
		if _, err := svc.Join(ctx, bob, roomID); err != nil {
			t.Fatalf("раунд %d: join: %v", round, err)
		}

		if _, dup, err := svc.SendMembership(ctx, alice, "DEV", roomID, bob, "leave", "", ""); err != nil || dup {
			t.Fatalf("раунд %d: kick: dup=%v err=%v", round, dup, err)
		}
	}
}

// TestSendTxnScopedByRoom — тот же txnId в другой комнате - другое событие.
func TestSendTxnScopedByRoom(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRoomService(t)
	r1, r2 := createRoom(t, svc), createRoom(t, svc)

	e1, _, err := svc.SendMessage(ctx, alice, "DEV", r1, "m.room.message", "same", json.RawMessage(`{"body":"1"}`))
	if err != nil {
		t.Fatal(err)
	}

	e2, dup, err := svc.SendMessage(ctx, alice, "DEV", r2, "m.room.message", "same", json.RawMessage(`{"body":"2"}`))
	if err != nil || dup || e1 == e2 {
		t.Fatalf("txnId в другой комнате: e1=%s e2=%s dup=%v err=%v", e1, e2, dup, err)
	}
}
