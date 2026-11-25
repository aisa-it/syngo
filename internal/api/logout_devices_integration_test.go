//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestLogoutDeletesDevice (Complement TestLogout): /logout удаляет
// устройство сессии, /logout/all - все устройства, как в Synapse.
func TestLogoutDeletesDevice(t *testing.T) {
	server := newIntegrationServer(t)
	first := registerHTTP(t, server, "lodev", "FIRST")

	login := func(device string) string {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
			`{"type":"m.login.password","user":"lodev","password":"1234567890","device_id":"`+device+`"}`, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return resp.AccessToken
	}

	devices := func(token string) int {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/devices", "", token)

		var resp struct {
			Devices []json.RawMessage `json:"devices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("devices: %d %s", rec.Code, rec.Body.String())
		}

		return len(resp.Devices)
	}

	second := login("SECOND")
	third := login("THIRD")

	if n := devices(first); n != 3 {
		t.Fatalf("устройств до выхода: %d", n)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/logout", `{}`, second); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
	}

	if n := devices(first); n != 2 {
		t.Fatalf("устройств после logout: %d, ожидали 2", n)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/logout/all", `{}`, third); rec.Code != http.StatusOK {
		t.Fatalf("logout/all: %d %s", rec.Code, rec.Body.String())
	}

	if n := devices(login("FOURTH")); n != 1 {
		t.Fatalf("устройств после logout/all и нового входа: %d, ожидали 1", n)
	}
}
