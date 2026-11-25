//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPushDelivery — сообщение в комнате уходит на http-pusher участника.
func TestPushDelivery(t *testing.T) {
	var (
		mu      sync.Mutex
		payload []map[string]any
	)

	pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		payload = append(payload, body)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(pushSrv.Close)

	srv := newIntegrationServer(t)
	// pusher на httptest-сервере (loopback) допустим только в тестовом режиме.
	srv.cfg.Push.AllowHTTP = true
	srv.cfg.Push.AllowPrivate = true
	alice := registerPusherProbe(t, srv, "pushalice", "push-alice-pass")
	bob := registerPusherProbe(t, srv, "pushbob", "push-bob-pass")

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Комната и участники.
	rec := do(alice, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"push","preset":"public_chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec = do(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("bob join: %d", rec.Code)
	}

	// bob регистрирует pusher на тестовый gateway.
	if rec = do(bob, http.MethodPost, "/_matrix/client/v3/pushers/set",
		`{"app_id":"com.example","pushkey":"key1","kind":"http","app_display_name":"App","lang":"en","data":{"url":"`+pushSrv.URL+pushNotifyPath+`"}}`); rec.Code != http.StatusOK {
		t.Fatalf("set pusher: %d %s", rec.Code, rec.Body.String())
	}

	// alice пишет — bob должен получить push.
	if rec = do(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/p1",
		`{"msgtype":"m.text","body":"привет"}`); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(payload)
		mu.Unlock()

		if n > 0 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(payload) == 0 {
		t.Fatal("push не доставлен")
	}

	notif, _ := payload[0]["notification"].(map[string]any)
	if notif["room_id"] != created.RoomID {
		t.Fatalf("room_id в push = %v", notif["room_id"])
	}

	if notif["sender"] != "@pushalice:test.local" {
		t.Fatalf("sender в push = %v", notif["sender"])
	}
}
