package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// memKeysStore — in-memory реализация core.KeysStore для HTTP-тестов.
type memKeysStore struct {
	mu      sync.Mutex
	devices map[string]json.RawMessage            // user/device
	otk     map[string]map[string]json.RawMessage // user/device -> key_id -> key
}

func newMemKeysStore() *memKeysStore {
	return &memKeysStore{
		devices: map[string]json.RawMessage{},
		otk:     map[string]map[string]json.RawMessage{},
	}
}

func keysKey(userID, deviceID string) string { return userID + "/" + deviceID }

func (m *memKeysStore) UpsertDeviceKeys(_ context.Context, userID, deviceID string, keys json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.devices[keysKey(userID, deviceID)] = keys

	return nil
}

func (m *memKeysStore) UploadOneTimeKeys(_ context.Context, userID, deviceID string, keys map[string]json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := keysKey(userID, deviceID)
	if m.otk[key] == nil {
		m.otk[key] = map[string]json.RawMessage{}
	}

	for keyID, data := range keys {
		if _, exists := m.otk[key][keyID]; !exists {
			m.otk[key][keyID] = data
		}
	}

	return nil
}

// UploadFallbackKeys - fallback keys в HTTP-тестах не проверяются (см.
// TestFallbackKeys на живой БД), поэтому не хранятся.
func (m *memKeysStore) UploadFallbackKeys(context.Context, string, string, map[string]json.RawMessage) error {
	return nil
}

func (m *memKeysStore) OneTimeKeyCounts(_ context.Context, userID, deviceID string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	counts := map[string]int{}
	for keyID := range m.otk[keysKey(userID, deviceID)] {
		algorithm, _, _ := strings.Cut(keyID, ":")
		counts[algorithm]++
	}

	return counts, nil
}

func (m *memKeysStore) QueryDeviceKeys(_ context.Context, requests map[string][]string) (map[string]map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := map[string]map[string]json.RawMessage{}

	for userID, devices := range requests {
		for _, deviceID := range devices {
			if keys, ok := m.devices[keysKey(userID, deviceID)]; ok {
				if result[userID] == nil {
					result[userID] = map[string]json.RawMessage{}
				}

				result[userID][deviceID] = keys
			}
		}
	}

	return result, nil
}

func (m *memKeysStore) ClaimOneTimeKeys(_ context.Context, requests map[string]map[string]string) (map[string]map[string]map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := map[string]map[string]map[string]json.RawMessage{}

	for userID, devices := range requests {
		for deviceID, algorithm := range devices {
			keys := m.otk[keysKey(userID, deviceID)]

			for keyID, data := range keys {
				if !strings.HasPrefix(keyID, algorithm+":") {
					continue
				}

				if result[userID] == nil {
					result[userID] = map[string]map[string]json.RawMessage{}
				}

				if result[userID][deviceID] == nil {
					result[userID][deviceID] = map[string]json.RawMessage{}
				}

				result[userID][deviceID][keyID] = data
				delete(keys, keyID)

				break
			}
		}
	}

	return result, nil
}

// newKeysServer собирает сервер с ключами и фейковым хранилищем.
func newKeysServer(t *testing.T) *Server {
	t.Helper()

	server := newAuthServer(t, core.AuthOptions{})
	server.keys = core.NewKeysService(newMemKeysStore(), "test.local")

	return server
}

// uploadKeys загружает device keys и OTK от имени пользователя.
func uploadKeys(t *testing.T, server *Server, token, userID, deviceID string, otk int) map[string]int {
	t.Helper()

	oneTimeKeys := map[string]json.RawMessage{}
	for i := 0; i < otk; i++ {
		oneTimeKeys[fmt.Sprintf("signed_curve25519:otk%d", i)] = json.RawMessage(fmt.Sprintf(`{"key":"otk%d"}`, i))
	}

	body, err := json.Marshal(map[string]any{
		"device_keys": map[string]any{
			"user_id":    userID,
			"device_id":  deviceID,
			"algorithms": []string{"m.olm.v1.curve25519-aes-sha2"},
			"keys":       map[string]string{"curve25519:" + deviceID: "curve"},
		},
		"one_time_keys": oneTimeKeys,
	})
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload", string(body), token)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/upload: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp keysUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal upload: %v", err)
	}

	return resp.OneTimeKeyCounts
}

// TestKeysUploadQuery — upload → query отдаёт те же ключи.
func TestKeysUploadQuery(t *testing.T) {
	t.Parallel()

	server := newKeysServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	counts := uploadKeys(t, server, alice.AccessToken, alice.UserID, alice.DeviceID, 3)
	if counts["signed_curve25519"] != 3 {
		t.Fatalf("one_time_key_counts = %v, ожидали 3", counts)
	}

	body := fmt.Sprintf(`{"device_keys":{%q:[%q]}}`, alice.UserID, alice.DeviceID)
	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query", body, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/query: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp keysQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal query: %v", err)
	}

	if _, ok := resp.DeviceKeys[alice.UserID][alice.DeviceID]; !ok {
		t.Fatalf("ключи устройства не найдены: %+v", resp.DeviceKeys)
	}

	if len(resp.Failures) != 0 {
		t.Fatalf("failures = %v", resp.Failures)
	}
}

// TestKeysQueryPrivacy — чужие серверы не раскрываются.
func TestKeysQueryPrivacy(t *testing.T) {
	t.Parallel()

	server := newKeysServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	body := `{"device_keys":{"@bob:other.local":["DEV"]}}`
	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query", body, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp keysQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.DeviceKeys) != 0 {
		t.Fatalf("ключи чужого сервера раскрыты: %+v", resp.DeviceKeys)
	}
}

// TestKeysClaimOnce — one-time key выдаётся ровно один раз, затем M_NOT_FOUND.
func TestKeysClaimOnce(t *testing.T) {
	t.Parallel()

	server := newKeysServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	uploadKeys(t, server, alice.AccessToken, alice.UserID, alice.DeviceID, 1)
	uploadKeys(t, server, bob.AccessToken, bob.UserID, bob.DeviceID, 1)

	claim := func() (int, map[string]any) {
		body := fmt.Sprintf(`{"one_time_keys":{%q:{%q:"signed_curve25519"}}}`, alice.UserID, alice.DeviceID)
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/claim", body, bob.AccessToken)

		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return rec.Code, resp
	}

	code, resp := claim()
	if code != http.StatusOK {
		t.Fatalf("первый claim: status %d, body %v", code, resp)
	}

	if _, ok := resp["one_time_keys"]; !ok {
		t.Fatalf("в ответе нет one_time_keys: %v", resp)
	}

	// Ключ уже выдан: по спеке это 200 с пустым one_time_keys, а не 404.
	// 404 клиенты трактуют как ошибку эндпоинта и не ретраят.
	code, resp = claim()
	if code != http.StatusOK {
		t.Fatalf("повторный claim: status %d, ожидали 200 (ключ уже выдан), body %v", code, resp)
	}

	if keys, ok := resp["one_time_keys"].(map[string]any); !ok || len(keys) != 0 {
		t.Fatalf("повторный claim: ожидали пустой one_time_keys, получили %v", resp["one_time_keys"])
	}
}

// TestKeysUploadRejectsForeignDevice — ключи не могут быть загружены за другое устройство.
func TestKeysUploadRejectsForeignDevice(t *testing.T) {
	t.Parallel()

	server := newKeysServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	body := fmt.Sprintf(`{"device_keys":{"user_id":%q,"device_id":"OTHERDEVICE","algorithms":[],"keys":{}}}`,
		alice.UserID)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload", body, alice.AccessToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, ожидали 400 (чужое устройство)", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeInvalidParam {
		t.Fatalf("errcode %q", errResp.Code)
	}
}

// TestKeysQueryShapes (Complement TestKeysQueryWithDeviceIDAsObjectFails,
// TestUploadKey): device_keys не той формы - 400 M_BAD_JSON, локальный
// пользователь без ключей - пустой объект в device_keys.
func TestKeysQueryShapes(t *testing.T) {
	t.Parallel()

	server := newKeysServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query",
		`{"device_keys":{"`+bob.UserID+`":{"device_id1":true}}}`, alice.AccessToken)
	if rec.Code != http.StatusBadRequest || decodeMatrixError(t, rec).Code != ErrCodeBadJSON {
		t.Fatalf("device_keys объектом: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query",
		`{"device_keys":{"`+bob.UserID+`":[]}}`, alice.AccessToken)

	var resp struct {
		DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("query: %d %s", rec.Code, rec.Body.String())
	}

	if keys, ok := resp.DeviceKeys[bob.UserID]; !ok || len(keys) != 0 {
		t.Fatalf("device_keys bob без ключей: %s", rec.Body.String())
	}
}
