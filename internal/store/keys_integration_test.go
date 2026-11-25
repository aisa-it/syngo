//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/store"
)

// freshKeysStore поднимает схему с нуля и возвращает хранилище ключей.
func freshKeysStore(t *testing.T) *store.KeysStore {
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

	return store.NewKeysStore(openDB(t))
}

// seedKeyUser создаёт пользователя и устройство для ключей.
func seedKeyUser(t *testing.T, keys *store.KeysStore, userID, deviceID string) {
	t.Helper()

	ctx := context.Background()
	db := openDB(t)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, split_part(substr($1, 2), ':', 1), 'x')
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO devices (user_id, device_id, display_name) VALUES ($1, $2, '')
		 ON CONFLICT (user_id, device_id) DO NOTHING`, userID, deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	_ = keys
}

// TestKeysStoreUploadQuery — загрузка ключей и выдача через query.
func TestKeysStoreUploadQuery(t *testing.T) {
	keys := freshKeysStore(t)
	ctx := context.Background()

	const (
		userID   = "@alice:test.local"
		deviceID = "DEV1"
	)

	seedKeyUser(t, keys, userID, deviceID)

	if err := keys.UpsertDeviceKeys(ctx, userID, deviceID,
		json.RawMessage(`{"user_id":"@alice:test.local","device_id":"DEV1","algorithms":["m.olm.v1.curve25519-aes-sha2"]}`)); err != nil {
		t.Fatalf("UpsertDeviceKeys() error: %v", err)
	}

	if err := keys.UploadOneTimeKeys(ctx, userID, deviceID, map[string]json.RawMessage{
		"signed_curve25519:aaa": json.RawMessage(`{"key":"a"}`),
		"signed_curve25519:bbb": json.RawMessage(`{"key":"b"}`),
	}); err != nil {
		t.Fatalf("UploadOneTimeKeys() error: %v", err)
	}

	counts, err := keys.OneTimeKeyCounts(ctx, userID, deviceID)
	if err != nil {
		t.Fatalf("OneTimeKeyCounts() error: %v", err)
	}

	if counts["signed_curve25519"] != 2 {
		t.Fatalf("counts = %v, ожидали 2", counts)
	}

	queried, err := keys.QueryDeviceKeys(ctx, map[string][]string{userID: {deviceID}})
	if err != nil {
		t.Fatalf("QueryDeviceKeys() error: %v", err)
	}

	if len(queried[userID][deviceID]) == 0 {
		t.Fatalf("ключи не найдены: %+v", queried)
	}
}

// TestKeysStoreClaimAtomic — конкурентные claim не выдают один ключ дважды.
func TestKeysStoreClaimAtomic(t *testing.T) {
	keys := freshKeysStore(t)
	ctx := context.Background()

	const (
		userID   = "@alice:test.local"
		deviceID = "DEV1"
	)

	seedKeyUser(t, keys, userID, deviceID)

	if err := keys.UploadOneTimeKeys(ctx, userID, deviceID, map[string]json.RawMessage{
		"signed_curve25519:only": json.RawMessage(`{"key":"only"}`),
	}); err != nil {
		t.Fatalf("UploadOneTimeKeys() error: %v", err)
	}

	const workers = 8

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		issued int
	)

	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()

			result, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
				userID: {deviceID: "signed_curve25519"},
			})
			if err != nil {
				return
			}

			count := 0
			for _, devices := range result[userID] {
				count += len(devices)
			}

			mu.Lock()
			issued += count
			mu.Unlock()
		}()
	}

	wg.Wait()

	if issued != 1 {
		t.Fatalf("ключ выдан %d раз, ожидали ровно 1", issued)
	}

	// Повторный claim пуст: ключей больше нет.
	again, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
		userID: {deviceID: "signed_curve25519"},
	})
	if err != nil {
		t.Fatalf("повторный ClaimOneTimeKeys() error: %v", err)
	}

	if len(again[userID][deviceID]) != 0 {
		t.Fatalf("повторная выдача: %v", again)
	}

	_ = fmt.Sprintf
}

// keyField достаёт поле key из отданного one-time key (jsonb нормализует пробелы).
func keyField(t *testing.T, raw json.RawMessage) string {
	t.Helper()

	var body struct {
		Key string `json:"key"`
	}

	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode key %s: %v", raw, err)
	}

	return body.Key
}

// TestKeysStoreClaimIsolatedByDevice — key_id — счётчик, одинаковый у
// всех устройств (libolm/vodozemac «AAAAAQ»). Claim для одного устройства не
// должен помечать выданными ключи других пользователей/устройств и не должен
// отдавать чужой ключ.
func TestKeysStoreClaimIsolatedByDevice(t *testing.T) {
	keys := freshKeysStore(t)

	ctx := context.Background()

	const (
		alice = "@alice:test.local"
		bob   = "@bob:test.local"
		devA  = "ALICEDEV"
		devB  = "BOBDEV"
	)

	seedKeyUser(t, keys, alice, devA)
	seedKeyUser(t, keys, bob, devB)

	// Один и тот же key_id у разных устройств — как в реальных клиентах.
	if err := keys.UploadOneTimeKeys(ctx, alice, devA, map[string]json.RawMessage{
		"signed_curve25519:AAAAAQ": json.RawMessage(`{"key":"alice-key"}`),
	}); err != nil {
		t.Fatalf("upload alice: %v", err)
	}

	if err := keys.UploadOneTimeKeys(ctx, bob, devB, map[string]json.RawMessage{
		"signed_curve25519:AAAAAQ": json.RawMessage(`{"key":"bob-key"}`),
	}); err != nil {
		t.Fatalf("upload bob: %v", err)
	}

	claimed, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
		alice: {devA: "signed_curve25519"},
	})
	if err != nil {
		t.Fatalf("claim alice: %v", err)
	}

	if keyField(t, claimed[alice][devA]["signed_curve25519:AAAAAQ"]) != "alice-key" {
		t.Fatalf("alice получила чужой/не тот ключ: %s", claimed[alice][devA]["signed_curve25519:AAAAAQ"])
	}

	if _, leaked := claimed[bob]; leaked {
		t.Fatalf("в ответе для alice есть ключи другого пользователя: %v", claimed)
	}

	// Ключ bob остаётся доступным: claim alice не должен был его сжечь.
	claimedBob, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
		bob: {devB: "signed_curve25519"},
	})
	if err != nil {
		t.Fatalf("claim bob: %v", err)
	}

	if keyField(t, claimedBob[bob][devB]["signed_curve25519:AAAAAQ"]) != "bob-key" {
		t.Fatalf("bob: key_id-совпадение сожгло его ключ, получили %s",
			claimedBob[bob][devB]["signed_curve25519:AAAAAQ"])
	}
}

// TestKeysStoreClaimWildcardAlgorithm — algorithm "*" в запросе означает «любой
// алгоритм»: устройство получает самый старый невыданный ключ независимо от
// алгоритма.
func TestKeysStoreClaimWildcardAlgorithm(t *testing.T) {
	keys := freshKeysStore(t)

	ctx := context.Background()

	const (
		userID   = "@carol:test.local"
		deviceID = "CAROLDEV"
	)

	seedKeyUser(t, keys, userID, deviceID)

	if err := keys.UploadOneTimeKeys(ctx, userID, deviceID, map[string]json.RawMessage{
		"curve25519:AAAAAQ": json.RawMessage(`{"key":"curve"}`),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	claimed, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
		userID: {deviceID: "*"},
	})
	if err != nil {
		t.Fatalf("claim wildcard: %v", err)
	}

	if got := keyField(t, claimed[userID][deviceID]["curve25519:AAAAAQ"]); got != "curve" {
		t.Fatalf("wildcard не выдал ключ: %v", claimed)
	}

	// Повторный wildcard-claim: ключей больше нет — пусто, без ошибки.
	again, err := keys.ClaimOneTimeKeys(ctx, map[string]map[string]string{
		userID: {deviceID: "*"},
	})
	if err != nil {
		t.Fatalf("второй claim wildcard: %v", err)
	}

	if len(again) != 0 {
		t.Fatalf("повторный wildcard выдал ключи: %v", again)
	}
}

// TestKeysStoreConcurrentOTKUpload — Element шлет /keys/upload параллельно с
// пересекающимися наборами one-time keys; построчная вставка в порядке обхода map
// давала deadlock (40P01) и 500. Упорядоченная вставка - без ошибок.
func TestKeysStoreConcurrentOTKUpload(t *testing.T) {
	keys := freshKeysStore(t)
	ctx := context.Background()

	const userID, deviceID = "@alice:test.local", "DEV1"

	seedKeyUser(t, keys, userID, deviceID)

	batch := func(from, to int) map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for i := from; i < to; i++ {
			out[fmt.Sprintf("signed_curve25519:K%03d", i)] = json.RawMessage(fmt.Sprintf(`{"key":"k%d"}`, i))
		}

		return out
	}

	for round := 0; round < 10; round++ {
		errs := make(chan error, 4)

		for w := 0; w < 4; w++ {
			go func(w int) {
				errs <- keys.UploadOneTimeKeys(ctx, userID, deviceID, batch(round*10+w*5, round*10+w*5+40))
			}(w)
		}

		for w := 0; w < 4; w++ {
			if err := <-errs; err != nil {
				t.Fatalf("раунд %d: параллельная загрузка: %v", round, err)
			}
		}
	}

	counts, err := keys.OneTimeKeyCounts(ctx, userID, deviceID)
	if err != nil {
		t.Fatal(err)
	}

	if counts["signed_curve25519"] != 145 {
		t.Fatalf("ключей %d, ожидали 145 (без дублей)", counts["signed_curve25519"])
	}
}
