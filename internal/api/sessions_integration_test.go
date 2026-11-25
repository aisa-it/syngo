//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSessionsAndUIA - сессии и UIA через HTTP.
func TestSessionsAndUIA(t *testing.T) {
	server := newIntegrationServer(t)
	server.cfg.Admin.Enabled = true

	const password = "correct horse battery"

	tok := registerPusherProbe(t, server, "sessalice", password)

	login := func() (string, string) {
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
			`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"sessalice"},"password":"`+password+`"}`, "")

		var resp struct {
			AccessToken string `json:"access_token"`
			DeviceID    string `json:"device_id"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return resp.AccessToken, resp.DeviceID
	}

	other, otherDevice := login()

	// UIA по спецификации: без auth - 401 с stages массивом и session.
	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/account/password", `{"new_password":"new-password-123"}`, tok)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"stages":["m.login.password"]`) ||
		strings.Contains(rec.Body.String(), `"session":""`) {
		t.Fatalf("UIA без auth: %d %s", rec.Code, rec.Body.String())
	}

	// Смена пароля с logout_devices: текущая сессия живет, другие устройства удалены.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/account/password",
		`{"new_password":"new-password-123","auth":{"type":"m.login.password","password":"`+password+`"}}`, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("смена пароля: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("текущий токен отозван после смены пароля: %d", rec.Code)
	}

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", other); rec.Code != http.StatusUnauthorized {
		t.Fatalf("токен другого устройства жив: %d", rec.Code)
	}

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/devices/"+otherDevice, "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("другое устройство не удалено: %d", rec.Code)
	}

	// Неверный пароль - 401 с M_FORBIDDEN и flows; после 5 ошибок - 429.
	for i := range 6 {
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/account/deactivate",
			`{"auth":{"type":"m.login.password","password":"wrong-`+string(rune('a'+i))+`"}}`, tok)

		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}

		if rec.Code != want {
			t.Fatalf("попытка %d: %d %s, ожидали %d", i+1, rec.Code, rec.Body.String(), want)
		}
	}

	// Вход в admin-контур: не-админ с верным и неверным паролем получает один ответ.
	bad := do(t, server, http.MethodPost, "/_syngo/admin/v1/login", `{"username":"sessalice","password":"nope"}`, "")
	good := do(t, server, http.MethodPost, "/_syngo/admin/v1/login", `{"username":"sessalice","password":"new-password-123"}`, "")

	if bad.Code != good.Code || bad.Body.String() != good.Body.String() || strings.Contains(good.Body.String(), "Not an admin") {
		t.Fatalf("admin login различает роль: %d %s / %d %s", bad.Code, bad.Body.String(), good.Code, good.Body.String())
	}
}

// TestDeactivateCleansUp — деактивация выводит из комнат и удаляет устройства.
func TestDeactivateCleansUp(t *testing.T) {
	server := newIntegrationServer(t)

	owner := registerPusherProbe(t, server, "deowner", "owner-password-1")
	tok := registerPusherProbe(t, server, "debob", "bob-password-123")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, owner)

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(created.RoomID)+"/join", `{}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/account/deactivate",
		`{"auth":{"type":"m.login.password","password":"bob-password-123"}}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("deactivate: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(created.RoomID)+"/joined_members", "", owner)
	if strings.Contains(rec.Body.String(), "@debob:") {
		t.Fatalf("деактивированный остался в комнате: %s", rec.Body.String())
	}

	var devices int
	if err := server.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM devices WHERE user_id = '@debob:test.local'`).Scan(&devices); err != nil || devices != 0 {
		t.Fatalf("устройства деактивированного: %d (%v)", devices, err)
	}
}
