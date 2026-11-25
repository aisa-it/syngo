//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/auth"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// directoryProvider - подставной каталог LDAP: имя -> пароль и displayname.
type directoryProvider struct {
	passwords map[string]string
	names     map[string]string
}

func (p *directoryProvider) Name() string { return "test-ldap" }

func (p *directoryProvider) CheckPassword(_ context.Context, username, password string) (core.ExternalIdentity, bool, error) {
	name := strings.ToLower(username)
	if want, ok := p.passwords[name]; !ok || want != password {
		return core.ExternalIdentity{}, false, nil
	}

	return core.ExternalIdentity{Localpart: name, DisplayName: p.names[name], SyncDisplayName: true}, true, nil
}

// TestLoginViaDirectory - вход пользователя каталога: первый вход создает
// учетную запись с именем из каталога, следующий синхронизирует displayname и
// рассылает его в комнаты; пароль каталога подходит для UIA.
func TestLoginViaDirectory(t *testing.T) {
	server := newIntegrationServer(t)
	directory := &directoryProvider{
		passwords: map[string]string{"ivanov": "ldap-pass"},
		names:     map[string]string{"ivanov": "Иванов Иван"},
	}

	server.auth = core.NewAuth(store.NewAuthStore(server.db), auth.NewMultiHasher(), auth.NewTokenSource(),
		core.AuthOptions{
			ServerName: "test.local", RegistrationOpen: true, MinPasswordLength: 8,
			PasswordProviders: []core.PasswordProvider{directory},
			OnExternalLogin:   server.syncExternalProfile,
		})

	login := func(user, password string) (int, map[string]any) {
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
			`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"`+user+`"},"password":"`+password+`"}`, "")

		return rec.Code, decodeJSON(t, rec.Body.Bytes())
	}

	code, body := login("Ivanov", "ldap-pass")
	if code != http.StatusOK || body["user_id"] != "@ivanov:test.local" {
		t.Fatalf("первый вход: %d %v", code, body)
	}

	token, _ := body["access_token"].(string)

	profile := decodeJSON(t, do(t, server, http.MethodGet, "/_matrix/client/v3/profile/@ivanov:test.local", "", token).Body.Bytes())
	if profile["displayname"] != "Иванов Иван" {
		t.Fatalf("профиль после первого входа: %v", profile)
	}

	// Комната: смена имени в каталоге должна дойти до участников.
	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"ldap"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	roomID, _ := decodeJSON(t, rec.Body.Bytes())["room_id"].(string)

	directory.names["ivanov"] = "Иванов Иван Иванович"

	if code, body := login("ivanov", "ldap-pass"); code != http.StatusOK {
		t.Fatalf("второй вход: %d %v", code, body)
	}

	profile = decodeJSON(t, do(t, server, http.MethodGet, "/_matrix/client/v3/profile/@ivanov:test.local", "", token).Body.Bytes())
	if profile["displayname"] != "Иванов Иван Иванович" {
		t.Fatalf("displayname не синхронизирован: %v", profile)
	}

	deadline := time.Now().Add(10 * time.Second)

	for {
		member := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/state/m.room.member/@ivanov:test.local", "", token)
		if strings.Contains(member.Body.String(), "Иванов Иван Иванович") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("новое имя не разослано в комнату: %s", member.Body.String())
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Неверный пароль каталога: локального пароля нет - 403 M_FORBIDDEN.
	if code, body := login("ivanov", "wrong"); code != http.StatusForbidden || body["errcode"] != ErrCodeForbidden {
		t.Fatalf("неверный пароль: %d %v", code, body)
	}

	// Пароль каталога подтверждает UIA (удаление устройства).
	devices := decodeJSON(t, do(t, server, http.MethodGet, "/_matrix/client/v3/devices", "", token).Body.Bytes())

	list, _ := devices["devices"].([]any)
	if len(list) != 2 {
		t.Fatalf("устройства: %v", devices)
	}

	other := ""

	for _, raw := range list {
		device, _ := raw.(map[string]any)
		if id, _ := device["device_id"].(string); id != body["device_id"] && id != "" {
			other = id
		}
	}

	first := do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices", `{"devices":["`+other+`"]}`, token)
	session, _ := decodeJSON(t, first.Body.Bytes())["session"].(string)

	second := do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices",
		`{"devices":["`+other+`"],"auth":{"type":"m.login.password","session":"`+session+`",`+
			`"identifier":{"type":"m.id.user","user":"@ivanov:test.local"},"password":"ldap-pass"}}`, token)
	if second.Code != http.StatusOK {
		t.Fatalf("UIA паролем каталога: %d %s", second.Code, second.Body.String())
	}
}

// decodeJSON разбирает тело ответа в map.
func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	return out
}
