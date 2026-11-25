//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Интеграционные тесты simplified sliding sync (MSC4186).

const slidingPath = "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync"

type ssEvent struct {
	EventID  string         `json:"event_id"`
	Type     string         `json:"type"`
	Sender   string         `json:"sender"`
	StateKey *string        `json:"state_key"`
	Content  map[string]any `json:"content"`
	Unsigned map[string]any `json:"unsigned"`
}

type ssRoom struct {
	Name   string `json:"name"`
	Heroes []struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"displayname"`
	} `json:"heroes"`
	Initial           bool      `json:"initial"`
	Expanded          bool      `json:"unstable_expanded_timeline"`
	RequiredState     []ssEvent `json:"required_state"`
	Timeline          []ssEvent `json:"timeline"`
	PrevBatch         string    `json:"prev_batch"`
	Limited           *bool     `json:"limited"`
	NumLive           *int      `json:"num_live"`
	BumpStamp         *int64    `json:"bump_stamp"`
	JoinedCount       *int      `json:"joined_count"`
	InvitedCount      *int      `json:"invited_count"`
	NotificationCount *int      `json:"notification_count"`
	IsDM              bool      `json:"is_dm"`
	InviteState       []ssEvent `json:"invite_state"`
}

type ssResp struct {
	Pos   string `json:"pos"`
	Lists map[string]struct {
		Count int `json:"count"`
		Ops   []struct {
			Op      string   `json:"op"`
			Range   [2]int   `json:"range"`
			RoomIDs []string `json:"room_ids"`
		} `json:"ops"`
	} `json:"lists"`
	Rooms      map[string]ssRoom `json:"rooms"`
	Extensions struct {
		ToDevice *struct {
			NextBatch string           `json:"next_batch"`
			Events    []map[string]any `json:"events"`
		} `json:"to_device"`
		E2EE *struct {
			DeviceLists *struct {
				Changed []string `json:"changed"`
				Left    []string `json:"left"`
			} `json:"device_lists"`
			OTK      map[string]int `json:"device_one_time_keys_count"`
			Fallback []string       `json:"device_unused_fallback_key_types"`
		} `json:"e2ee"`
		AccountData *struct {
			Global []struct {
				Type    string         `json:"type"`
				Content map[string]any `json:"content"`
			} `json:"global"`
			Rooms map[string][]struct {
				Type    string         `json:"type"`
				Content map[string]any `json:"content"`
			} `json:"rooms"`
		} `json:"account_data"`
		Receipts *struct {
			Rooms map[string]struct {
				Type    string                               `json:"type"`
				Content map[string]map[string]map[string]any `json:"content"`
			} `json:"rooms"`
		} `json:"receipts"`
		Typing *struct {
			Rooms map[string]struct {
				Type    string `json:"type"`
				Content struct {
					UserIDs []string `json:"user_ids"`
				} `json:"content"`
			} `json:"rooms"`
		} `json:"typing"`
	} `json:"extensions"`
}

// ssEnv - сервер и действия пользователей для тестов sliding sync.
type ssEnv struct {
	t      *testing.T
	server *Server
}

func newSSEnv(t *testing.T) *ssEnv {
	t.Helper()

	return &ssEnv{t: t, server: newIntegrationServer(t)}
}

func (e *ssEnv) must(token, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()

	rec := do(e.t, e.server, method, path, body, token)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}

	return rec
}

func (e *ssEnv) createRoom(token, body string) string {
	e.t.Helper()

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(e.must(token, http.MethodPost, "/_matrix/client/v3/createRoom", body).Body.Bytes(), &created); err != nil {
		e.t.Fatalf("createRoom: %v", err)
	}

	return created.RoomID
}

func (e *ssEnv) send(token, roomID, txn, text string) string {
	e.t.Helper()

	var sent struct {
		EventID string `json:"event_id"`
	}

	rec := e.must(token, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/send/m.room.message/"+txn,
		`{"msgtype":"m.text","body":"`+text+`"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil {
		e.t.Fatalf("send: %v", err)
	}

	return sent.EventID
}

func (e *ssEnv) roomAction(token, roomID, action, body string) {
	e.t.Helper()
	e.must(token, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/"+action, body)
}

// sync - POST sliding sync; код ответа, разобранный ответ и тело.
func (e *ssEnv) sync(token, pos, body string, timeoutMS int) (int, ssResp, string) {
	e.t.Helper()

	path := slidingPath + "?timeout=" + strconv.Itoa(timeoutMS)
	if pos != "" {
		path += "&pos=" + url.QueryEscape(pos)
	}

	rec := do(e.t, e.server, http.MethodPost, path, body, token)

	var resp ssResp

	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			e.t.Fatalf("sliding sync: %v %s", err, rec.Body.String())
		}
	}

	return rec.Code, resp, rec.Body.String()
}

func (e *ssEnv) mustSync(token, pos, body string) ssResp {
	e.t.Helper()

	code, resp, raw := e.sync(token, pos, body, 0)
	if code != http.StatusOK || resp.Pos == "" {
		e.t.Fatalf("sliding sync: %d %s", code, raw)
	}

	return resp
}

func listBody(ranges string, timelineLimit int, requiredState string) string {
	return `{"conn_id":"room-list","lists":{"all":{"ranges":` + ranges + `,"timeline_limit":` +
		strconv.Itoa(timelineLimit) + `,"required_state":` + requiredState + `}}}`
}

func stateOf(events []ssEvent, eventType, stateKey string) *ssEvent {
	for i := range events {
		if events[i].Type == eventType && events[i].StateKey != nil && *events[i].StateKey == stateKey {
			return &events[i]
		}
	}

	return nil
}

func memberKeys(events []ssEvent) []string {
	out := []string{}

	for i := range events {
		if events[i].Type == "m.room.member" && events[i].StateKey != nil {
			out = append(out, *events[i].StateKey)
		}
	}

	return out
}

// TestSlidingSyncInitialListsAndRanges - initial: число комнат списка, окна по
// ranges в порядке bump_stamp, первая отдача комнаты с именем, timeline,
// limited и prev_batch; объявление в /versions.
func TestSlidingSyncInitialListsAndRanges(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "ssalice", "ADEV")

	rec := e.must("", http.MethodGet, "/_matrix/client/versions", "")
	if !strings.Contains(rec.Body.String(), `"org.matrix.simplified_msc3575":true`) {
		t.Fatalf("versions без simplified_msc3575: %s", rec.Body.String())
	}

	rooms := make([]string, 0, 5)
	for i := 1; i <= 5; i++ {
		rooms = append(rooms, e.createRoom(alice, `{"name":"r`+strconv.Itoa(i)+`"}`))
	}

	e.send(alice, rooms[0], "t1", "hello r1")

	resp := e.mustSync(alice, "", listBody(`[[0,1]]`, 1, `[["m.room.name",""]]`))

	list := resp.Lists["all"]
	if list.Count != 5 || len(list.Ops) != 1 || list.Ops[0].Op != "SYNC" {
		t.Fatalf("список: %+v", list)
	}

	if got := list.Ops[0].RoomIDs; len(got) != 2 || got[0] != rooms[0] || got[1] != rooms[4] {
		t.Fatalf("окно [0,1]: %v, ожидали r1 (сообщение), r5 (последняя созданная)", got)
	}

	if len(resp.Rooms) != 2 {
		t.Fatalf("rooms: %d, ожидали 2", len(resp.Rooms))
	}

	r1 := resp.Rooms[rooms[0]]
	if !r1.Initial || r1.Name != "r1" || len(r1.Timeline) != 1 || r1.Timeline[0].Content["body"] != "hello r1" {
		t.Fatalf("r1: %+v", r1)
	}

	if r1.Limited == nil || !*r1.Limited || r1.PrevBatch == "" || r1.BumpStamp == nil || r1.JoinedCount == nil ||
		*r1.JoinedCount != 1 || r1.NotificationCount == nil || r1.NumLive != nil {
		t.Fatalf("r1 поля: %+v", r1)
	}

	if stateOf(r1.RequiredState, "m.room.name", "") == nil || len(r1.RequiredState) != 1 {
		t.Fatalf("r1 required_state: %+v", r1.RequiredState)
	}

	if r1.Timeline[0].Unsigned["membership"] != "join" {
		t.Fatalf("unsigned.membership: %+v", r1.Timeline[0].Unsigned)
	}

	if r5 := resp.Rooms[rooms[4]]; r5.BumpStamp == nil || *r5.BumpStamp >= *r1.BumpStamp {
		t.Fatalf("bump_stamp r5 %v должен быть меньше r1 %d", r5.BumpStamp, *r1.BumpStamp)
	}

	// prev_batch ведет в /messages: история до первого события timeline.
	msgs := e.must(alice, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(rooms[0])+
		"/messages?dir=b&limit=50&from="+url.QueryEscape(r1.PrevBatch), "")
	if !strings.Contains(msgs.Body.String(), "m.room.create") || strings.Contains(msgs.Body.String(), "hello r1") {
		t.Fatalf("messages по prev_batch: %s", msgs.Body.String())
	}

	resp = e.mustSync(alice, "", listBody(`[[2,4]]`, 0, `[]`))
	if got := resp.Lists["all"].Ops[0].RoomIDs; len(got) != 3 || got[0] != rooms[3] || got[2] != rooms[1] {
		t.Fatalf("окно [2,4]: %v", got)
	}

	if r := resp.Rooms[rooms[3]]; r.Timeline != nil || r.Limited != nil || r.PrevBatch != "" {
		t.Fatalf("timeline_limit 0 - без timeline: %+v", r)
	}
}

// TestSlidingSyncIncrementalOnlyChanges - incremental отдает только комнаты с
// изменениями и только новые события; повтор запроса с тем же pos дает тот же
// ответ; комната без имени получает героев.
func TestSlidingSyncIncrementalOnlyChanges(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "ssialice", "ADEV")
	bob := registerHTTP(t, e.server, "ssibob", "BDEV")

	named := e.createRoom(alice, `{"name":"named"}`)
	dm := e.createRoom(alice, `{"preset":"trusted_private_chat","invite":["@ssibob:test.local"],"is_direct":true}`)
	e.roomAction(bob, dm, "join", `{}`)
	e.send(alice, named, "t1", "first")

	body := listBody(`[[0,9]]`, 2, `[["m.room.name",""]]`)
	first := e.mustSync(alice, "", body)

	if len(first.Rooms) != 2 || first.Lists["all"].Count != 2 {
		t.Fatalf("initial: %+v", first)
	}

	dmRoom := first.Rooms[dm]
	if dmRoom.Name != "" || len(dmRoom.Heroes) != 1 || dmRoom.Heroes[0].UserID != "@ssibob:test.local" ||
		dmRoom.Heroes[0].DisplayName != "ssibob" {
		t.Fatalf("герои комнаты без имени: %+v", dmRoom.Heroes)
	}

	quiet := e.mustSync(alice, first.Pos, body)
	if len(quiet.Rooms) != 0 || quiet.Lists["all"].Count != 2 {
		t.Fatalf("без изменений: %+v", quiet)
	}

	msg := e.send(bob, dm, "t2", "news")
	next := e.mustSync(alice, quiet.Pos, body)

	if len(next.Rooms) != 1 {
		t.Fatalf("incremental: комнат %d, ожидали 1 (%v)", len(next.Rooms), next.Rooms)
	}

	room := next.Rooms[dm]
	if room.Initial || len(room.Timeline) != 1 || room.Timeline[0].EventID != msg || room.NumLive == nil ||
		*room.NumLive != 1 || room.Limited == nil || *room.Limited || room.Name != "" || room.JoinedCount != nil {
		t.Fatalf("incremental комната: %+v", room)
	}

	if ids := next.Lists["all"].Ops[0].RoomIDs; ids[0] != dm {
		t.Fatalf("после сообщения dm должна быть первой: %v", ids)
	}

	// Ответ потерялся - клиент повторяет с тем же pos и получает то же.
	retry := e.mustSync(alice, quiet.Pos, body)
	if len(retry.Rooms) != 1 || len(retry.Rooms[dm].Timeline) != 1 {
		t.Fatalf("повтор с тем же pos: %+v", retry.Rooms)
	}

	if after := e.mustSync(alice, next.Pos, body); len(after.Rooms) != 0 {
		t.Fatalf("после подтверждения изменений нет: %+v", after.Rooms)
	}
}

// TestSlidingSyncUnknownPos - нечитаемый pos, чужое соединение и пересозданное
// соединение - 400 M_UNKNOWN_POS: клиент начинает заново.
func TestSlidingSyncUnknownPos(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "ssualice", "ADEV")
	bob := registerHTTP(t, e.server, "ssubob", "BDEV")

	e.createRoom(alice, `{"name":"x"}`)

	first := e.mustSync(alice, "", listBody(`[[0,9]]`, 1, `[]`))

	unknown := func(token, pos, body, name string) {
		t.Helper()

		code, _, raw := e.sync(token, pos, body, 0)
		if code != http.StatusBadRequest || !strings.Contains(raw, "M_UNKNOWN_POS") {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
	}

	unknown(alice, "garbage", listBody(`[[0,9]]`, 1, `[]`), "мусор")
	unknown(alice, "5/s58_224_0_13_10_1_1_16_0_1", listBody(`[[0,9]]`, 1, `[]`), "pos Synapse")
	unknown(alice, first.Pos, `{"conn_id":"other","lists":{"all":{"ranges":[[0,9]],"timeline_limit":1,"required_state":[]}}}`,
		"другой conn_id")
	unknown(bob, first.Pos, listBody(`[[0,9]]`, 1, `[]`), "чужой пользователь")

	// Новый initial с тем же conn_id сбрасывает соединение: старый pos неизвестен.
	e.mustSync(alice, "", listBody(`[[0,9]]`, 1, `[]`))
	unknown(alice, first.Pos, listBody(`[[0,9]]`, 1, `[]`), "сброшенное соединение")

	if code, _, raw := e.sync(alice, "", `{"lists":{"a":{"ranges":[[0]],"timeline_limit":1}}}`, 0); code != http.StatusBadRequest {
		t.Fatalf("битый range: %d %s", code, raw)
	}

	if rec := do(t, e.server, http.MethodPost, slidingPath, `{}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
}

// TestSlidingSyncRequiredStateLazy - $LAZY отдает членство отправителей timeline
// один раз, $ME - свое; расширение required_state дочитывает состояние без новых
// событий.
func TestSlidingSyncRequiredStateLazy(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "sslalice", "ADEV")
	bob := registerHTTP(t, e.server, "sslbob", "BDEV")
	carol := registerHTTP(t, e.server, "sslcarol", "CDEV")

	room := e.createRoom(alice, `{"preset":"public_chat","name":"lazy","topic":"тема"}`)
	e.roomAction(bob, room, "join", `{}`)
	e.roomAction(carol, room, "join", `{}`)
	e.send(bob, room, "b1", "от боба")

	state := `[["m.room.member","$LAZY"],["m.room.member","$ME"],["m.room.topic",""]]`
	body := listBody(`[[0,0]]`, 1, state)
	first := e.mustSync(alice, "", body)

	got := memberKeys(first.Rooms[room].RequiredState)
	if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "@sslbob:test.local") ||
		!strings.Contains(strings.Join(got, ","), "@sslalice:test.local") {
		t.Fatalf("initial lazy: %v, ожидали alice ($ME) и bob (отправитель)", got)
	}

	if stateOf(first.Rooms[room].RequiredState, "m.room.topic", "") == nil {
		t.Fatalf("нет m.room.topic: %+v", first.Rooms[room].RequiredState)
	}

	e.send(carol, room, "c1", "от кэрол")

	second := e.mustSync(alice, first.Pos, body)
	if got := memberKeys(second.Rooms[room].RequiredState); len(got) != 1 || got[0] != "@sslcarol:test.local" {
		t.Fatalf("новый отправитель: %v", got)
	}

	e.send(bob, room, "b2", "снова боб")

	third := e.mustSync(alice, second.Pos, body)
	if rs := third.Rooms[room].RequiredState; len(rs) != 0 {
		t.Fatalf("известный участник повторно: %+v", rs)
	}

	wider := listBody(`[[0,0]]`, 1, `[["m.room.member","$LAZY"],["m.room.member","$ME"],["m.room.topic",""],["m.room.create",""]]`)

	fourth := e.mustSync(alice, third.Pos, wider)

	r, ok := fourth.Rooms[room]
	if !ok || stateOf(r.RequiredState, "m.room.create", "") == nil || len(r.Timeline) != 0 || r.Initial {
		t.Fatalf("расширение required_state без событий: %+v", fourth.Rooms)
	}

	all := e.mustSync(alice, "", listBody(`[[0,0]]`, 1, `[["*","*"]]`))
	if got := memberKeys(all.Rooms[room].RequiredState); len(got) != 3 {
		t.Fatalf("[*,*] - все состояние: участники %v", got)
	}
}

// TestSlidingSyncInviteAndLeave - приглашение с invite_state и без timeline, вход
// (комната с нуля), выход (последний раз с событием выхода, дальше комнаты нет).
func TestSlidingSyncInviteAndLeave(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "ssvalice", "ADEV")
	bob := registerHTTP(t, e.server, "ssvbob", "BDEV")

	room := e.createRoom(alice, `{"name":"inv"}`)
	e.roomAction(alice, room, "invite", `{"user_id":"@ssvbob:test.local"}`)

	body := `{"lists":{"all":{"ranges":[[0,9]],"timeline_limit":5,"required_state":[["m.room.name",""]]},
		"invites":{"ranges":[[0,9]],"timeline_limit":0,"required_state":[],"filters":{"is_invite":true}},
		"joined":{"ranges":[[0,9]],"timeline_limit":0,"required_state":[],"filters":{"is_invite":false}}}}`
	first := e.mustSync(bob, "", body)

	if first.Lists["all"].Count != 1 || first.Lists["invites"].Count != 1 || first.Lists["joined"].Count != 0 {
		t.Fatalf("списки приглашения: %+v", first.Lists)
	}

	inv := first.Rooms[room]
	if member := stateOf(inv.InviteState, "m.room.member", "@ssvbob:test.local"); member == nil ||
		member.Content["membership"] != "invite" || inv.Timeline != nil || inv.Name != "inv" || inv.BumpStamp == nil {
		t.Fatalf("приглашение: %+v", inv)
	}

	e.roomAction(bob, room, "join", `{}`)

	joined := e.mustSync(bob, first.Pos, body)

	r := joined.Rooms[room]
	if !r.Initial || len(r.Timeline) == 0 || r.Timeline[len(r.Timeline)-1].Type != "m.room.member" || len(r.InviteState) != 0 {
		t.Fatalf("после входа: %+v", r)
	}

	e.roomAction(bob, room, "leave", `{}`)
	e.send(alice, room, "after", "после выхода")

	left := e.mustSync(bob, joined.Pos, body)

	r = left.Rooms[room]
	if len(r.Timeline) == 0 || r.Timeline[len(r.Timeline)-1].Content["membership"] != "leave" {
		t.Fatalf("выход: %+v", r)
	}

	for i := range r.Timeline {
		if r.Timeline[i].Content["body"] == "после выхода" {
			t.Fatal("после выхода отдано событие комнаты")
		}
	}

	gone := e.mustSync(bob, left.Pos, body)
	if gone.Lists["all"].Count != 0 || len(gone.Rooms) != 0 {
		t.Fatalf("покинутая комната осталась: %+v", gone)
	}
}

// TestSlidingSyncRoomSubscriptions - подписка на комнату вне окна; рост
// timeline_limit отдает расширенный timeline; подписка на чужую комнату
// игнорируется.
func TestSlidingSyncRoomSubscriptions(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "sssalice", "ADEV")
	bob := registerHTTP(t, e.server, "sssbob", "BDEV")

	older := e.createRoom(alice, `{"name":"older"}`)
	for i := range 6 {
		e.send(alice, older, "o"+strconv.Itoa(i), "msg "+strconv.Itoa(i))
	}

	newer := e.createRoom(alice, `{"name":"newer"}`)
	foreign := e.createRoom(bob, `{"name":"foreign"}`)

	sub := func(limit int) string {
		return `{"lists":{"all":{"ranges":[[0,0]],"timeline_limit":1,"required_state":[]}},
			"room_subscriptions":{"` + older + `":{"timeline_limit":` + strconv.Itoa(limit) + `,"required_state":[["m.room.name",""]]},
			"` + foreign + `":{"timeline_limit":5,"required_state":[]}}}`
	}

	first := e.mustSync(alice, "", sub(3))

	if ids := first.Lists["all"].Ops[0].RoomIDs; len(ids) != 1 || ids[0] != newer {
		t.Fatalf("окно: %v", ids)
	}

	r, ok := first.Rooms[older]
	if !ok || len(r.Timeline) != 3 || r.Limited == nil || !*r.Limited || r.Name != "older" {
		t.Fatalf("подписка: %+v", first.Rooms)
	}

	if _, ok := first.Rooms[foreign]; ok {
		t.Fatal("подписка на комнату без членства отдана")
	}

	second := e.mustSync(alice, first.Pos, sub(5))

	r, ok = second.Rooms[older]
	if !ok || !r.Expanded || len(r.Timeline) != 5 || r.Initial {
		t.Fatalf("рост timeline_limit: %+v", second.Rooms)
	}

	if third := e.mustSync(alice, second.Pos, sub(5)); len(third.Rooms) != 0 {
		t.Fatalf("без изменений: %+v", third.Rooms)
	}
}

// TestSlidingSyncExtensions - to_device (закладка since удаляет доставленное),
// e2ee (счетчики, device_lists.changed), account_data (глобальная и комнатная),
// receipts и typing - с фильтром по комнатам ответа.
func TestSlidingSyncExtensions(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "ssealice", "ADEV")
	bob := registerHTTP(t, e.server, "ssebob", "BDEV")

	room := e.createRoom(alice, `{"preset":"public_chat","name":"ext"}`)
	e.roomAction(bob, room, "join", `{}`)
	msg := e.send(alice, room, "m1", "прочти")

	e.must(alice, http.MethodPut, "/_matrix/client/v3/user/@ssealice:test.local/account_data/m.test.global", `{"a":1}`)
	e.must(alice, http.MethodPut, "/_matrix/client/v3/user/@ssealice:test.local/rooms/"+url.PathEscape(room)+
		"/account_data/m.test.room", `{"b":2}`)
	e.must(bob, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.test/txn1",
		`{"messages":{"@ssealice:test.local":{"ADEV":{"k":"v"}}}}`)

	encryption := `{"conn_id":"encryption","extensions":{"to_device":{"enabled":true},"e2ee":{"enabled":true}}}`
	enc := e.mustSync(alice, "", encryption)

	td := enc.Extensions.ToDevice
	if td == nil || len(td.Events) != 1 || td.Events[0]["type"] != "m.test" || td.Events[0]["sender"] != "@ssebob:test.local" ||
		td.NextBatch == "" || td.NextBatch == "0" {
		t.Fatalf("to_device: %+v", td)
	}

	if ee := enc.Extensions.E2EE; ee == nil || ee.OTK == nil || ee.Fallback == nil || ee.DeviceLists != nil {
		t.Fatalf("e2ee initial: %+v", ee)
	}

	ack := `{"conn_id":"encryption","extensions":{"to_device":{"enabled":true,"since":"` + td.NextBatch + `"},"e2ee":{"enabled":true}}}`

	upload := `{"device_keys":{"user_id":"@ssebob:test.local","device_id":"BDEV",
		"algorithms":["m.olm.v1.curve25519-aes-sha2"],"keys":{"curve25519:BDEV":"c1","ed25519:BDEV":"e1"}}}`
	e.must(bob, http.MethodPost, "/_matrix/client/v3/keys/upload", upload)

	enc2 := e.mustSync(alice, enc.Pos, ack)
	if td := enc2.Extensions.ToDevice; td == nil || len(td.Events) != 0 {
		t.Fatalf("to_device после подтверждения: %+v", td)
	}

	if ee := enc2.Extensions.E2EE; ee == nil || ee.DeviceLists == nil || !strings.Contains(strings.Join(ee.DeviceLists.Changed, ","), "@ssebob:test.local") {
		t.Fatalf("device_lists.changed: %+v", enc2.Extensions.E2EE)
	}

	// Повтор с since=0: доставленное удалено.
	if again := e.mustSync(alice, "", `{"conn_id":"encryption","extensions":{"to_device":{"enabled":true}}}`); len(again.Extensions.ToDevice.Events) != 0 {
		t.Fatalf("to_device не удален: %+v", again.Extensions.ToDevice)
	}

	exts := `{"conn_id":"room-list","lists":{"all":{"ranges":[[0,9]],"timeline_limit":1,"required_state":[]}},
		"extensions":{"account_data":{"enabled":true},"receipts":{"enabled":true},"typing":{"enabled":true}}}`
	list := e.mustSync(alice, "", exts)

	ad := list.Extensions.AccountData
	if ad == nil || !hasAccountData(ad.Global, "m.test.global") || !hasAccountData(ad.Global, "m.push_rules") ||
		!hasAccountData(ad.Rooms[room], "m.test.room") {
		t.Fatalf("account_data initial: %+v", ad)
	}

	e.must(alice, http.MethodPut, "/_matrix/client/v3/user/@ssealice:test.local/account_data/m.test.second", `{"c":3}`)
	e.must(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/receipt/m.read/"+url.PathEscape(msg), `{}`)
	e.must(bob, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/typing/@ssebob:test.local",
		`{"typing":true,"timeout":30000}`)

	next := e.mustSync(alice, list.Pos, exts)

	ad = next.Extensions.AccountData
	if ad == nil || len(ad.Global) != 1 || ad.Global[0].Type != "m.test.second" || len(ad.Rooms) != 0 {
		t.Fatalf("account_data incremental: %+v", ad)
	}

	receipt, ok := next.Extensions.Receipts.Rooms[room]
	if !ok || receipt.Type != "m.receipt" || receipt.Content[msg]["m.read"]["@ssebob:test.local"] == nil {
		t.Fatalf("receipts: %+v", next.Extensions.Receipts)
	}

	typing, ok := next.Extensions.Typing.Rooms[room]
	if !ok || typing.Type != "m.typing" || len(typing.Content.UserIDs) != 1 || typing.Content.UserIDs[0] != "@ssebob:test.local" {
		t.Fatalf("typing: %+v", next.Extensions.Typing)
	}

	// Расширение только для другого списка - комната не попадает.
	narrow := `{"lists":{"all":{"ranges":[[0,9]],"timeline_limit":1,"required_state":[]}},
		"extensions":{"typing":{"enabled":true,"lists":["other"],"rooms":[]}}}`
	if got := e.mustSync(alice, "", narrow); len(got.Extensions.Typing.Rooms) != 0 {
		t.Fatalf("typing вне lists/rooms: %+v", got.Extensions.Typing)
	}
}

func hasAccountData(events []struct {
	Type    string         `json:"type"`
	Content map[string]any `json:"content"`
}, eventType string,
) bool {
	for _, ev := range events {
		if ev.Type == eventType {
			return true
		}
	}

	return false
}

// TestSlidingSyncLongPollWakesOnMessage - incremental без изменений ждет до
// timeout и просыпается на сообщение; таймаут без изменений - пустой ответ.
func TestSlidingSyncLongPollWakesOnMessage(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "sswalice", "ADEV")
	bob := registerHTTP(t, e.server, "sswbob", "BDEV")

	room := e.createRoom(alice, `{"preset":"public_chat","name":"wake"}`)
	e.roomAction(bob, room, "join", `{}`)

	body := listBody(`[[0,9]]`, 1, `[]`)
	first := e.mustSync(alice, "", body)

	started := time.Now()

	code, empty, raw := e.sync(alice, first.Pos, body, 400)
	if code != http.StatusOK || len(empty.Rooms) != 0 || time.Since(started) < 350*time.Millisecond {
		t.Fatalf("таймаут без изменений: %d за %v %s", code, time.Since(started), raw)
	}

	type result struct {
		code int
		resp ssResp
	}

	done := make(chan result, 1)
	started = time.Now()

	go func() {
		code, resp, _ := e.sync(alice, empty.Pos, body, 20000)
		done <- result{code, resp}
	}()

	time.Sleep(300 * time.Millisecond)

	msg := e.send(bob, room, "w1", "проснись")

	select {
	case got := <-done:
		r := got.resp.Rooms[room]
		if got.code != http.StatusOK || len(r.Timeline) != 1 || r.Timeline[0].EventID != msg {
			t.Fatalf("long-poll: %d %+v", got.code, got.resp.Rooms)
		}

		if waited := time.Since(started); waited < 250*time.Millisecond || waited > 10*time.Second {
			t.Fatalf("long-poll ответил за %v", waited)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("long-poll не проснулся на сообщение")
	}
}

// TestSlidingSyncQueryCountIsConstant - число SQL-запросов sliding sync не
// зависит от числа комнат в окне (ADR-0005).
func TestSlidingSyncQueryCountIsConstant(t *testing.T) {
	server := newIntegrationServerWithTracer(t, syncQueryTracer{})
	e := &ssEnv{t: t, server: server}
	alice := registerHTTP(t, server, "ssqalice", "ADEV")

	body := `{"lists":{"all":{"ranges":[[0,99]],"timeline_limit":3,
		"required_state":[["m.room.member","$LAZY"],["m.room.member","$ME"],["m.room.name",""],["m.room.encryption",""]],
		"filters":{"not_room_types":["m.space"]}}},
		"extensions":{"account_data":{"enabled":true},"receipts":{"enabled":true},"typing":{"enabled":true},
		"e2ee":{"enabled":true},"to_device":{"enabled":true}}}`

	counted := func(pos string) (int64, ssResp) {
		t.Helper()

		var counter atomic.Int64

		path := slidingPath + "?timeout=0"
		if pos != "" {
			path += "&pos=" + url.QueryEscape(pos)
		}

		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+alice)
		req = req.WithContext(context.WithValue(req.Context(), syncQueryKey{}, &counter))

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		var resp ssResp
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("sliding sync: %d %s", rec.Code, rec.Body.String())
		}

		return counter.Load(), resp
	}

	addRooms := func(n int, prefix string) {
		for i := range n {
			roomID := e.createRoom(alice, `{"name":"`+prefix+strconv.Itoa(i)+`"}`)
			e.send(alice, roomID, prefix+strconv.Itoa(i), "x")
		}
	}

	addRooms(2, "a")
	smallInitial, small := counted("")
	addRooms(1, "b")
	smallIncr, _ := counted(small.Pos)

	addRooms(10, "c")
	bigInitial, big := counted("")
	addRooms(5, "d")
	bigIncr, _ := counted(big.Pos)

	if smallInitial != bigInitial || smallIncr != bigIncr {
		t.Fatalf("число запросов зависит от числа комнат: initial %d/%d, incremental %d/%d",
			smallInitial, bigInitial, smallIncr, bigIncr)
	}

	t.Logf("sliding sync: initial %d запросов, incremental %d", bigInitial, bigIncr)
}

// TestSlidingSyncStickySubscriptionAndPeek - как работает Element Web: подписка
// присылается один раз и действует до unsubscribe_rooms; публичную комнату без
// членства можно открыть (stripped state, затем вход отдает ее с timeline);
// смена имени участника приходит в required_state подписки.
func TestSlidingSyncStickySubscriptionAndPeek(t *testing.T) {
	e := newSSEnv(t)
	alice := registerHTTP(t, e.server, "sspalice", "ADEV")
	bob := registerHTTP(t, e.server, "sspbob", "BDEV")

	room := e.createRoom(alice, `{"preset":"public_chat","name":"Публичная"}`)
	e.send(alice, room, "p1", "до входа")

	subscribe := `{"room_subscriptions":{"` + room + `":{"timeline_limit":10,"required_state":[["*","*"]]}}}`
	first := e.mustSync(bob, "", subscribe)

	peek := first.Rooms[room]
	if peek.Timeline != nil || len(peek.InviteState) == 0 || peek.Name != "Публичная" ||
		stateOf(peek.InviteState, "m.room.join_rules", "") == nil {
		t.Fatalf("просмотр до входа: %+v", first.Rooms)
	}

	e.roomAction(bob, room, "join", `{}`)

	joined := e.mustSync(bob, first.Pos, `{}`)

	r := joined.Rooms[room]
	if !r.Initial || len(r.Timeline) == 0 || len(r.InviteState) != 0 || stateOf(r.RequiredState, "m.room.member", "@sspbob:test.local") == nil {
		t.Fatalf("вход по липкой подписке: %+v", joined.Rooms)
	}

	e.must(alice, http.MethodPut, "/_matrix/client/v3/profile/@sspalice:test.local/displayname", `{"displayname":"Алиса"}`)

	// Профиль расходится по комнатам в фоне: ждем long-poll.
	_, renamed, _ := e.sync(bob, joined.Pos, `{}`, 10000)
	if member := stateOf(renamed.Rooms[room].RequiredState, "m.room.member", "@sspalice:test.local"); member == nil ||
		member.Content["displayname"] != "Алиса" {
		st := do(t, e.server, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/state/m.room.member/@sspalice:test.local", "", alice)
		t.Fatalf("смена имени в required_state: %+v (state %s, pos %s -> %s)", renamed.Rooms, st.Body.String(), joined.Pos, renamed.Pos)
	}

	gone := e.mustSync(bob, renamed.Pos, `{"unsubscribe_rooms":["`+room+`"]}`)
	e.send(alice, room, "p2", "после отписки")

	if after := e.mustSync(bob, gone.Pos, `{}`); len(after.Rooms) != 0 {
		t.Fatalf("после unsubscribe_rooms комната отдается: %+v", after.Rooms)
	}
}
