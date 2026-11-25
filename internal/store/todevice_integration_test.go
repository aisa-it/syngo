//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/store"
)

// freshToDeviceDB поднимает схему с нуля и отдаёт БД для тестов to-device.
func freshToDeviceDB(t *testing.T) *store.DB {
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

	return openDB(t)
}

// TestSendToDeviceWildcardDevice — device_id "*" адресует все устройства
// пользователя, а не создаёт устройство с именем "*".
func TestSendToDeviceWildcardDevice(t *testing.T) {
	db := freshToDeviceDB(t)
	ctx := context.Background()

	userID := "@wild:test.local"

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, 'wild', 'x')
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	for _, deviceID := range []string{"DEV1", "DEV2"} {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO devices (user_id, device_id, display_name) VALUES ($1, $2, '')
			 ON CONFLICT (user_id, device_id) DO NOTHING`, userID, deviceID); err != nil {
			t.Fatalf("insert device %s: %v", deviceID, err)
		}
	}

	if _, err := db.SendToDevice(ctx, "@sender:test.local", "SENDERDEV", "m.key.verification.request", "txn1",
		map[store.DeviceTarget]json.RawMessage{
			{UserID: userID, DeviceID: "*"}: json.RawMessage(`{"transaction_id":"1"}`),
		}); err != nil {
		t.Fatalf("SendToDevice() error: %v", err)
	}

	for _, deviceID := range []string{"DEV1", "DEV2"} {
		pending, err := db.PendingToDevice(ctx, userID, deviceID, 0, 100)
		if err != nil {
			t.Fatalf("pending %s: %v", deviceID, err)
		}

		if len(pending) != 1 || pending[0].Type != "m.key.verification.request" {
			t.Fatalf("%s: ожидали одно сообщение, получили %+v", deviceID, pending)
		}
	}

	// Устройства "*" не появилось.
	var count int

	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM device_inbox WHERE device_id = '*'`).Scan(&count); err != nil {
		t.Fatalf("count wildcard rows: %v", err)
	}

	if count != 0 {
		t.Fatalf("записей с device_id='*': %d", count)
	}
}

// TestDeleteDeviceDropsInbox — удаление устройства чистит device_inbox, иначе
// новое устройство с тем же id получит чужие сообщения.
func TestDeleteDeviceDropsInbox(t *testing.T) {
	db := freshToDeviceDB(t)
	ctx := context.Background()

	userID := "@drop:test.local"
	deviceID := "DROPDEV"

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, 'drop', 'x')
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO devices (user_id, device_id, display_name) VALUES ($1, $2, '')`, userID, deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	if _, err := db.SendToDevice(ctx, "@sender:test.local", "SENDERDEV", "m.room_key", "txn2",
		map[store.DeviceTarget]json.RawMessage{
			{UserID: userID, DeviceID: deviceID}: json.RawMessage(`{"k":"v"}`),
		}); err != nil {
		t.Fatalf("SendToDevice() error: %v", err)
	}

	if err := store.NewProfileStore(db).DeleteDevice(ctx, userID, deviceID); err != nil {
		t.Fatalf("DeleteDevice() error: %v", err)
	}

	pending, err := db.PendingToDevice(ctx, userID, deviceID, 0, 100)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	if len(pending) != 0 {
		t.Fatalf("после удаления устройства в inbox осталось %d сообщений", len(pending))
	}
}
