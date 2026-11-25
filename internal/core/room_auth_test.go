package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// Локальные действия проходят auth rules по текущему состоянию комнаты:
// раньше join/invite/leave писались мимо проверок.

const carol = "@carol:test.local"

func createPreset(t *testing.T, svc *core.RoomService, preset string) string {
	t.Helper()

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{Creator: alice, Name: "r", Preset: preset})
	if err != nil {
		t.Fatalf("CreateRoom(%s): %v", preset, err)
	}

	return roomID
}

func TestLocalMembershipFollowsAuthRules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRoomService(t)
	public := createPreset(t, svc, "public_chat")

	// Публичная комната пускает без приглашения.
	if _, err := svc.Join(ctx, bob, public); err != nil {
		t.Fatalf("join public: %v", err)
	}

	// Забаненный не входит в публичную комнату и не снимает бан своим leave.
	if _, _, err := svc.SendMembership(ctx, alice, "DEV", public, bob, "ban", "", "ban-1"); err != nil {
		t.Fatalf("ban: %v", err)
	}

	if _, err := svc.Join(ctx, bob, public); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("join забаненного: %v, ожидали ErrForbidden", err)
	}

	// Приглашение не снимает бан.
	if _, err := svc.Invite(ctx, alice, public, bob); err == nil {
		t.Fatal("invite забаненного принят")
	}

	// Private (invite) не пускает без приглашения, после приглашения - пускает.
	private := createPreset(t, svc, "private_chat")

	if _, err := svc.Join(ctx, carol, private); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("join private без приглашения: %v", err)
	}

	if _, err := svc.Invite(ctx, alice, private, carol); err != nil {
		t.Fatalf("invite: %v", err)
	}

	if _, err := svc.Join(ctx, carol, private); err != nil {
		t.Fatalf("join по приглашению: %v", err)
	}

	// Повторное «приглашение» вошедшего не сбрасывает его членство.
	if _, err := svc.Invite(ctx, carol, private, alice); err == nil {
		t.Fatal("приглашение уже вошедшего принято")
	}
}

func TestStateGuards(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRoomService(t)
	roomID := createPreset(t, svc, "private_chat")

	// m.room.create после создания не меняется даже создателем.
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.create", "", "c1",
		json.RawMessage(`{"creator":"@alice:test.local","room_version":"10"}`)); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("PUT m.room.create: %v, ожидали ErrNotAllowed", err)
	}

	// power_levels, с которыми не строится состояние, отклоняются до записи.
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.power_levels", "", "p1",
		json.RawMessage(`{"users":{"@alice:test.local":"100"}}`)); err == nil {
		t.Fatal("power_levels со строковым уровнем принят")
	}

	// Комната после отказа работает.
	if _, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m1", json.RawMessage(`{"body":"ok"}`)); err != nil {
		t.Fatalf("сообщение после отказа: %v", err)
	}
}

func TestCreateRoomPresetsAndValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRoomService(t)

	stateOf := func(roomID, eventType string) map[string]any {
		for _, ev := range store.Events() {
			if ev.RoomID == roomID && ev.Type == eventType {
				var out map[string]any
				_ = json.Unmarshal(ev.Content, &out)

				return out
			}
		}

		return nil
	}

	public := createPreset(t, svc, "public_chat")
	if stateOf(public, "m.room.join_rules")["join_rule"] != "public" ||
		stateOf(public, "m.room.guest_access")["guest_access"] != "forbidden" {
		t.Fatal("public_chat: неверные join_rules/guest_access")
	}

	// initial_state перекрывает пресет; override сливается с уровнями.
	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: alice, Preset: "trusted_private_chat", Invite: []string{bob},
		InitialState:       []core.StateEventRequest{{Type: "m.room.history_visibility", Content: json.RawMessage(`{"history_visibility":"invited"}`)}},
		PowerLevelOverride: map[string]any{"events": map[string]any{"m.room.encryption": float64(100)}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if stateOf(roomID, "m.room.history_visibility")["history_visibility"] != "invited" {
		t.Fatal("initial_state не перекрыл пресет")
	}

	pl := stateOf(roomID, "m.room.power_levels")
	users, _ := pl["users"].(map[string]any)
	events, _ := pl["events"].(map[string]any)

	if users[bob] != float64(100) || events["m.room.encryption"] != float64(100) || events["m.room.name"] != float64(50) {
		t.Fatalf("power_levels: %v", pl)
	}

	for _, bad := range []core.CreateRoomRequest{
		{Creator: alice, InitialState: []core.StateEventRequest{{Type: "m.room.member", StateKey: bob, Content: json.RawMessage(`{"membership":"join"}`)}}},
		{Creator: alice, InitialState: []core.StateEventRequest{{Type: "m.room.create", Content: json.RawMessage(`{}`)}}},
		{Creator: alice, Invite: []string{"not-a-user-id"}},
	} {
		if _, err := svc.CreateRoom(ctx, bad); !errors.Is(err, core.ErrBadJSON) {
			t.Fatalf("CreateRoom(%+v): %v, ожидали ErrBadJSON", bad, err)
		}
	}
}
