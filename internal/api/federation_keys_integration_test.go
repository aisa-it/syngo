//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestFederationUserKeysEndpoints - /_matrix/federation/v1/user/keys/query,
// /user/keys/claim и /user/devices/{userId} отдают ключи и устройства наших
// пользователей другому серверу. Раньше этих эндпоинтов не было, и
// Synapse не мог зашифровать сообщение для наших устройств.
func TestFederationUserKeysEndpoints(t *testing.T) {
	env := newFedEnv(t)
	bob := env.localUserToken(t)

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/keys/upload", `{
		"device_keys":{"user_id":"@bob:test.local","device_id":"BOB","algorithms":["m.olm.v1.curve25519-aes-sha2"],
			"keys":{"curve25519:BOB":"cBOB","ed25519:BOB":"eBOB"}},
		"one_time_keys":{"signed_curve25519:AAAA":{"key":"otk1"}}}`, bob)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/upload: %d %s", rec.Code, rec.Body.String())
	}

	rec = env.request(t, http.MethodPost, "/_matrix/federation/v1/user/keys/query",
		map[string]any{"device_keys": map[string]any{"@bob:test.local": []string{}, "@x:remote.test": []string{}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("user/keys/query: %d %s", rec.Code, rec.Body.String())
	}

	var query struct {
		DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &query)

	if _, ok := query.DeviceKeys["@bob:test.local"]["BOB"]; !ok {
		t.Fatalf("user/keys/query: %s", rec.Body.String())
	}

	if _, ok := query.DeviceKeys["@x:remote.test"]; ok {
		t.Fatalf("сервер ответил за чужого пользователя: %s", rec.Body.String())
	}

	rec = env.request(t, http.MethodPost, "/_matrix/federation/v1/user/keys/claim",
		map[string]any{"one_time_keys": map[string]any{"@bob:test.local": map[string]string{"BOB": "signed_curve25519"}}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "otk1") {
		t.Fatalf("user/keys/claim: %d %s", rec.Code, rec.Body.String())
	}

	rec = env.request(t, http.MethodGet, "/_matrix/federation/v1/user/devices/@bob:test.local", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("user/devices: %d %s", rec.Code, rec.Body.String())
	}

	var devices userDevicesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &devices)

	if devices.UserID != "@bob:test.local" || len(devices.Devices) != 1 || devices.Devices[0].DeviceID != "BOB" ||
		!strings.Contains(string(devices.Devices[0].Keys), "cBOB") {
		t.Fatalf("user/devices: %s", rec.Body.String())
	}

	// Пользователь другого сервера - не наш.
	if rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/user/devices/@x:remote.test", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("user/devices чужого: %d", rec.Code)
	}

	// Без X-Matrix - 401.
	if rec := env.requestUnsigned(t, http.MethodGet, "/_matrix/federation/v1/user/devices/@bob:test.local", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без подписи: %d", rec.Code)
	}
}
