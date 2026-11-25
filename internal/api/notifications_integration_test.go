//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNotifications — чужие сообщения в комнате попадают в /notifications.
func TestNotifications(t *testing.T) {
	srv := newIntegrationServer(t)

	alice := registerPusherProbe(t, srv, "notifalice", "notif-alice-pass")
	bob := registerPusherProbe(t, srv, "notifbob", "notif-bob-pass")

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Публичная комната: alice создаёт, bob вступает и пишет.
	rec := do(alice, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"notif","preset":"public_chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec = do(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("bob join: %d %s", rec.Code, rec.Body.String())
	}

	if rec = do(bob, http.MethodPut, "/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/n1",
		`{"msgtype":"m.text","body":"привет alice"}`); rec.Code != http.StatusOK {
		t.Fatalf("bob send: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(alice, http.MethodGet, "/_matrix/client/v3/notifications", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("notifications: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Notifications []struct {
			RoomID string `json:"room_id"`
			Read   bool   `json:"read"`
			Event  struct {
				Sender  string `json:"sender"`
				Content struct {
					Body string `json:"body"`
				} `json:"content"`
			} `json:"event"`
		} `json:"notifications"`
		NextToken string `json:"next_token"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	found := false

	for _, n := range resp.Notifications {
		if n.RoomID == created.RoomID && n.Event.Sender == "@notifbob:test.local" {
			found = true

			if n.Event.Content.Body != "привет alice" {
				t.Fatalf("body = %q", n.Event.Content.Body)
			}

			if n.Read {
				t.Fatal("новое уведомление помечено прочитанным")
			}
		}
	}

	if !found {
		t.Fatalf("уведомление от bob не найдено: %s", rec.Body.String())
	}

	// Собственные сообщения alice в уведомления не попадают.
	if rec = do(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/a1",
		`{"msgtype":"m.text","body":"моё"} `); rec.Code != http.StatusOK {
		t.Fatalf("alice send: %d", rec.Code)
	}

	rec = do(alice, http.MethodGet, "/_matrix/client/v3/notifications?from=0", "")
	if strings.Contains(rec.Body.String(), "моё") {
		t.Fatalf("своё сообщение попало в уведомления: %s", rec.Body.String())
	}
}
