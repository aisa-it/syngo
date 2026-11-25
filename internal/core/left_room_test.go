package core_test

import (
	"context"
	"encoding/json"
	"testing"
)

// TestLeftRoomReadsUntilLeave - вышедший участник читает состояние, участников и
// историю комнаты на момент выхода (SPEC-216, как в Synapse,
// Complement TestLeftRoomFixture).
func TestLeftRoomReadsUntilLeave(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("join: %v", err)
	}

	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.name", "", "", json.RawMessage(`{"name":"before"}`)); err != nil {
		t.Fatalf("name: %v", err)
	}

	if _, err := svc.Leave(ctx, bob, roomID); err != nil {
		t.Fatalf("leave: %v", err)
	}

	leavePos := store.Events()[len(store.Events())-1].StreamOrdering

	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.name", "", "", json.RawMessage(`{"name":"after"}`)); err != nil {
		t.Fatalf("name after: %v", err)
	}

	name, err := svc.StateEvent(ctx, bob, roomID, "m.room.name", "")
	if err != nil || string(name.Content) != `{"name":"before"}` {
		t.Fatalf("имя для вышедшего: %s, %v", name.Content, err)
	}

	if name, _ := svc.StateEvent(ctx, alice, roomID, "m.room.name", ""); string(name.Content) != `{"name":"after"}` {
		t.Fatalf("имя для участника: %s", name.Content)
	}

	members, _, err := svc.MembersPage(ctx, bob, roomID, "", 100)
	if err != nil || len(members) != 2 {
		t.Fatalf("участники для вышедшего: %d, %v", len(members), err)
	}

	back, err := svc.Messages(ctx, bob, roomID, "", "b", 100)
	if err != nil || len(back.Events) == 0 || back.Events[0].StreamOrdering != leavePos {
		t.Fatalf("история назад для вышедшего: %+v, %v", back.Events, err)
	}

	forward, err := svc.Messages(ctx, bob, roomID, "", "f", 100)
	if err != nil {
		t.Fatalf("история вперед: %v", err)
	}

	for _, ev := range forward.Events {
		if ev.StreamOrdering > leavePos {
			t.Fatalf("вперед после выхода: событие %d > %d", ev.StreamOrdering, leavePos)
		}
	}

	if _, _, err := svc.MembersAt(ctx, alice, roomID, leavePos-1); err != nil {
		t.Fatalf("MembersAt: %v", err)
	}
}
