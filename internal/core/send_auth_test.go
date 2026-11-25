package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestSendMessageRequiresPowerLevel — обычные события тоже подчиняются power
// levels: если events_default выше уровня участника, /send обязан вернуть отказ,
// а не писать событие в комнату.
func TestSendMessageRequiresPowerLevel(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)

	ctx := context.Background()

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, Invite: []string{bob}})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("Join bob: %v", err)
	}

	pl := `{"users":{"@alice:test.local":100},"events_default":50,"state_default":50}`
	if _, _, err := svc.SendState(ctx, alice, "DEV1", roomID, "m.room.power_levels", "", "txn-pl", []byte(pl)); err != nil {
		t.Fatalf("SendState power_levels: %v", err)
	}

	_, _, err = svc.SendMessage(ctx, bob, "DEV2", roomID, "m.room.message", "txn-1",
		[]byte(`{"msgtype":"m.text","body":"hi"}`))
	if !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("ожидали ErrNotAllowed, получили %v", err)
	}
}
