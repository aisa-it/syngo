//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type pushEvalEnv struct {
	t   *testing.T
	srv *Server
}

func (e pushEvalEnv) do(token, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	e.srv.Handler().ServeHTTP(rec, req)

	return rec
}

func (e pushEvalEnv) must(token, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()

	rec := e.do(token, method, path, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}

	return rec
}

func (e pushEvalEnv) room(creator string, members ...string) string {
	e.t.Helper()

	rec := e.must(creator, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for _, m := range members {
		e.must(m, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`)
	}

	return created.RoomID
}

func (e pushEvalEnv) send(token, roomID, txn, content string) string {
	e.t.Helper()

	rec := e.must(token, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/"+txn, content)

	var sent struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sent)

	return sent.EventID
}

func (e pushEvalEnv) mute(token, roomID string) {
	e.t.Helper()
	e.must(token, http.MethodPut, "/_matrix/client/v3/pushrules/global/override/"+url.PathEscape(roomID),
		`{"conditions":[{"kind":"event_match","key":"room_id","pattern":"`+roomID+`"}],"actions":[]}`)
}

func (e pushEvalEnv) unreadCounts(token, roomID string) (unreadCounts, bool) {
	e.t.Helper()

	rec := e.must(token, http.MethodGet, "/_matrix/client/v3/sync", "")

	var resp struct {
		Rooms struct {
			Join map[string]struct {
				Unread *unreadCounts `json:"unread_notifications"`
			} `json:"join"`
		} `json:"rooms"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("decode sync: %v", err)
	}

	room, ok := resp.Rooms.Join[roomID]
	if !ok || room.Unread == nil {
		return unreadCounts{}, false
	}

	return *room.Unread, true
}

// TestPushMutedRoom - в заглушенной комнате push нет; после
// снятия mute push приходит с бейджем и tweaks из правила.
func TestPushMutedRoom(t *testing.T) {
	var (
		mu       sync.Mutex
		payloads []map[string]any
	)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()
	}))
	t.Cleanup(gateway.Close)

	srv := newIntegrationServer(t)
	srv.cfg.Push.AllowHTTP = true
	srv.cfg.Push.AllowPrivate = true
	env := pushEvalEnv{t: t, srv: srv}

	alice := registerPusherProbe(t, srv, "mutealice", "mute-alice-pass")
	bob := registerPusherProbe(t, srv, "mutebob", "mute-bob-pass")
	roomID := env.room(alice, bob)

	env.must(bob, http.MethodPost, "/_matrix/client/v3/pushers/set",
		`{"app_id":"com.example","pushkey":"k1","kind":"http","app_display_name":"App","lang":"en","data":{"url":"`+gateway.URL+pushNotifyPath+`","format":"event_id_only"}}`)

	env.mute(bob, roomID)
	muted := env.send(alice, roomID, "m1", `{"msgtype":"m.text","body":"в заглушенной"}`)

	env.must(bob, http.MethodDelete, "/_matrix/client/v3/pushrules/global/override/"+url.PathEscape(roomID), "")
	loud := env.send(alice, roomID, "m2", `{"msgtype":"m.text","body":"после mute"}`)

	waitFor := func(eventID string) map[string]any {
		deadline := time.Now().Add(3 * time.Second)

		for time.Now().Before(deadline) {
			mu.Lock()
			for _, p := range payloads {
				if n, _ := p["notification"].(map[string]any); n["event_id"] == eventID {
					mu.Unlock()

					return n
				}
			}
			mu.Unlock()

			time.Sleep(50 * time.Millisecond)
		}

		return nil
	}

	notif := waitFor(loud)
	if notif == nil {
		t.Fatal("push после снятия mute не пришел")
	}

	// Рассылки асинхронные: даем первой время дойти, если она есть.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	for _, p := range payloads {
		if n, _ := p["notification"].(map[string]any); n["event_id"] == muted {
			mu.Unlock()
			t.Fatalf("push из заглушенной комнаты: %v", n)
		}
	}
	mu.Unlock()

	// event_id_only: содержимое не уходит; в личке - звук из .m.rule.room_one_to_one.
	if _, ok := notif["content"]; ok {
		t.Fatalf("content при format=event_id_only: %v", notif)
	}

	devices, _ := notif["devices"].([]any)
	if len(devices) != 1 {
		t.Fatalf("devices: %v", notif)
	}

	dev, _ := devices[0].(map[string]any)
	tweaks, _ := dev["tweaks"].(map[string]any)

	if tweaks["sound"] != "default" || notif["prio"] != "high" {
		t.Fatalf("tweaks/prio: %v", notif)
	}

	if c, _ := notif["counts"].(map[string]any); c["unread"] != float64(1) {
		t.Fatalf("counts.unread: %v", notif["counts"])
	}
}

// TestSyncUnreadNotifications - unread_notifications в /sync
// по push-правилам после отметки прочтения.
func TestSyncUnreadNotifications(t *testing.T) {
	srv := newIntegrationServer(t)
	env := pushEvalEnv{t: t, srv: srv}

	bob := registerPusherProbe(t, srv, "unreadbob", "unread-bob-pass")
	alice := registerPusherProbe(t, srv, "unreadalice", "unread-alice-pass")
	carol := registerPusherProbe(t, srv, "unreadcarol", "unread-carol-pass")
	roomID := env.room(bob, alice, carol)

	env.send(alice, roomID, "u1", `{"msgtype":"m.text","body":"первое"}`)
	second := env.send(alice, roomID, "u2", `{"msgtype":"m.text","body":"второе"}`)
	env.send(alice, roomID, "u3", `{"msgtype":"m.notice","body":"бот"}`)
	env.send(carol, roomID, "u4", `{"msgtype":"m.text","body":"unreadbob, глянь"}`)

	got, ok := env.unreadCounts(bob, roomID)
	if !ok {
		t.Fatal("нет unread_notifications в /sync")
	}

	if got != (unreadCounts{Notification: 3, Highlight: 1}) {
		t.Fatalf("до прочтения: %+v, ожидали 3/1", got)
	}

	env.must(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read/"+url.PathEscape(second), `{}`)

	if got, _ = env.unreadCounts(bob, roomID); got != (unreadCounts{Notification: 1, Highlight: 1}) {
		t.Fatalf("после receipt: %+v, ожидали 1/1", got)
	}

	// Свое сообщение - неявная отметка прочтения.
	env.send(bob, roomID, "u5", `{"msgtype":"m.text","body":"ок"}`)

	if got, _ = env.unreadCounts(bob, roomID); got != (unreadCounts{}) {
		t.Fatalf("после своего сообщения: %+v", got)
	}

	// Заглушенная комната - не считается.
	env.mute(bob, roomID)
	env.send(alice, roomID, "u6", `{"msgtype":"m.text","body":"тихо"}`)

	if got, _ = env.unreadCounts(bob, roomID); got != (unreadCounts{}) {
		t.Fatalf("в заглушенной комнате: %+v", got)
	}

	// У carol свое сообщение не считается, следующие за ним bob и alice - да.
	if got, _ = env.unreadCounts(carol, roomID); got.Notification != 2 {
		t.Fatalf("у carol: %+v, ожидали 2", got)
	}
}

// TestNotificationsByRules - /notifications отдает только события,
// для которых правила дали notify, с actions из правила и полем read.
func TestNotificationsByRules(t *testing.T) {
	srv := newIntegrationServer(t)
	env := pushEvalEnv{t: t, srv: srv}

	bob := registerPusherProbe(t, srv, "nrbob", "nr-bob-pass")
	alice := registerPusherProbe(t, srv, "nralice", "nr-alice-pass")
	carol := registerPusherProbe(t, srv, "nrcarol", "nr-carol-pass")
	loudRoom := env.room(bob, alice, carol)
	mutedRoom := env.room(bob, alice)

	env.mute(bob, mutedRoom)

	plain := env.send(alice, loudRoom, "n1", `{"msgtype":"m.text","body":"просто"}`)
	env.send(alice, loudRoom, "n2", `{"msgtype":"m.notice","body":"бот"}`)
	mention := env.send(alice, loudRoom, "n3", `{"msgtype":"m.text","body":"nrbob, смотри"}`)
	env.send(alice, mutedRoom, "n4", `{"msgtype":"m.text","body":"в заглушенной"}`)

	env.must(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+loudRoom+"/receipt/m.read/"+url.PathEscape(plain), `{}`)

	type item struct {
		Actions []json.RawMessage `json:"actions"`
		Read    bool              `json:"read"`
		RoomID  string            `json:"room_id"`
		Event   struct {
			EventID string `json:"event_id"`
		} `json:"event"`
	}

	fetch := func(query string) ([]item, string) {
		rec := env.must(bob, http.MethodGet, "/_matrix/client/v3/notifications"+query, "")

		var resp struct {
			Notifications []item `json:"notifications"`
			NextToken     string `json:"next_token"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		return resp.Notifications, resp.NextToken
	}

	all, _ := fetch("")
	if len(all) != 2 || all[0].Event.EventID != mention || all[1].Event.EventID != plain {
		t.Fatalf("ожидали [упоминание, обычное] от новых к старым: %+v", all)
	}

	if all[0].Read || !all[1].Read {
		t.Fatalf("read: %+v", all)
	}

	if !strings.Contains(string(all[0].Actions[len(all[0].Actions)-1]), "highlight") {
		t.Fatalf("actions упоминания: %+v", all[0].Actions)
	}

	only, _ := fetch("?only=highlight")
	if len(only) != 1 || only[0].Event.EventID != mention {
		t.Fatalf("only=highlight: %+v", only)
	}

	page, next := fetch("?limit=1")
	if len(page) != 1 || next == "" {
		t.Fatalf("первая страница: %+v next=%q", page, next)
	}

	page, _ = fetch("?limit=1&from=" + next)
	if len(page) != 1 || page[0].Event.EventID != plain {
		t.Fatalf("вторая страница: %+v", page)
	}
}
