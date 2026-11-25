//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registerProbe регистрирует пользователя через UIA dummy и возвращает токен.
func registerProbe(t *testing.T, srv *Server, username, password string) string {
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

// loginProbe логинится и возвращает HTTP-код и токен.
func loginProbe(t *testing.T, srv *Server, username, password string) (int, string, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", strings.NewReader(
		`{"type":"m.login.password","user":"`+username+`","password":"`+password+`","device_id":"DEV2"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	var resp struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	return rec.Code, resp.AccessToken, rec.Body.String()
}

// TestChangePassword — смена пароля через UIA, старый пароль перестаёт работать.
func TestChangePassword(t *testing.T) {
	srv := newIntegrationServer(t)

	token := registerProbe(t, srv, "chpw", "old-password-1")

	// Без auth — 401 с flow m.login.password.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/account/password",
		strings.NewReader(`{"new_password":"new-password-1"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "m.login.password") {
		t.Fatalf("без UIA: %d %s", rec.Code, rec.Body.String())
	}

	// Неверный текущий пароль - 401 с flows и M_FORBIDDEN (UIA по спецификации).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/account/password",
		strings.NewReader(`{"new_password":"new-password-1","auth":{"type":"m.login.password","password":"wrong"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"errcode":"M_FORBIDDEN"`) {
		t.Fatalf("неверный пароль: %d %s", rec.Code, rec.Body.String())
	}

	// Успешная смена (logout_devices по умолчанию true).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/account/password",
		strings.NewReader(`{"new_password":"new-password-1","auth":{"type":"m.login.password","password":"old-password-1"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("смена пароля: %d %s", rec.Code, rec.Body.String())
	}

	// Старый пароль не работает, новый — работает.
	if code, _, _ := loginProbe(t, srv, "chpw", "old-password-1"); code != http.StatusForbidden {
		t.Fatalf("старый пароль: код %d, ожидали 403", code)
	}

	if code, _, body := loginProbe(t, srv, "chpw", "new-password-1"); code != http.StatusOK {
		t.Fatalf("новый пароль: код %d, тело %s", code, body)
	}
}

// TestDeactivateAccount — деактивация отзывает токены и запрещает вход.
func TestDeactivateAccount(t *testing.T) {
	srv := newIntegrationServer(t)

	token := registerProbe(t, srv, "deact", "deact-password-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/account/deactivate",
		strings.NewReader(`{"auth":{"type":"m.login.password","password":"deact-password-1"}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate: %d %s", rec.Code, rec.Body.String())
	}

	// Вход запрещён с M_USER_DEACTIVATED.
	code, _, body := loginProbe(t, srv, "deact", "deact-password-1")
	if code != http.StatusForbidden || !strings.Contains(body, "M_USER_DEACTIVATED") {
		t.Fatalf("вход после деактивации: %d %s", code, body)
	}

	// Прежний токен отозван.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/account/whoami", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("токен после деактивации: %d %s", rec.Code, rec.Body.String())
	}
}
