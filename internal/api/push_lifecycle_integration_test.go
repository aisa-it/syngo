//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/pushers"
)

// pushGateway - тестовый шлюз: запоминает уведомления, отвечает handler.
type pushGateway struct {
	*httptest.Server

	mu       sync.Mutex
	received []map[string]any
}

func newPushGateway(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *pushGateway {
	t.Helper()

	g := &pushGateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		g.mu.Lock()
		g.received = append(g.received, body)
		g.mu.Unlock()

		if respond != nil {
			respond(w, r)
		}
	}))
	t.Cleanup(g.Close)

	return g
}

func (g *pushGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.received)
}

func (g *pushGateway) notifyURL() string { return g.URL + pushNotifyPath }

// waitCond ждет условия до timeout.
func waitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(20 * time.Millisecond)
	}

	return cond()
}

func pusherBody(appID, pushkey, url, extra string) string {
	return fmt.Sprintf(`{"app_id":%q,"pushkey":%q,"kind":"http","app_display_name":"App","lang":"en",`+
		`"data":{"url":%q}%s}`, appID, pushkey, url, extra)
}

// listPushkeys - pushkeys пользователя из GET /pushers.
func (e pushEvalEnv) listPushkeys(token string) []string {
	e.t.Helper()

	rec := e.must(token, http.MethodGet, "/_matrix/client/v3/pushers", "")

	var list struct {
		Pushers []pushers.Pusher `json:"pushers"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		e.t.Fatalf("decode pushers: %v", err)
	}

	keys := make([]string, 0, len(list.Pushers))
	for _, p := range list.Pushers {
		keys = append(keys, p.Pushkey)
	}

	return keys
}

// loginDevice входит на устройство deviceID и возвращает токен.
func loginDevice(t *testing.T, srv *Server, username, password, deviceID string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", strings.NewReader(
		`{"type":"m.login.password","user":"`+username+`","password":"`+password+`","device_id":"`+deviceID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", deviceID, rec.Code, rec.Body.String())
	}

	var resp struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	return resp.AccessToken
}

// TestPushSlowGatewayIsolation - недоступный шлюз одного
// пользователя не задерживает push другим.
func TestPushSlowGatewayIsolation(t *testing.T) {
	release := make(chan struct{})
	slow := newPushGateway(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })

	fast := newPushGateway(t, nil)

	srv := newIntegrationServer(t)
	srv.cfg.Push.AllowHTTP = true
	srv.cfg.Push.AllowPrivate = true
	env := pushEvalEnv{t: t, srv: srv}

	alice := registerPusherProbe(t, srv, "isoalice", "iso-alice-pass")
	// Пользователь с зависшим шлюзом идет первым и по алфавиту, и по порядку вставки.
	aaron := registerPusherProbe(t, srv, "isoaaron", "iso-aaron-pass")
	bob := registerPusherProbe(t, srv, "isobob", "iso-bob-pass")
	roomID := env.room(alice, aaron, bob)

	for i := range 3 {
		env.must(aaron, http.MethodPost, "/_matrix/client/v3/pushers/set",
			pusherBody("com.example", fmt.Sprintf("slow%d", i), slow.notifyURL(), ""))
	}

	env.must(bob, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("com.example", "fast", fast.notifyURL(), ""))

	start := time.Now()
	env.send(alice, roomID, "iso1", `{"msgtype":"m.text","body":"всем"}`)

	if !waitCond(2*time.Second, func() bool { return fast.count() == 1 }) {
		t.Fatalf("push на рабочий шлюз не пришел за %v: зависший шлюз блокирует остальных", time.Since(start))
	}
}

// TestPusherLifecycle - logout, logout/all, удаление
// устройства удаляют pushers; append; лимит; путь URL шлюза.
func TestPusherLifecycle(t *testing.T) {
	srv := newIntegrationServer(t)
	env := pushEvalEnv{t: t, srv: srv}

	const pass = "lifecycle-pass-1"

	registerPusherProbe(t, srv, "lifeuser", pass)
	keeper := loginDevice(t, srv, "lifeuser", pass, "KEEP")
	url := "https://push.example" + pushNotifyPath

	// logout устройства удаляет его pushers, но не чужие.
	phone := loginDevice(t, srv, "lifeuser", pass, "PHONE")
	env.must(phone, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "phone-key", url, ""))
	env.must(keeper, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "keep-key", url, ""))
	env.must(phone, http.MethodPost, "/_matrix/client/v3/logout", `{}`)

	if keys := env.listPushkeys(keeper); len(keys) != 1 || keys[0] != "keep-key" {
		t.Fatalf("после logout PHONE: %v, ожидали [keep-key]", keys)
	}

	// Удаление устройства (UIA паролем) удаляет его pushers.
	tablet := loginDevice(t, srv, "lifeuser", pass, "TABLET")
	env.must(tablet, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "tablet-key", url, ""))
	env.must(keeper, http.MethodDelete, "/_matrix/client/v3/devices/TABLET",
		`{"auth":{"type":"m.login.password","identifier":{"type":"m.id.user","user":"lifeuser"},"password":"`+pass+`"}}`)

	if keys := env.listPushkeys(keeper); len(keys) != 1 || keys[0] != "keep-key" {
		t.Fatalf("после удаления TABLET: %v", keys)
	}

	// Путь шлюза - только /_matrix/push/v1/notify.
	if rec := env.do(keeper, http.MethodPost, "/_matrix/client/v3/pushers/set",
		pusherBody("app", "bad-path", "https://push.example/any/path", "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("произвольный путь: %d %s", rec.Code, rec.Body.String())
	}

	// append=false (по умолчанию): тот же app_id+pushkey у другого пользователя
	// удаляется; append=true - остается.
	other := registerPusherProbe(t, srv, "lifeother", "lifeother-pass")
	env.must(other, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "keep-key", url, `,"append":true`))

	if keys := env.listPushkeys(keeper); len(keys) != 1 {
		t.Fatalf("append=true удалил pusher первого пользователя: %v", keys)
	}

	env.must(other, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "keep-key", url, ""))

	if keys := env.listPushkeys(keeper); len(keys) != 0 {
		t.Fatalf("append=false не удалил pusher прежнего пользователя: %v", keys)
	}

	// Лимит числа pushers.
	for i := range pushers.MaxPerUser - 1 {
		env.must(other, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", fmt.Sprintf("k%d", i), url, ""))
	}

	if rec := env.do(other, http.MethodPost, "/_matrix/client/v3/pushers/set",
		pusherBody("app", "one-too-many", url, "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("сверх лимита: %d %s", rec.Code, rec.Body.String())
	}

	// Обновление существующего в пределах лимита проходит.
	env.must(other, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("app", "k0", url, ""))

	// logout/all удаляет все pushers пользователя.
	env.must(other, http.MethodPost, "/_matrix/client/v3/logout/all", `{}`)

	otherAgain := loginDevice(t, srv, "lifeother", "lifeother-pass", "NEW")
	if keys := env.listPushkeys(otherAgain); len(keys) != 0 {
		t.Fatalf("после logout/all осталось %d pushers", len(keys))
	}
}

// TestPushRejectedRemovesPusher - pushkey из rejected в ответе
// шлюза удаляет pusher.
func TestPushRejectedRemovesPusher(t *testing.T) {
	gateway := newPushGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rejected":["dead-key"]}`))
	})

	srv := newIntegrationServer(t)
	srv.cfg.Push.AllowHTTP = true
	srv.cfg.Push.AllowPrivate = true
	env := pushEvalEnv{t: t, srv: srv}

	alice := registerPusherProbe(t, srv, "rejalice", "rej-alice-pass")
	bob := registerPusherProbe(t, srv, "rejbob", "rej-bob-pass")
	roomID := env.room(alice, bob)

	env.must(bob, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("com.example", "dead-key", gateway.notifyURL(), ""))
	env.send(alice, roomID, "rej1", `{"msgtype":"m.text","body":"привет"}`)

	if !waitCond(3*time.Second, func() bool { return len(env.listPushkeys(bob)) == 0 }) {
		t.Fatalf("pusher из rejected не удален: %v (запросов на шлюз %d)", env.listPushkeys(bob), gateway.count())
	}
}

// TestPushInvite - приглашение дает push приглашенному
// (.m.rule.invite_for_me), хотя он еще не участник комнаты.
func TestPushInvite(t *testing.T) {
	gateway := newPushGateway(t, nil)

	srv := newIntegrationServer(t)
	srv.cfg.Push.AllowHTTP = true
	srv.cfg.Push.AllowPrivate = true
	env := pushEvalEnv{t: t, srv: srv}

	alice := registerPusherProbe(t, srv, "invalice", "inv-alice-pass")
	bob := registerPusherProbe(t, srv, "invbob", "inv-bob-pass")
	roomID := env.room(alice)

	env.must(bob, http.MethodPost, "/_matrix/client/v3/pushers/set", pusherBody("com.example", "k", gateway.notifyURL(), ""))
	env.must(alice, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/invite", `{"user_id":"@invbob:test.local"}`)

	if !waitCond(3*time.Second, func() bool { return gateway.count() == 1 }) {
		t.Fatal("push о приглашении не пришел")
	}

	gateway.mu.Lock()
	notif, _ := gateway.received[0]["notification"].(map[string]any)
	gateway.mu.Unlock()

	if notif["type"] != "m.room.member" || notif["room_id"] != roomID {
		t.Fatalf("push о приглашении: %v", notif)
	}
}
