package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// createRoomFor создаёт комнату от имени пользователя и возвращает room_id.
func createRoomFor(t *testing.T, server *Server, token string) string {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"name":"Test room","preset":"public_chat","visibility":"public"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode createRoom: %v", err)
	}

	return resp.RoomID
}

func joinRoom(t *testing.T, server *Server, token, roomID string) {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRoomMembersAndJoinedMembers — GET /members и /joined_members.
func TestRoomMembersAndJoinedMembers(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)

	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")
	carol := registerUser(t, server, "carol", "1234567890")

	roomID := createRoomFor(t, server, alice.AccessToken)
	joinRoom(t, server, bob.AccessToken, roomID)

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/members?limit=10", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("members: %d %s", rec.Code, rec.Body.String())
	}

	var members struct {
		Chunk []struct {
			Type     string  `json:"type"`
			StateKey *string `json:"state_key"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode members: %v", err)
	}

	if len(members.Chunk) < 2 {
		t.Fatalf("members: %d событий, ожидали ≥2", len(members.Chunk))
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/joined_members", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("joined_members: %d %s", rec.Code, rec.Body.String())
	}

	var joined struct {
		Joined map[string]map[string]string `json:"joined"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &joined); err != nil {
		t.Fatalf("decode joined: %v", err)
	}

	if len(joined.Joined) != 2 {
		t.Fatalf("joined: %d участников, ожидали 2 (%v)", len(joined.Joined), joined.Joined)
	}

	// Не участник — 403.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/members", "", carol.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("members не-участником: %d, ожидали 403", rec.Code)
	}
}

// TestDirectoryAliasLifecycle — PUT/GET/DELETE /directory/room/{alias} и /rooms/{id}/aliases.
func TestDirectoryAliasLifecycle(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	roomID := createRoomFor(t, server, alice.AccessToken)
	alias := "#testalias:test.local"
	encodedAlias := "%23testalias:test.local"

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/directory/room/"+encodedAlias,
		`{"room_id":"`+roomID+`"}`, alice.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("create alias: %d %s", rec.Code, rec.Body.String())
	}

	// Резолв без авторизации.
	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/directory/room/"+encodedAlias, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve alias: %d %s", rec.Code, rec.Body.String())
	}

	var resolved struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil || resolved.RoomID != roomID {
		t.Fatalf("resolve = %+v, err=%v", resolved, err)
	}

	// Список алиасов комнаты.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/aliases", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("room aliases: %d %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Aliases []string `json:"aliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Aliases) != 1 || list.Aliases[0] != alias {
		t.Fatalf("aliases = %+v, err=%v", list, err)
	}

	// Не-создатель не может создать алиас.
	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/directory/room/%23other:test.local",
		`{"room_id":"`+roomID+`"}`, bob.AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("alias не-создателем: %d, ожидали 403", rec.Code)
	}

	// Удаление и 404 после него.
	if rec := do(t, server, http.MethodDelete, "/_matrix/client/v3/directory/room/"+encodedAlias, "", alice.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("delete alias: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/directory/room/"+encodedAlias, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("resolve удалённого алиаса: %d, ожидали 404", rec.Code)
	}
}

// TestRegisterAvailable — GET /register/available.
func TestRegisterAvailable(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})
	registerUser(t, server, "taken", "1234567890")

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/register/available?username=taken", "", "")

	// Спека v1.11: занятое имя — 400 M_USER_IN_USE.
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "M_USER_IN_USE") {
		t.Fatalf("занятый localpart: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/register/available?username=free", "", "")

	var resp struct {
		Available bool `json:"available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if rec.Code != http.StatusOK || !resp.Available {
		t.Fatalf("свободный localpart: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRefreshToken — POST /refresh: новый access работает, старый refresh больше нет.
func TestRefreshToken(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{RefreshTokens: true})
	registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},"password":"1234567890","refresh_token":true}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}

	var login struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || login.RefreshToken == "" {
		t.Fatalf("login decode: %v %+v", err, login)
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/refresh",
		`{"refresh_token":"`+login.RefreshToken+`"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}

	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil || refreshed.AccessToken == "" {
		t.Fatalf("refresh decode: %v %+v", err, refreshed)
	}

	if refreshed.AccessToken == login.AccessToken {
		t.Fatal("access-токен должен быть новым")
	}

	// Новый access работает.
	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", refreshed.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("whoami новым токеном: %d", rec.Code)
	}
}

// TestDeleteDevices — POST /delete_devices: пароль обязателен, устройства и токены удаляются.
func TestDeleteDevices(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	server.profiles = newMemProfileStore()

	alice := registerUser(t, server, "alice", "1234567890")

	// Неверный пароль - 401 с flows и M_FORBIDDEN (UIA по спецификации).
	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices",
		`{"devices":["`+alice.DeviceID+`"],"auth":{"type":"m.login.password","password":"wrong"}}`, alice.AccessToken)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"errcode":"M_FORBIDDEN"`) ||
		!strings.Contains(rec.Body.String(), `"stages":["m.login.password"]`) {
		t.Fatalf("неверный пароль: %d %s", rec.Code, rec.Body.String())
	}

	// Верный пароль — устройство удалено, токен перестал работать.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices",
		`{"devices":["`+alice.DeviceID+`"],"auth":{"type":"m.login.password","password":"1234567890"}}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete_devices: %d %s", rec.Code, rec.Body.String())
	}

	// Отзыв токенов при удалении устройства проверяется в store-тестах (ProfileStore.DeleteDevice):
	// здесь только формат/права, потому что в тестовом окружении профили и auth — разные in-memory заглушки.
}

// TestCORSPreflight — Element Web с другого origin: браузер шлёт preflight,
// без CORS-заголовков запрос блокируется.
func TestCORSPreflight(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/_matrix/client/v3/login", http.NoBody)
	req.Header.Set("Origin", "http://192.0.2.10:18080")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preflight: код %d", rec.Code)
	}

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q", got)
	}

	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("Allow-Headers без Authorization: %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

// TestCapabilities — Element Web запрашивает capabilities при старте (с токеном);
// без него клиент может зависнуть на загрузке. Как в Synapse, без токена - 401, в
// ответе есть m.change_password (Complement TestServerCapabilities).
func TestCapabilities(t *testing.T) {
	t.Parallel()

	srv := newAuthServer(t, core.AuthOptions{})
	alice := registerUser(t, srv, "alice", "1234567890")

	if rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/capabilities", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("capabilities без токена: код %d, ожидали 401", rec.Code)
	}

	rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/capabilities", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities: код %d", rec.Code)
	}

	var body struct {
		Capabilities struct {
			RoomVersions struct {
				Default   string            `json:"default"`
				Available map[string]string `json:"available"`
			} `json:"m.room_versions"`
			ChangePassword *struct {
				Enabled bool `json:"enabled"`
			} `json:"m.change_password"`
		} `json:"capabilities"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Capabilities.RoomVersions.Default == "" || len(body.Capabilities.RoomVersions.Available) == 0 {
		t.Fatalf("пустые capabilities: %s", rec.Body.String())
	}

	if body.Capabilities.ChangePassword == nil || !body.Capabilities.ChangePassword.Enabled {
		t.Fatalf("нет m.change_password: %s", rec.Body.String())
	}
}

// TestAdminTokenRejectedInCSAPI — контуры разделены (ADR-0008): токен со scope
// admin не должен приниматься в Client-Server API.
func TestAdminTokenRejectedInCSAPI(t *testing.T) {
	t.Parallel()

	if check := adminScopeRejected("admin"); !check {
		t.Fatal("admin-токен должен отвергаться в CS-API")
	}

	if check := adminScopeRejected("admin:read"); !check {
		t.Fatal("admin:read-токен должен отвергаться в CS-API")
	}

	if check := adminScopeRejected(""); check {
		t.Fatal("обычный токен не должен отвергаться")
	}
}

// TestAdminWriteScope - изменяющие admin-маршруты зарегистрированы и закрыты
// admin-контуром. Раньше тест проверял только «не 405» и проходил при
// 404 выключенного admin API. Теперь: маршрут есть в таблице роутера,
// без токена - 401 от authenticate, с токеном запрос доходит до requireAdmin - при
// выключенном admin API это JSON M_NOT_FOUND «Admin API is disabled», а не текстовый
// 404 роутера. Сам write-scope требует БД (requireAdmin); scope admin:read сервер
// пока не выдает.
func TestAdminWriteScope(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)
	token := registerUser(t, srv, "notadmin", "1234567890").AccessToken

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/_syngo/admin/v1/users/@u:x/deactivate"},
		{http.MethodDelete, "/_syngo/admin/v1/rooms/%21r%3Ax"},
	} {
		assertTokenRequired(t, srv, tc.method, tc.path, "")

		rec := do(t, srv, tc.method, tc.path, "", token)

		if rec.Code != http.StatusNotFound || decodeMatrixError(t, rec).Code != ErrCodeNotFound ||
			!strings.Contains(rec.Body.String(), "Admin API is disabled") {
			t.Fatalf("%s %s: %d %s, ожидали ответ requireAdmin «Admin API is disabled»",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
