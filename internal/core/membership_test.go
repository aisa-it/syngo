package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// /joined_rooms: только join, пустой список вместо nil.
func TestMembershipServiceJoinedRooms(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rooms, store := newRoomService(t)
	svc := core.NewMembershipService(store)

	joined, err := svc.JoinedRooms(ctx, bob)
	if err != nil || joined == nil || len(joined) != 0 {
		t.Fatalf("без комнат: %v %v", joined, err)
	}

	createRoom(t, rooms)
	second := createRoom(t, rooms)

	if _, err := rooms.Invite(ctx, alice, second, bob); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	joined, err = svc.JoinedRooms(ctx, alice)
	if err != nil || len(joined) != 2 || joined[0] > joined[1] {
		t.Fatalf("комнаты alice: %v %v", joined, err)
	}

	joined, err = svc.JoinedRooms(ctx, bob)
	if err != nil || len(joined) != 0 {
		t.Fatalf("приглашение не join: %v %v", joined, err)
	}
}

// Деактивация: комнаты с join и invite, без leave; сбой чтения - ошибка.
func TestMembershipServiceActiveRooms(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rooms, store := newRoomService(t)
	svc := core.NewMembershipService(store)

	joined := createRoom(t, rooms)
	invited := createRoom(t, rooms)
	left := createRoom(t, rooms)

	if _, err := rooms.Invite(ctx, alice, invited, bob); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	for _, roomID := range []string{joined, left} {
		if _, err := rooms.Invite(ctx, alice, roomID, bob); err != nil {
			t.Fatalf("Invite: %v", err)
		}

		if _, err := rooms.Join(ctx, bob, roomID); err != nil {
			t.Fatalf("Join: %v", err)
		}
	}

	if _, err := rooms.Leave(ctx, bob, left); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	active, err := svc.ActiveRooms(ctx, bob)
	if err != nil || len(active) != 2 {
		t.Fatalf("активные комнаты: %v %v", active, err)
	}

	for _, roomID := range active {
		if roomID == left {
			t.Fatalf("покинутая комната в списке: %v", active)
		}
	}

	if _, err := core.NewMembershipService(failingMemberships{}).ActiveRooms(ctx, bob); err == nil {
		t.Fatal("сбой хранилища не вернул ошибку")
	}
}

// memProfiles - профили для теста рассылки.
type memProfiles struct {
	core.ProfileStore

	mu       sync.Mutex
	profiles map[string]core.Profile
}

func (m *memProfiles) Profile(_ context.Context, userID string) (core.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	profile, ok := m.profiles[userID]
	if !ok {
		return core.Profile{}, core.ErrNotFound
	}

	return profile, nil
}

// failingMemberships - сбой чтения комнат пользователя.
type failingMemberships struct{ core.MembershipStore }

func (failingMemberships) MembershipRooms(context.Context, string) ([]core.RoomMembership, error) {
	return nil, errors.New("db down")
}

// Рассылка профиля: m.room.member во все комнаты с join, под замком
// пользователя, события - в OnEvent.
func TestProfilePropagator(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rooms, store := newRoomService(t)
	first := createRoom(t, rooms)
	second := createRoom(t, rooms)

	profiles := &memProfiles{profiles: map[string]core.Profile{
		alice: {UserID: alice, DisplayName: "Алиса", AvatarURL: "mxc://test.local/a"},
	}}

	var (
		sent   []string
		locked []string
	)

	propagator := core.NewProfilePropagator(profiles, store, rooms, core.ProfilePropagationOptions{
		Lock: func(userID string) func() {
			locked = append(locked, userID)

			return func() {}
		},
		OnEvent: func(_ context.Context, roomID, _ string) { sent = append(sent, roomID) },
	})

	propagator.Propagate(ctx, alice)

	if len(sent) != 2 || len(locked) != 1 || locked[0] != alice {
		t.Fatalf("разослано %v, замки %v", sent, locked)
	}

	for _, roomID := range []string{first, second} {
		event, err := rooms.StateEvent(ctx, alice, roomID, "m.room.member", alice)
		if err != nil {
			t.Fatalf("StateEvent: %v", err)
		}

		var content map[string]string
		if err := json.Unmarshal(event.Content, &content); err != nil || content["displayname"] != "Алиса" ||
			content["avatar_url"] != "mxc://test.local/a" || content["membership"] != "join" {
			t.Fatalf("m.room.member в %s: %s", roomID, event.Content)
		}
	}

	// Пользователь без профиля - ничего не рассылается; без опций - не падает.
	sent = nil

	core.NewProfilePropagator(profiles, store, rooms, core.ProfilePropagationOptions{}).Propagate(ctx, bob)

	propagator.Propagate(ctx, bob)

	if len(sent) != 0 {
		t.Fatalf("без профиля разослано %v", sent)
	}

	// Заблокированная комната пропускается, остальные получают событие. Профиль
	// меняется: тот же профиль повторно не рассылается - нового события нет.
	store.SetBlocked(first, true)
	profiles.profiles[alice] = core.Profile{UserID: alice, DisplayName: "Алиса 2", AvatarURL: "mxc://test.local/a"}

	var skipped []string

	sent = nil

	core.NewProfilePropagator(profiles, store, rooms, core.ProfilePropagationOptions{
		OnEvent: func(_ context.Context, roomID, _ string) { sent = append(sent, roomID) },
		Skip:    func(_ context.Context, _ string, _ error, args ...any) { skipped = append(skipped, args[1].(string)) },
	}).Propagate(ctx, alice)

	if len(sent) != 1 || sent[0] != second || len(skipped) != 1 || skipped[0] != first {
		t.Fatalf("с блокировкой: разослано %v, пропущено %v", sent, skipped)
	}

	// Сбой чтения комнат - предупреждение и выход.
	var warned error

	core.NewProfilePropagator(profiles, failingMemberships{}, rooms, core.ProfilePropagationOptions{
		Warn: func(_ context.Context, _ string, err error, _ ...any) { warned = err },
	}).Propagate(ctx, alice)

	if warned == nil {
		t.Fatal("сбой чтения комнат без предупреждения")
	}
}
