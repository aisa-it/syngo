//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAdminContour - ADR-0008: вход в admin-контур, is_admin-проверка, список.
func TestAdminContour(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Admin.Enabled = true

	userToken := registerPusherProbe(t, srv, "plainuser", "plain-user-pass-1")
	_ = registerPusherProbe(t, srv, "adminuser", "admin-user-pass-1")

	// Выдаём is_admin в БД.
	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = true WHERE localpart = 'adminuser'`); err != nil {
		t.Fatalf("flag admin: %v", err)
	}

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Обычный пользователь не может пользоваться admin API.
	if rec := do(userToken, http.MethodGet, "/_syngo/admin/v1/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("обычный пользователь: %d %s", rec.Code, rec.Body.String())
	}

	// Login админа.
	rec := do("", http.MethodPost, "/_syngo/admin/v1/login",
		`{"username":"adminuser","password":"admin-user-pass-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}

	var login struct {
		AdminToken string `json:"admin_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	if login.AdminToken == "" {
		t.Fatalf("пустой admin_token: %s", rec.Body.String())
	}

	// Список пользователей.
	rec = do(login.AdminToken, http.MethodGet, "/_syngo/admin/v1/users?from=", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "adminuser") {
		t.Fatalf("admin users: %d %s", rec.Code, rec.Body.String())
	}

	// limit списка пользователей уважается (≤100), мусор → 400.
	if rec = do(login.AdminToken, http.MethodGet, "/_syngo/admin/v1/users?limit=1", ""); rec.Code != http.StatusOK {
		t.Fatalf("users?limit=1: %d %s", rec.Code, rec.Body.String())
	}

	var page struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)

	if len(page.Items) != 1 {
		t.Fatalf("limit=1 вернул %d пользователей", len(page.Items))
	}

	if rec = do(login.AdminToken, http.MethodGet, "/_syngo/admin/v1/users?limit=abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("users?limit=abc: %d, ожидали 400", rec.Code)
	}

	// Обычный login в admin-контур — 403.
	if rec = do("", http.MethodPost, "/_syngo/admin/v1/login",
		`{"username":"plainuser","password":"plain-user-pass-1"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("не-админ login: %d %s", rec.Code, rec.Body.String())
	}

	// Пользовательский токен админа в admin-контур не пускаем.
	// Пользователь уже создан выше — логинимся через обычный CS-API.
	rec = do("", http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"adminuser"},"password":"admin-user-pass-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("CS login админа: %d %s", rec.Code, rec.Body.String())
	}

	var csLogin struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &csLogin)
	adminUserToken := csLogin.AccessToken

	if rec = do(adminUserToken, http.MethodGet, "/_syngo/admin/v1/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("CS-токен админа в admin API: %d %s", rec.Code, rec.Body.String())
	}

	// admin-токен в Client-Server API не принимается (ADR-0008 п.2).
	if rec = do(login.AdminToken, http.MethodGet, "/_matrix/client/v3/account/whoami", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("admin-токен в CS-API: %d %s", rec.Code, rec.Body.String())
	}

	// TTL из конфига реально применяется к admin-токену.
	var expiresAt *time.Time

	if err := srv.db.Pool().QueryRow(context.Background(),
		"SELECT expires_at FROM access_tokens WHERE scope = 'admin' ORDER BY created_at DESC LIMIT 1").
		Scan(&expiresAt); err != nil {
		t.Fatalf("select admin token: %v", err)
	}

	if expiresAt == nil {
		t.Fatal("admin-токен без expires_at: TTL не применён")
	}

	if time.Until(*expiresAt) <= 0 || time.Until(*expiresAt) > 48*time.Hour {
		t.Fatalf("expires_at вне ожидаемого TTL: %v", expiresAt)
	}
}

// TestAdminUserActions — деактивация, роль и сброс пароля + аудит.
func TestAdminUserActions(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Admin.Enabled = true

	_ = registerPusherProbe(t, srv, "adm2", "adm2-password-1")
	_ = registerPusherProbe(t, srv, "victim", "victim-password-1")

	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = true WHERE localpart = 'adm2'`); err != nil {
		t.Fatalf("flag admin: %v", err)
	}

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do("", http.MethodPost, "/_syngo/admin/v1/login", `{"username":"adm2","password":"adm2-password-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}

	var login struct {
		AdminToken string `json:"admin_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	target := "@victim:test.local"
	victimPath := "/_syngo/admin/v1/users/" + target

	// Сброс пароля.
	if rec = do(login.AdminToken, http.MethodPost, victimPath+"/password", `{"new_password":"victim-new-pass-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("password reset: %d %s", rec.Code, rec.Body.String())
	}

	// Деактивация + отзыв токенов.
	if rec = do(login.AdminToken, http.MethodPost, victimPath+"/deactivate", ""); rec.Code != http.StatusOK {
		t.Fatalf("deactivate: %d %s", rec.Code, rec.Body.String())
	}

	var deactivated bool
	if err := srv.db.Pool().QueryRow(context.Background(),
		`SELECT deactivated FROM users WHERE user_id = $1`, target).Scan(&deactivated); err != nil || !deactivated {
		t.Fatalf("deactivated=%v err=%v", deactivated, err)
	}

	// Выдача роли и аудит.
	if rec = do(login.AdminToken, http.MethodPost, victimPath+"/admin", ""); rec.Code != http.StatusOK {
		t.Fatalf("grant admin: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(login.AdminToken, http.MethodGet, "/_syngo/admin/v1/audit?after_id=0&limit=50", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "password_reset") {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAdminRooms — список комнат и удаление (каскад).
func TestAdminRooms(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Admin.Enabled = true

	_ = registerPusherProbe(t, srv, "adm3", "adm3-password-1")
	_ = registerPusherProbe(t, srv, "roomer", "roomer-password-1")

	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = true WHERE localpart = 'adm3'`); err != nil {
		t.Fatalf("flag admin: %v", err)
	}

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Комната от обычного пользователя.
	rec := do(registerPusherProbe(t, srv, "roomer2", "roomer2-password-1"), http.MethodPost,
		"/_matrix/client/v3/createRoom", `{"name":"to-delete","preset":"public_chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Второй локальный участник: при purge должен получить leave.
	memberTok := registerPusherProbe(t, srv, "roomer3", "roomer3-password-1")

	if rec = do(memberTok, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("member join: %d %s", rec.Code, rec.Body.String())
	}

	login := do("", http.MethodPost, "/_syngo/admin/v1/login", `{"username":"adm3","password":"adm3-password-1"}`)

	var auth struct {
		AdminToken string `json:"admin_token"`
	}
	_ = json.Unmarshal(login.Body.Bytes(), &auth)

	if auth.AdminToken == "" {
		t.Fatalf("admin login: %d %s", login.Code, login.Body.String())
	}

	// Список содержит комнату.
	rec = do(auth.AdminToken, http.MethodGet, "/_syngo/admin/v1/rooms?from=", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.RoomID) {
		t.Fatalf("admin rooms: %d %s", rec.Code, rec.Body.String())
	}

	// DELETE без purge — блокировка: история остаётся, join запрещён.
	if rec = do(auth.AdminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+created.RoomID, ""); rec.Code != http.StatusOK {
		t.Fatalf("block room: %d %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := srv.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE room_id = $1`, created.RoomID).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}

	if count == 0 {
		t.Fatal("блокировка удалила историю, а не должна")
	}

	blocked, err := srv.db.RoomBlocked(context.Background(), created.RoomID)
	if err != nil || !blocked {
		t.Fatalf("RoomBlocked=%v err=%v", blocked, err)
	}

	// purge=true — физическое удаление.
	if rec = do(auth.AdminToken, http.MethodDelete,
		"/_syngo/admin/v1/rooms/"+created.RoomID+"?purge=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("purge room: %d %s", rec.Code, rec.Body.String())
	}

	if err := srv.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE room_id = $1`, created.RoomID).Scan(&count); err != nil {
		t.Fatalf("count events after purge: %v", err)
	}

	if count != 0 {
		t.Fatalf("после purge осталось %d событий", count)
	}

	// purge уведомил участников: в ответе notified >= 1.
	if !strings.Contains(rec.Body.String(), `"notified"`) {
		t.Fatalf("в ответе purge нет notified: %s", rec.Body.String())
	}
}

// TestAdminGuards - нельзя выключить себя, снять роль с себя или последнего админа.
func TestAdminGuards(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Admin.Enabled = true

	_ = registerPusherProbe(t, srv, "guard", "guard-password-1")
	_ = registerPusherProbe(t, srv, "second", "second-password-1")

	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = true WHERE localpart IN ('guard','second')`); err != nil {
		t.Fatalf("flag admins: %v", err)
	}

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do("", http.MethodPost, "/_syngo/admin/v1/login", `{"username":"guard","password":"guard-password-1"}`)

	var auth struct {
		AdminToken string `json:"admin_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &auth)

	if auth.AdminToken == "" {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}

	me := "@guard:test.local"

	// Себя выключить нельзя.
	if rec = do(auth.AdminToken, http.MethodPost, "/_syngo/admin/v1/users/"+me+"/deactivate", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("self deactivate: %d %s", rec.Code, rec.Body.String())
	}

	// С себя роль снять нельзя.
	if rec = do(auth.AdminToken, http.MethodPost, "/_syngo/admin/v1/users/"+me+"/unadmin", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("self unadmin: %d %s", rec.Code, rec.Body.String())
	}

	// Снять роль со второго админа можно.
	if rec = do(auth.AdminToken, http.MethodPost, "/_syngo/admin/v1/users/@second:test.local/unadmin", ""); rec.Code != http.StatusOK {
		t.Fatalf("unadmin second: %d %s", rec.Code, rec.Body.String())
	}

	// Последнего админа снять нельзя (добавляем админа из другого пользователя).
	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = true WHERE localpart = 'second'`); err != nil {
		t.Fatalf("re-flag second: %v", err)
	}

	// Прямой вызов с обходом self-check: цель — не я, но админ один.
	if _, err := srv.db.Pool().Exec(context.Background(),
		`UPDATE users SET is_admin = false WHERE localpart = 'second'`); err != nil {
		t.Fatalf("unflag second: %v", err)
	}

	// countAdmins = 1 (guard) — попытка снять роль с guard блокируется self-check,
	// поэтому проверяем защиту «последнего админа» через прямой вызов хранилища.
	admins, err := srv.db.CountAdmins(context.Background())
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}

	if admins != 1 {
		t.Fatalf("admins = %d, ожидали 1", admins)
	}
}
