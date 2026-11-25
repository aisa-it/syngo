//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// freshProfileStore поднимает схему с нуля и возвращает хранилище профилей.
func freshProfileStore(t *testing.T) *store.ProfileStore {
	t.Helper()

	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	return store.NewProfileStore(openDB(t))
}

// TestProfileStoreDeviceDeletion — удаление устройства отзывает токены и стирает ключи.
func TestProfileStoreDeviceDeletion(t *testing.T) {
	profiles := freshProfileStore(t)
	ctx := context.Background()

	const (
		userID   = "@alice:test.local"
		deviceID = "DEV1"
	)

	db := openDB(t)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, 'alice', 'x')`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO devices (user_id, device_id, display_name) VALUES ($1, $2, 'phone')`, userID, deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO access_tokens (token_hash, user_id, device_id) VALUES ($1, $2, $3)`,
		core.HashToken("syt_test"), userID, deviceID); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	keys := store.NewKeysStore(db)
	if err := keys.UpsertDeviceKeys(ctx, userID, deviceID, json.RawMessage(`{"device_id":"DEV1"}`)); err != nil {
		t.Fatalf("upsert keys: %v", err)
	}

	if err := keys.UploadOneTimeKeys(ctx, userID, deviceID, map[string]json.RawMessage{
		"signed_curve25519:x": json.RawMessage(`{"key":"x"}`),
	}); err != nil {
		t.Fatalf("upload otk: %v", err)
	}

	// Профиль и переименование устройства.
	profile, err := profiles.Profile(ctx, userID)
	if err != nil {
		t.Fatalf("Profile() error: %v", err)
	}

	if profile.UserID != userID {
		t.Fatalf("profile = %+v", profile)
	}

	if err := profiles.UpdateDisplayName(ctx, userID, "Alice"); err != nil {
		t.Fatalf("UpdateDisplayName() error: %v", err)
	}

	if err := profiles.UpdateDeviceDisplayName(ctx, userID, deviceID, "workstation"); err != nil {
		t.Fatalf("UpdateDeviceDisplayName() error: %v", err)
	}

	device, err := profiles.Device(ctx, userID, deviceID)
	if err != nil {
		t.Fatalf("Device() error: %v", err)
	}

	if device.DisplayName != "workstation" {
		t.Fatalf("display_name = %q", device.DisplayName)
	}

	// Удаление устройства: токены и ключи должны исчезнуть.
	if err := profiles.DeleteDevice(ctx, userID, deviceID); err != nil {
		t.Fatalf("DeleteDevice() error: %v", err)
	}

	for _, probe := range []struct {
		name  string
		query string
	}{
		{"access_tokens", `SELECT count(*) FROM access_tokens WHERE user_id = $1 AND device_id = $2`},
		{"device_keys", `SELECT count(*) FROM device_keys WHERE user_id = $1 AND device_id = $2`},
		{"one_time_keys", `SELECT count(*) FROM one_time_keys WHERE user_id = $1 AND device_id = $2`},
	} {
		var count int

		if err := db.Pool().QueryRow(ctx, probe.query, userID, deviceID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", probe.name, err)
		}

		if count != 0 {
			t.Fatalf("%s: осталось %d записей после удаления устройства", probe.name, count)
		}
	}

	// Профиль сохранился, устройство — нет.
	if _, err := profiles.Profile(ctx, userID); err != nil {
		t.Fatalf("Profile() после удаления устройства: %v", err)
	}

	if _, err := profiles.Device(ctx, userID, deviceID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Device() = %v, ожидали ErrNotFound", err)
	}

	// Приватность: без общей комнаты профиль другого пользователя не считается доступным.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ('@bob:test.local', 'bob', 'x')`); err != nil {
		t.Fatalf("insert bob: %v", err)
	}

	shares, err := profiles.SharesRoom(ctx, userID, "@bob:test.local")
	if err != nil {
		t.Fatalf("SharesRoom() error: %v", err)
	}

	if shares {
		t.Fatal("SharesRoom() = true без общей комнаты")
	}
}
