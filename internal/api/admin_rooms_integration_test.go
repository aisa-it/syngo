//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adminEnv - сервер с включенным admin API и помощники запросов.
type adminEnv struct {
	t   *testing.T
	srv *Server
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()

	srv := newIntegrationServer(t)
	srv.cfg.Admin.Enabled = true

	return &adminEnv{t: t, srv: srv}
}

func (e *adminEnv) do(token, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	req.Header.Set("Content-Type", "application/json")
	e.srv.Handler().ServeHTTP(rec, req)

	return rec
}

func (e *adminEnv) exec(query string, args ...any) {
	e.t.Helper()

	if _, err := e.srv.db.Pool().Exec(context.Background(), query, args...); err != nil {
		e.t.Fatalf("exec %q: %v", query, err)
	}
}

func (e *adminEnv) count(query string, args ...any) int {
	e.t.Helper()

	var n int
	if err := e.srv.db.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", query, err)
	}

	return n
}

// admin регистрирует пользователя, выдает роль и возвращает (CS-токен, admin-токен).
func (e *adminEnv) admin(localpart, password string) (string, string) {
	e.t.Helper()

	userToken := registerPusherProbe(e.t, e.srv, localpart, password)
	e.exec(`UPDATE users SET is_admin = true WHERE localpart = $1`, localpart)

	rec := e.do("", http.MethodPost, "/_syngo/admin/v1/login",
		`{"username":"`+localpart+`","password":"`+password+`"}`)

	var login struct {
		AdminToken string `json:"admin_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	if rec.Code != http.StatusOK || login.AdminToken == "" {
		e.t.Fatalf("admin login %s: %d %s", localpart, rec.Code, rec.Body.String())
	}

	return userToken, login.AdminToken
}

func (e *adminEnv) createRoom(token, body string) string {
	e.t.Helper()

	rec := e.do(token, http.MethodPost, "/_matrix/client/v3/createRoom", body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	return created.RoomID
}

// TestAdminBlockedRoomRejectsWrites - в заблокированной комнате send, state, invite
// и join - 403; блокировку видно через GET .../block, PUT {"block":false} снимает ее,
// и запись снова проходит; оба действия в аудите с результатом. Раньше
// блокировка проверялась только в /join.
func TestAdminBlockedRoomRejectsWrites(t *testing.T) {
	env := newAdminEnv(t)

	_, adminToken := env.admin("blockadm", "blockadm-password-1")
	owner := registerPusherProbe(t, env.srv, "blockowner", "blockowner-password-1")
	outsider := registerPusherProbe(t, env.srv, "blockout", "blockout-password-1")

	roomID := env.createRoom(owner, `{"preset":"public_chat"}`)
	base := "/_matrix/client/v3/rooms/" + roomID

	// Блокировка прежним маршрутом DELETE без purge.
	if rec := env.do(adminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+roomID, ""); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}

	writes := []struct{ name, token, method, path, body string }{
		{"send", owner, http.MethodPut, base + "/send/m.room.message/txn-blocked", `{"msgtype":"m.text","body":"hi"}`},
		{"state", owner, http.MethodPut, base + "/state/m.room.topic/", `{"topic":"x"}`},
		{"invite", owner, http.MethodPost, base + "/invite", `{"user_id":"@blockout:test.local"}`},
		{"join", outsider, http.MethodPost, base + "/join", `{}`},
	}

	for _, w := range writes {
		if rec := env.do(w.token, w.method, w.path, w.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s в заблокированной комнате: %d %s, ожидали 403", w.name, rec.Code, rec.Body.String())
		}
	}

	rec := env.do(adminToken, http.MethodGet, "/_syngo/admin/v1/rooms/"+roomID+"/block", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"block":true`) ||
		!strings.Contains(rec.Body.String(), "@blockadm:test.local") {
		t.Fatalf("block status: %d %s", rec.Code, rec.Body.String())
	}

	// Повторная блокировка через PUT - не ошибка.
	if rec := env.do(adminToken, http.MethodPut, "/_syngo/admin/v1/rooms/"+roomID+"/block", `{"block":true}`); rec.Code != http.StatusOK {
		t.Fatalf("повторный block: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(adminToken, http.MethodPut, "/_syngo/admin/v1/rooms/"+roomID+"/block", `{"block":false}`); rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(owner, http.MethodPut, base+"/send/m.room.message/txn-unblocked",
		`{"msgtype":"m.text","body":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("send после разблокировки: %d %s", rec.Code, rec.Body.String())
	}

	for action, want := range map[string]int{"room_block": 2, "room_unblock": 1} {
		if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = $1 AND target = $2
			AND actor = '@blockadm:test.local' AND details->>'result' = 'ok'`, action, roomID); n != want {
			t.Errorf("аудит %s: %d записей, ожидали %d", action, n, want)
		}
	}
}

// TestAdminPurgeKeepsBlock - блокировка переживает purge: после удаления комната
// по-прежнему заблокирована; purge с block=true блокирует незаблокированную
// комнату; /sync бывшего участника после purge отвечает 200 без комнаты.
func TestAdminPurgeKeepsBlock(t *testing.T) {
	env := newAdminEnv(t)

	_, adminToken := env.admin("purgeadm", "purgeadm-password-1")
	owner := registerPusherProbe(t, env.srv, "purgeowner", "purgeowner-password-1")
	member := registerPusherProbe(t, env.srv, "purgemember", "purgemember-password-1")

	blockedRoom := env.createRoom(owner, `{"preset":"public_chat"}`)
	plainRoom := env.createRoom(owner, `{"preset":"public_chat"}`)

	for _, roomID := range []string{blockedRoom, plainRoom} {
		if rec := env.do(member, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", `{}`); rec.Code != http.StatusOK {
			t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := env.do(member, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sync до purge: %d %s", rec.Code, rec.Body.String())
	}

	var before struct {
		NextBatch string `json:"next_batch"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &before)

	if rec := env.do(adminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+blockedRoom, ""); rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(adminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+blockedRoom+"?purge=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("purge заблокированной: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(adminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+plainRoom+"?purge=true&block=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("purge с block=true: %d %s", rec.Code, rec.Body.String())
	}

	for _, roomID := range []string{blockedRoom, plainRoom} {
		rec := env.do(adminToken, http.MethodGet, "/_syngo/admin/v1/rooms/"+roomID+"/block", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"block":true`) {
			t.Errorf("блокировка %s после purge: %d %s", roomID, rec.Code, rec.Body.String())
		}

		if n := env.count(`SELECT count(*) FROM events WHERE room_id = $1`, roomID); n != 0 {
			t.Errorf("после purge %s осталось %d событий", roomID, n)
		}

		if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = 'room_purge' AND target = $1 AND details->>'result' = 'ok'`, roomID); n != 1 {
			t.Errorf("аудит purge %s: %d записей", roomID, n)
		}
	}

	// Повторный purge удаленной комнаты - 404 и запись об отказе в аудите.
	if rec := env.do(adminToken, http.MethodDelete, "/_syngo/admin/v1/rooms/"+plainRoom+"?purge=true", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("повторный purge: %d %s", rec.Code, rec.Body.String())
	}

	if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = 'room_purge' AND target = $1 AND details->>'result' = 'not_found'`, plainRoom); n != 1 {
		t.Errorf("аудит неуспешного purge: %d записей", n)
	}

	for _, path := range []string{"/_matrix/client/v3/sync?timeout=0", "/_matrix/client/v3/sync?timeout=0&since=" + before.NextBatch} {
		rec := env.do(member, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("sync после purge (%s): %d %s", path, rec.Code, rec.Body.String())
		}

		if strings.Contains(rec.Body.String(), blockedRoom) && strings.Contains(rec.Body.String(), `"join":{"`+blockedRoom) {
			t.Fatalf("удаленная комната в join после purge: %s", rec.Body.String())
		}
	}
}

// TestAdminLastAdminRaceAPI - два оператора одновременно снимают роль друг с
// друга: остается ровно один админ, второй запрос - 403.
func TestAdminLastAdminRaceAPI(t *testing.T) {
	env := newAdminEnv(t)

	_, tokenA := env.admin("racea", "racea-password-1")
	_, tokenB := env.admin("raceb", "raceb-password-1")

	for attempt := 1; attempt <= 30; attempt++ {
		env.exec(`UPDATE users SET is_admin = true WHERE localpart IN ('racea','raceb')`)
		// unadmin отзывает admin-токены цели: для следующей попытки возвращаем их.
		env.exec(`UPDATE access_tokens SET revoked_at = NULL WHERE scope = 'admin'`)

		var (
			wg    sync.WaitGroup
			codes [2]int
		)

		start := make(chan struct{})
		reqs := [2][2]string{
			{tokenA, "/_syngo/admin/v1/users/@raceb:test.local/unadmin"},
			{tokenB, "/_syngo/admin/v1/users/@racea:test.local/unadmin"},
		}

		for i := range reqs {
			wg.Add(1)

			go func(i int) {
				defer wg.Done()
				<-start
				codes[i] = env.do(reqs[i][0], http.MethodPost, reqs[i][1], "").Code
			}(i)
		}

		close(start)
		wg.Wait()

		if n := env.count(`SELECT count(*) FROM users WHERE is_admin AND localpart IN ('racea','raceb')`); n != 1 {
			t.Fatalf("попытка %d: админов осталось %d (коды %v), ожидали 1", attempt, n, codes)
		}

		if codes[0] == codes[1] || (codes[0] != http.StatusOK && codes[1] != http.StatusOK) {
			t.Fatalf("попытка %d: коды %v, ожидали 200 и 403", attempt, codes)
		}
	}

	// Отказ guard'а записан в аудит с причиной. Проигравший запрос может получить 403
	// и раньше, в requireAdmin (роль уже снята), поэтому записей не больше числа попыток.
	if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = 'unadmin'
		AND details->>'result' = 'denied' AND details->>'reason' = 'last_admin'`); n == 0 || n > 30 {
		t.Fatalf("аудит отказов last_admin: %d записей", n)
	}
}

// TestAdminAuditAndTokens - аудит: вход, отказ «последний админ» с причиной, сброс
// пароля без секретов; admin-токен отзывается через logout, служебное устройство
// ADMIN не видно в списке устройств; total в /rooms - число всех комнат.
func TestAdminAuditAndTokens(t *testing.T) {
	env := newAdminEnv(t)

	userToken, adminToken := env.admin("auditadm", "auditadm-password-1")
	_ = registerPusherProbe(t, env.srv, "auditvictim", "auditvictim-password-1")

	if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = 'login' AND actor = '@auditadm:test.local' AND details->>'result' = 'ok'`); n != 1 {
		t.Errorf("аудит входа: %d записей", n)
	}

	// Устройство ADMIN не видно в CS-API.
	rec := env.do(userToken, http.MethodGet, "/_matrix/client/v3/devices", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"ADMIN"`) {
		t.Errorf("устройство ADMIN в списке устройств: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(userToken, http.MethodGet, "/_matrix/client/v3/devices/ADMIN", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET /devices/ADMIN: %d %s, ожидали 404", rec.Code, rec.Body.String())
	}

	// Сброс пароля: в аудите нет пароля.
	const secret = "auditvictim-new-secret-1"
	if rec := env.do(adminToken, http.MethodPost, "/_syngo/admin/v1/users/@auditvictim:test.local/password",
		`{"new_password":"`+secret+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("password reset: %d %s", rec.Code, rec.Body.String())
	}

	if n := env.count(`SELECT count(*) FROM admin_audit WHERE details::text LIKE '%' || $1 || '%' OR target LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Errorf("пароль попал в аудит: %d записей", n)
	}

	// Отказы пишутся в аудит с причиной («последний админ» - в TestAdminLastAdminRaceAPI).
	if rec := env.do(adminToken, http.MethodPost, "/_syngo/admin/v1/users/@nobody:test.local/admin", ""); rec.Code != http.StatusNotFound {
		t.Errorf("admin неизвестному: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(adminToken, http.MethodPost, "/_syngo/admin/v1/users/@auditadm:test.local/unadmin", ""); rec.Code != http.StatusForbidden {
		t.Errorf("unadmin себя: %d %s", rec.Code, rec.Body.String())
	}

	for _, want := range []struct{ action, target, result, reason string }{
		{"admin", "@nobody:test.local", "not_found", "not_found"},
		{"unadmin", "@auditadm:test.local", "denied", "self"},
	} {
		if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = $1 AND target = $2
			AND details->>'result' = $3 AND details->>'reason' = $4`, want.action, want.target, want.result, want.reason); n != 1 {
			t.Errorf("аудит отказа %+v: %d записей", want, n)
		}
	}

	// total в /rooms - число всех комнат, а не размер страницы.
	env.createRoom(userToken, `{}`)
	env.createRoom(userToken, `{}`)

	rec = env.do(adminToken, http.MethodGet, "/_syngo/admin/v1/rooms?limit=1", "")

	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)

	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Total != 2 {
		t.Errorf("rooms?limit=1: %d items=%d total=%d", rec.Code, len(page.Items), page.Total)
	}

	// Выход из admin-контура: токен больше не действует, устройство удалено.
	if rec := env.do(adminToken, http.MethodPost, "/_syngo/admin/v1/logout", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin logout: %d %s", rec.Code, rec.Body.String())
	}

	if rec := env.do(adminToken, http.MethodGet, "/_syngo/admin/v1/users", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("admin-токен после logout: %d %s, ожидали 401", rec.Code, rec.Body.String())
	}

	if n := env.count(`SELECT count(*) FROM devices WHERE user_id = '@auditadm:test.local' AND device_id = 'ADMIN'`); n != 0 {
		t.Errorf("устройство ADMIN после logout: %d", n)
	}

	if n := env.count(`SELECT count(*) FROM admin_audit WHERE action = 'logout' AND actor = '@auditadm:test.local'`); n != 1 {
		t.Errorf("аудит logout: %d записей", n)
	}
}
