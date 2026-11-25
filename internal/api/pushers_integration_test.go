//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registerPusherProbe регистрирует пользователя через UIA dummy и возвращает токен.
func registerPusherProbe(t *testing.T, srv *Server, username, password string) string {
	t.Helper()

	body := `{"username":"` + username + `","password":"` + password + `","device_id":"DEV"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	var challenge struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &challenge)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/register", strings.NewReader(
		`{"username":"`+username+`","password":"`+password+`","device_id":"DEV","auth":{"type":"m.login.dummy","session":"`+challenge.Session+`"}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	var reg struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)

	return reg.AccessToken
}

// TestPushers — set/list/remove pushers и валидация.
func TestPushers(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "pusher", "pusher-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Пустой список.
	rec := do(http.MethodGet, "/_matrix/client/v3/pushers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pushers":[]`) {
		t.Fatalf("пустой список: %d %s", rec.Code, rec.Body.String())
	}

	// Создание.
	rec = do(http.MethodPost, "/_matrix/client/v3/pushers/set",
		`{"app_id":"com.example.app","pushkey":"key1","kind":"http","app_display_name":"App","lang":"en","data":{"url":"https://push.example/_matrix/push/v1/notify","format":"event_id_only"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}

	// Список содержит запись.
	rec = do(http.MethodGet, "/_matrix/client/v3/pushers", "")

	var list struct {
		Pushers []struct {
			AppID   string `json:"app_id"`
			Pushkey string `json:"pushkey"`
			Kind    string `json:"kind"`
			Data    struct {
				URL string `json:"url"`
			} `json:"data"`
		} `json:"pushers"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode pushers: %v", err)
	}

	if len(list.Pushers) != 1 || list.Pushers[0].AppID != "com.example.app" {
		t.Fatalf("pushers = %+v", list.Pushers)
	}

	if list.Pushers[0].Data.URL != "https://push.example/_matrix/push/v1/notify" {
		t.Fatalf("data.url = %q", list.Pushers[0].Data.URL)
	}

	// Невалидный pusher: http без data.url.
	if rec = do(http.MethodPost, "/_matrix/client/v3/pushers/set",
		`{"app_id":"a","pushkey":"k","kind":"http","data":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("без url: %d %s", rec.Code, rec.Body.String())
	}

	// Удаление через kind=null.
	if rec = do(http.MethodPost, "/_matrix/client/v3/pushers/set",
		`{"app_id":"com.example.app","pushkey":"key1","kind":null}`); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/pushers", "")
	if !strings.Contains(rec.Body.String(), `"pushers":[]`) {
		t.Fatalf("после удаления: %s", rec.Body.String())
	}
}
