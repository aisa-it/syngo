package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

type adminEnv struct {
	svc    *core.AdminService
	store  *coretest.MemAdminStore
	rooms  *core.RoomService
	events []string
	warns  []string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()

	rooms, roomStore := newRoomService(t)
	env := &adminEnv{store: coretest.NewMemAdminStore(), rooms: rooms}
	env.svc = core.NewAdminService(env.store, roomStore, rooms, core.AdminOptions{
		ServerName: "test.local",
		OnEvent:    func(_ context.Context, _, eventID string) { env.events = append(env.events, eventID) },
		Warn: func(_ context.Context, msg string, _ error, _ ...any) {
			env.warns = append(env.warns, msg)
		},
	})

	env.store.AddUser(core.User{UserID: alice, Localpart: "alice", IsAdmin: true})
	env.store.AddUser(core.User{UserID: bob, Localpart: "bob"})

	return env
}

// lastAudit - action и result последней записи аудита.
func lastAudit(t *testing.T, env *adminEnv) (action, result, reason string) {
	t.Helper()

	log := env.store.AuditLog()
	if len(log) == 0 {
		t.Fatal("аудит пуст")
	}

	var details struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}

	row := log[len(log)-1]
	if err := json.Unmarshal(row.Details, &details); err != nil {
		t.Fatal(err)
	}

	return row.Action, details.Result, details.Reason
}

// Пользователи: самоблокировка, guard последнего админа, аудит отказов.
func TestAdminUserActions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newAdminEnv(t)

	if err := env.svc.CheckUserAction(ctx, alice, alice, "deactivate"); !errors.Is(err, core.ErrAdminSelf) {
		t.Fatalf("самоблокировка: %v", err)
	}

	if action, result, reason := lastAudit(t, env); action != "deactivate" || result != core.AuditResultDenied || reason != "self" {
		t.Fatalf("аудит самоблокировки: %s %s %s", action, result, reason)
	}

	if err := env.svc.CheckUserAction(ctx, alice, alice, "password"); err != nil {
		t.Fatalf("смена своего пароля: %v", err)
	}

	if err := env.svc.SetUserAdmin(ctx, bob, alice, false); !errors.Is(err, core.ErrLastAdmin) {
		t.Fatalf("последний админ: %v", err)
	}

	if _, _, reason := lastAudit(t, env); reason != "last_admin" {
		t.Fatalf("аудит последнего админа: %s", reason)
	}

	if err := env.svc.SetUserDeactivated(ctx, alice, "@nobody:test.local", true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("неизвестный пользователь: %v", err)
	}

	if _, result, _ := lastAudit(t, env); result != core.AuditResultNotFound {
		t.Fatalf("аудит неизвестного: %s", result)
	}

	if err := env.svc.SetUserAdmin(ctx, alice, bob, true); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.SetUserDeactivated(ctx, alice, bob, true); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.SetUserDeactivated(ctx, alice, bob, false); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.SetUserPasswordHash(ctx, alice, bob, "hash"); err != nil || env.store.User("bob").PasswordHash != "hash" {
		t.Fatalf("смена пароля: %v", err)
	}

	if action, result, _ := lastAudit(t, env); action != "password" || result != core.AuditResultOK {
		t.Fatalf("аудит смены пароля: %s %s", action, result)
	}

	// Классификация отказов в аудите.
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{core.ErrForbidden, "forbidden"},
		{core.ErrWeakPassword, "invalid_request"},
		{errors.New("boom"), "internal"},
	} {
		env.svc.AuditFailure(ctx, alice, "password", bob, tc.err)

		if _, _, reason := lastAudit(t, env); reason != tc.reason {
			t.Fatalf("%v: reason %s, ждали %s", tc.err, reason, tc.reason)
		}
	}

	if env.store.Purges() == 0 {
		t.Fatal("аудит не чистится")
	}
}

// Списки admin API: курсоры, total и ограничение limit.
func TestAdminLists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newAdminEnv(t)

	page, err := env.svc.Users(ctx, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Localpart != "alice" || page.Total != 2 || page.Next == "" {
		t.Fatalf("первая страница: %+v %v", page, err)
	}

	page, err = env.svc.Users(ctx, page.Next, 500)
	if err != nil || len(page.Items) != 1 || page.Items[0].Localpart != "bob" {
		t.Fatalf("вторая страница: %+v %v", page, err)
	}

	env.store.AddRoom(core.AdminRoomRow{RoomID: "!a:test.local"})
	env.store.AddRoom(core.AdminRoomRow{RoomID: "!b:test.local"})

	rooms, err := env.svc.Rooms(ctx, "!a:test.local", 10)
	if err != nil || len(rooms.Items) != 1 || rooms.Next != "!b:test.local" || rooms.Total != 2 {
		t.Fatalf("комнаты: %+v %v", rooms, err)
	}

	if err := env.svc.RecordAudit(ctx, alice, "login", alice, nil); err != nil {
		t.Fatal(err)
	}

	rows, total, next, err := env.svc.Audit(ctx, 0, 10)
	if err != nil || len(rows) != 1 || total != 1 || next != rows[0].ID {
		t.Fatalf("аудит: %v %d %d %v", rows, total, next, err)
	}

	if _, _, next, _ := env.svc.Audit(ctx, 5, 10); next != 5 {
		t.Fatalf("пустая страница аудита: next %d", next)
	}

	// Сбой подсчета total - не ошибка страницы, а предупреждение.
	env.store.CountErr = errors.New("count")

	if _, err := env.svc.Users(ctx, "", 10); err != nil {
		t.Fatal(err)
	}

	if _, err := env.svc.Rooms(ctx, "", 10); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := env.svc.Audit(ctx, 0, 10); err != nil {
		t.Fatal(err)
	}

	if len(env.warns) != 3 {
		t.Fatalf("предупреждения: %v", env.warns)
	}

	env.store.Err = errors.New("boom")

	if _, err := env.svc.Users(ctx, "", 10); !errors.Is(err, env.store.Err) {
		t.Fatalf("сбой списка: %v", err)
	}

	if _, err := env.svc.Rooms(ctx, "", 10); !errors.Is(err, env.store.Err) {
		t.Fatalf("сбой списка: %v", err)
	}

	if _, _, _, err := env.svc.Audit(ctx, 0, 10); !errors.Is(err, env.store.Err) {
		t.Fatalf("сбой аудита: %v", err)
	}
}

// Комнаты: блокировка, purge с tombstone и leave локальным участникам, выход.
func TestAdminRoomsAndLogout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newAdminEnv(t)

	roomID := createRoom(t, env.rooms)

	if _, err := env.rooms.Invite(ctx, alice, roomID, bob); err != nil {
		t.Fatal(err)
	}

	if _, err := env.rooms.Join(ctx, bob, roomID); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.SetRoomBlock(ctx, alice, roomID, true); err != nil {
		t.Fatal(err)
	}

	info, err := env.svc.RoomBlockInfo(ctx, roomID)
	if err != nil || !info.Blocked || info.BlockedBy != alice {
		t.Fatalf("блокировка: %+v %v", info, err)
	}

	if err := env.svc.SetRoomBlock(ctx, alice, roomID, false); err != nil {
		t.Fatal(err)
	}

	if action, _, _ := lastAudit(t, env); action != "room_unblock" {
		t.Fatalf("аудит разблокировки: %s", action)
	}

	if _, err := env.svc.PurgeRoom(ctx, alice, "!missing:test.local", false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("purge неизвестной комнаты: %v", err)
	}

	if action, result, _ := lastAudit(t, env); action != "room_purge" || result != core.AuditResultNotFound {
		t.Fatalf("аудит purge неизвестной: %s %s", action, result)
	}

	// Tombstone от создателя и leave двум локальным участникам.
	notified, err := env.svc.PurgeRoom(ctx, alice, roomID, true)
	if err != nil || notified != 3 || len(env.events) != 3 {
		t.Fatalf("purge: %d %v %v", notified, env.events, err)
	}

	if purged := env.store.Purged(); len(purged) != 1 || purged[0] != roomID {
		t.Fatalf("удаленные комнаты: %v", purged)
	}

	env.store.AddToken("secret", alice)

	if err := env.svc.Logout(ctx, alice, "secret"); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.Logout(ctx, alice, "secret"); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("повторный выход: %v", err)
	}

	env.store.Err = errors.New("boom")

	if err := env.svc.SetRoomBlock(ctx, alice, roomID, true); !errors.Is(err, env.store.Err) {
		t.Fatalf("сбой блокировки: %v", err)
	}

	if _, result, reason := lastAudit(t, env); result != core.AuditResultError || reason != "internal" {
		t.Fatalf("аудит сбоя: %s %s", result, reason)
	}

	// Без сервиса комнат purge никого не уведомляет.
	_, roomStore := newRoomService(t)
	quiet := core.NewAdminService(coretest.NewMemAdminStore(), roomStore, nil, core.AdminOptions{})

	if _, err := quiet.PurgeRoom(ctx, alice, "!missing:test.local", false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("purge без комнаты: %v", err)
	}
}
