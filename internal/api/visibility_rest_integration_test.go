//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// hvFixture — комната и участники для проверок history_visibility вне /sync и
// /messages.
type hvFixture struct {
	t      *testing.T
	server *Server
	alice  string
	bob    string
}

func newHVFixture(t *testing.T, prefix string) *hvFixture {
	t.Helper()

	server := newIntegrationServer(t)

	return &hvFixture{
		t:      t,
		server: server,
		alice:  registerHTTP(t, server, prefix+"alice", "ADEV"),
		bob:    registerHTTP(t, server, prefix+"bob", "BDEV"),
	}
}

func (f *hvFixture) createRoom(visibility string) string {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","initial_state":[{"type":"m.room.history_visibility","state_key":"","content":{"history_visibility":"`+visibility+`"}}]}`, f.alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		f.t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	return created.RoomID
}

func (f *hvFixture) send(tok, roomID, txn, content string) string {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodPut,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/send/m.room.message/"+txn, content, tok)

	var resp struct {
		EventID string `json:"event_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.EventID == "" {
		f.t.Fatalf("send %s: %d %s", txn, rec.Code, rec.Body.String())
	}

	return resp.EventID
}

func (f *hvFixture) text(roomID, txn, body string) string {
	return f.send(f.alice, roomID, txn, `{"msgtype":"m.text","body":"`+body+`"}`)
}

func (f *hvFixture) setVisibility(roomID, visibility string) {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodPut,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/state/m.room.history_visibility/",
		`{"history_visibility":"`+visibility+`"}`, f.alice)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("history_visibility: %d %s", rec.Code, rec.Body.String())
	}
}

func (f *hvFixture) join(tok, roomID string) {
	f.t.Helper()

	if rec := do(f.t, f.server, http.MethodPost,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/join", `{}`, tok); rec.Code != http.StatusOK {
		f.t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}
}

func (f *hvFixture) get(tok, path string) (int, string) {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodGet, path, "", tok)

	return rec.Code, rec.Body.String()
}

func (f *hvFixture) search(tok, term string) string {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodPost, "/_matrix/client/v3/search",
		`{"search_categories":{"room_events":{"search_term":"`+term+`"}}}`, tok)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("search %s: %d %s", term, rec.Code, rec.Body.String())
	}

	return rec.Body.String()
}

// TestHistoryVisibilityContext — /context при history_visibility joined: событие до
// входа - 404, в events_before его нет; само событие не дублируется в
// events_before/events_after.
func TestHistoryVisibilityContext(t *testing.T) {
	f := newHVFixture(t, "hvctx")
	room := f.createRoom("joined")
	secret := f.text(room, "c1", "ctxsecret")

	f.join(f.bob, room)

	visible := f.text(room, "c2", "ctxvisible")
	f.text(room, "c3", "ctxafter")

	base := "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/context/"

	if code, body := f.get(f.bob, base+url.PathEscape(secret)); code != http.StatusNotFound {
		t.Fatalf("context события до входа: %d %s, ожидали 404", code, body)
	}

	code, body := f.get(f.bob, base+url.PathEscape(visible)+"?limit=50")
	if code != http.StatusOK {
		t.Fatalf("context: %d %s", code, body)
	}

	if strings.Contains(body, "ctxsecret") {
		t.Fatalf("events_before с событием до входа: %s", body)
	}

	var resp struct {
		Before []struct {
			EventID string `json:"event_id"`
		} `json:"events_before"`
		After []struct {
			EventID string `json:"event_id"`
		} `json:"events_after"`
		Event struct {
			EventID string `json:"event_id"`
		} `json:"event"`
		State []json.RawMessage `json:"state"`
	}

	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Event.EventID != visible || len(resp.After) == 0 || len(resp.Before) == 0 || len(resp.State) == 0 {
		t.Fatalf("context без события, окружения или состояния: %s", body)
	}

	for _, list := range [][]struct {
		EventID string `json:"event_id"`
	}{resp.Before, resp.After} {
		for _, ev := range list {
			if ev.EventID == visible {
				t.Fatalf("целевое событие продублировано в events_before/after: %s", body)
			}
		}
	}

	// Создатель видит всю историю.
	if code, body := f.get(f.alice, base+url.PathEscape(visible)+"?limit=50"); code != http.StatusOK ||
		!strings.Contains(body, "ctxsecret") {
		t.Fatalf("context создателя: %d %s", code, body)
	}
}

// TestHistoryVisibilityRelations — /relations и /threads: связи скрытого события
// - 404; связи, отправленные в скрытый промежуток, не отдаются; удаленная связь не
// отдается никому.
func TestHistoryVisibilityRelations(t *testing.T) {
	f := newHVFixture(t, "hvrel")
	room := f.createRoom("shared")
	// Родитель отправлен при shared - виден всем.
	parent := f.text(room, "r1", "relparent")

	f.setVisibility(room, "joined")

	hiddenParent := f.text(room, "r2", "relhiddenparent")
	f.send(f.alice, room, "r3",
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"`+parent+`","key":"hiddenkey"}}`)
	f.send(f.alice, room, "r4",
		`{"msgtype":"m.text","body":"hiddenreply","m.relates_to":{"rel_type":"m.thread","event_id":"`+parent+`"}}`)
	f.send(f.alice, room, "r5",
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"`+hiddenParent+`","key":"k"}}`)

	f.join(f.bob, room)

	f.send(f.alice, room, "r6",
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"`+parent+`","key":"visiblekey"}}`)
	f.send(f.alice, room, "r7",
		`{"msgtype":"m.text","body":"visiblereply","m.relates_to":{"rel_type":"m.thread","event_id":"`+parent+`"}}`)

	redacted := f.send(f.alice, room, "r8",
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"`+parent+`","key":"redactedkey"}}`)

	if rec := do(t, f.server, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/redact/"+
		url.PathEscape(redacted)+"/rd1", `{}`, f.alice); rec.Code != http.StatusOK {
		t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}

	roomPath := url.PathEscape(room)

	for _, path := range []string{
		"/_matrix/client/v1/rooms/" + roomPath + "/relations/" + url.PathEscape(parent),
		"/_matrix/client/v1/rooms/" + roomPath + "/relations/" + url.PathEscape(parent) + "/m.annotation",
		"/_matrix/client/v1/rooms/" + roomPath + "/relations/" + url.PathEscape(parent) + "/m.annotation/m.room.message",
		"/_matrix/client/v3/rooms/" + roomPath + "/relations/" + url.PathEscape(parent) + "?limit=50",
	} {
		code, body := f.get(f.bob, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}

		if strings.Contains(body, "hiddenkey") || strings.Contains(body, "hiddenreply") || !strings.Contains(body, "visiblekey") {
			t.Fatalf("%s: связи из скрытого промежутка или нет видимой: %s", path, body)
		}

		if strings.Contains(body, redacted) {
			t.Fatalf("%s: удаленная связь в выдаче: %s", path, body)
		}
	}

	code, body := f.get(f.bob, "/_matrix/client/v1/rooms/"+roomPath+"/threads/"+url.PathEscape(parent))
	if code != http.StatusOK || strings.Contains(body, "hiddenreply") || !strings.Contains(body, "visiblereply") {
		t.Fatalf("thread: %d %s", code, body)
	}

	for _, path := range []string{
		"/_matrix/client/v1/rooms/" + roomPath + "/relations/" + url.PathEscape(hiddenParent),
		"/_matrix/client/v1/rooms/" + roomPath + "/threads/" + url.PathEscape(hiddenParent),
	} {
		if code, body := f.get(f.bob, path); code != http.StatusNotFound {
			t.Fatalf("%s: %d %s, ожидали 404", path, code, body)
		}
	}

	// Автор видит все связи, кроме удаленной.
	_, body = f.get(f.alice, "/_matrix/client/v1/rooms/"+roomPath+"/relations/"+url.PathEscape(parent)+"?limit=50")
	if !strings.Contains(body, "hiddenkey") || strings.Contains(body, redacted) {
		t.Fatalf("relations автора: %s", body)
	}
}

// TestHistoryVisibilitySearch — поиск учитывает history_visibility для shared,
// world_readable, invited, joined и не находит удаленные сообщения.
func TestHistoryVisibilitySearch(t *testing.T) {
	f := newHVFixture(t, "hvsrch")

	shared := f.createRoom("shared")
	f.text(shared, "s1", "srchshared")

	readable := f.createRoom("world_readable")
	f.text(readable, "w1", "srchreadable")

	joined := f.createRoom("joined")
	f.text(joined, "j1", "srchjoinedbefore")

	invited := f.createRoom("invited")
	f.text(invited, "i1", "srchinvitedbefore")

	if rec := do(t, f.server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(invited)+"/invite",
		`{"user_id":"@hvsrchbob:test.local"}`, f.alice); rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	f.text(invited, "i2", "srchinvitedafter")

	for _, room := range []string{shared, readable, joined, invited} {
		f.join(f.bob, room)
	}

	f.text(joined, "j2", "srchjoinedafter")

	for term, want := range map[string]bool{
		"srchshared":        true,
		"srchreadable":      true,
		"srchjoinedbefore":  false,
		"srchjoinedafter":   true,
		"srchinvitedbefore": false,
		"srchinvitedafter":  true,
	} {
		// highlights повторяет запрос, поэтому ищем именно body результата.
		if got := strings.Contains(f.search(f.bob, term), `"body":"`+term+`"`); got != want {
			t.Errorf("поиск %q: найдено=%v, ожидали %v", term, got, want)
		}
	}

	if !strings.Contains(f.search(f.alice, "srchjoinedbefore"), `"body":"srchjoinedbefore"`) {
		t.Error("автор не находит свое сообщение")
	}

	// Удаленное сообщение не находится по исходному тексту.
	gone := f.text(shared, "s2", "srchredactedsecret")

	if rec := do(t, f.server, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(shared)+"/redact/"+
		url.PathEscape(gone)+"/rd1", `{}`, f.alice); rec.Code != http.StatusOK {
		t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}

	for _, tok := range []string{f.alice, f.bob} {
		if body := f.search(tok, "srchredactedsecret"); strings.Contains(body, gone) {
			t.Fatalf("поиск нашел удаленное сообщение: %s", body)
		}
	}
}

// TestHistoryVisibilityLeftMember (Complement TestLeftRoomFixture):
// при history_visibility joined вышедший видит в /messages события от своего
// входа до выхода, но не до входа и не после выхода.
func TestHistoryVisibilityLeftMember(t *testing.T) {
	f := newHVFixture(t, "hvleft")
	roomID := f.createRoom("joined")

	f.text(roomID, "before-join", "before-join")
	f.join(f.bob, roomID)
	f.text(roomID, "while-joined", "while-joined")

	if rec := do(t, f.server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/leave", `{}`, f.bob); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	f.text(roomID, "after-leave", "after-leave")

	code, body := f.get(f.bob, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/messages?dir=b&limit=50")
	if code != http.StatusOK {
		t.Fatalf("messages: %d %s", code, body)
	}

	if !strings.Contains(body, "while-joined") || strings.Contains(body, "before-join") || strings.Contains(body, "after-leave") {
		t.Fatalf("история вышедшего: %s", body)
	}
}
