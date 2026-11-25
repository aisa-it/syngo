package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

const backupKey = `{"first_message_index":0,"forwarded_count":0,"is_verified":true,"session_data":{"ciphertext":"x"}}`

// requireKind проверяет вид отказа сервиса и текст для клиента.
func requireKind(t *testing.T, err, kind error, message string) {
	t.Helper()

	var reqErr *core.RequestError
	if !errors.As(err, &reqErr) || !errors.Is(err, kind) {
		t.Fatalf("ошибка %v, ждали %v", err, kind)
	}

	if message != "" && reqErr.Message != message {
		t.Fatalf("текст %q, ждали %q", reqErr.Message, message)
	}
}

// Бэкап ключей: версии, запись только в текущую версию, чтение и удаление.
func TestKeyBackupService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc := core.NewKeyBackupService(coretest.NewMemKeyBackupStore())
	auth := json.RawMessage(`{"public_key":"abc"}`)

	if _, err := svc.Version(ctx, alice, ""); !errors.Is(err, core.ErrRoomKeysNotFound) {
		t.Fatalf("без бэкапа: %v", err)
	}

	_, err := svc.CreateVersion(ctx, alice, "", auth)
	requireKind(t, err, core.ErrMissingParam, "algorithm and auth_data are required")

	_, err = svc.CreateVersion(ctx, alice, "m.megolm_backup.v1", json.RawMessage(`null`))
	requireKind(t, err, core.ErrMissingParam, "")

	v1, err := svc.CreateVersion(ctx, alice, "m.megolm_backup.v1", auth)
	if err != nil || v1 != "1" {
		t.Fatalf("создание: %q %v", v1, err)
	}

	err = svc.UpdateVersion(ctx, alice, v1, core.VersionUpdate{Algorithm: "other", AuthData: auth})
	requireKind(t, err, core.ErrInvalidParam, "version and algorithm cannot be changed")

	err = svc.UpdateVersion(ctx, alice, v1, core.VersionUpdate{Version: "7", AuthData: auth})
	requireKind(t, err, core.ErrInvalidParam, "")

	err = svc.UpdateVersion(ctx, alice, v1, core.VersionUpdate{})
	requireKind(t, err, core.ErrMissingParam, "auth_data is required")

	if err := svc.UpdateVersion(ctx, alice, "9", core.VersionUpdate{AuthData: auth}); !errors.Is(err, core.ErrRoomKeysNotFound) {
		t.Fatalf("обновление неизвестной версии: %v", err)
	}

	newAuth := json.RawMessage(`{"public_key":"def"}`)
	if err := svc.UpdateVersion(ctx, alice, v1, core.VersionUpdate{AuthData: newAuth, Version: v1}); err != nil {
		t.Fatalf("обновление: %v", err)
	}

	got, err := svc.Version(ctx, alice, v1)
	if err != nil || string(got.AuthData) != string(newAuth) || got.Algorithm != "m.megolm_backup.v1" {
		t.Fatalf("версия после обновления: %+v %v", got, err)
	}

	keys := core.RoomKeys{"!r:test.local": {"s1": json.RawMessage(backupKey)}}

	_, err = svc.PutKeys(ctx, alice, v1, core.RoomKeys{"!r:test.local": {"s1": json.RawMessage(`{}`)}})
	requireKind(t, err, core.ErrMalformed, "Invalid key data")

	_, err = svc.PutKeys(ctx, alice, "", keys)
	requireKind(t, err, core.ErrMissingParam, "version is required")

	stat, err := svc.PutKeys(ctx, alice, v1, keys)
	if err != nil || stat.Count != 1 {
		t.Fatalf("запись ключей: %+v %v", stat, err)
	}

	v2, err := svc.CreateVersion(ctx, alice, "m.megolm_backup.v1", auth)
	if err != nil {
		t.Fatal(err)
	}

	// Старая версия существует, но не текущая: 403 с номером текущей.
	_, err = svc.PutKeys(ctx, alice, v1, keys)

	var wrong *core.WrongBackupVersionError
	if !errors.As(err, &wrong) || wrong.Current != v2 || wrong.Error() == "" {
		t.Fatalf("запись в старую версию: %v", err)
	}

	_, err = svc.PutKeys(ctx, alice, "42", keys)
	requireKind(t, err, core.ErrNotFound, "Unknown backup version")

	if _, err := svc.PutKeys(ctx, alice, v2, keys); err != nil {
		t.Fatal(err)
	}

	// Без ?version= - текущая версия (совместимость со старыми клиентами).
	all, err := svc.Keys(ctx, alice, "", "", "")
	if err != nil || len(all["!r:test.local"]) != 1 {
		t.Fatalf("ключи текущей версии: %v %v", all, err)
	}

	if _, err := svc.Keys(ctx, alice, "42", "", ""); !errors.Is(err, core.ErrRoomKeysNotFound) {
		t.Fatalf("ключи неизвестной версии: %v", err)
	}

	if _, err := svc.Keys(ctx, bob, "", "", ""); !errors.Is(err, core.ErrRoomKeysNotFound) {
		t.Fatalf("ключи без бэкапа: %v", err)
	}

	stat, err = svc.DeleteKeys(ctx, alice, v2, "!r:test.local", "s1")
	if err != nil || stat.Count != 0 {
		t.Fatalf("удаление ключей: %+v %v", stat, err)
	}

	if _, err := svc.DeleteKeys(ctx, alice, "42", "", ""); !errors.Is(err, core.ErrRoomKeysNotFound) {
		t.Fatalf("удаление ключей неизвестной версии: %v", err)
	}

	if err := svc.DeleteVersion(ctx, alice, v2); err != nil {
		t.Fatal(err)
	}

	current, err := svc.Version(ctx, alice, "")
	if err != nil || current.Version != v1 {
		t.Fatalf("после удаления текущей: %+v %v", current, err)
	}
}

func TestValidBackupKey(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]bool{
		backupKey: true,
		`{"first_message_index":0,"forwarded_count":0,"is_verified":true,"session_data":"x"}`: false,
		`{"forwarded_count":0,"is_verified":true,"session_data":{}}`:                          false,
		`[]`: false,
	} {
		if got := core.ValidBackupKey(json.RawMessage(raw)); got != want {
			t.Errorf("ValidBackupKey(%s) = %v", raw, got)
		}
	}
}
