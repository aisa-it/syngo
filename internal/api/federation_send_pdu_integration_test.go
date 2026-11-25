//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// Прием /send по PDU: ошибка по плохому PDU вместо 400/403/500 на
// всю транзакцию, состояние по комнатам, отклоненные и soft-failed события вне
// таймлайна, m.room.server_acl, content hash, вычисляемый event_id, лимиты.

var seedAuth = []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}

// joinBob - bob входит в комнату seedRoom через /send; возвращает id join.
func (e *fedEnv) joinBob(t *testing.T, roomID, txnID string) string {
	t.Helper()

	join, joinID := e.member(t, roomID, "join", seedAuth, 4)
	if results := e.sendTxn(t, txnID, join); len(results[joinID]) != 0 {
		t.Fatalf("join bob: %v", results)
	}

	return joinID
}

// timelineIDs - события комнаты, которые видит клиент (/sync, /messages).
func (e *fedEnv) timelineIDs(t *testing.T, roomID string) []string {
	t.Helper()

	events, err := e.server.db.RoomEventsSince(context.Background(), roomID, 0, 1000)
	if err != nil {
		t.Fatalf("RoomEventsSince: %v", err)
	}

	ids := make([]string, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].EventID)
	}

	return ids
}

// TestFederationSendTwoRooms - состояние каждой комнаты транзакции - в своей
// комнате (раньше state-события всех комнат материализовались в комнату pdus[0]).
func TestFederationSendTwoRooms(t *testing.T) {
	env := newFedEnv(t)
	env.seedRoom(t, "!one:test.local")

	ctx := context.Background()

	// Вторая комната - те же события с другими id.
	for _, sql := range []string{
		`INSERT INTO rooms (room_id, creator) VALUES ('!two:test.local', '@alice:test.local')`,
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, auth_events, depth, origin_server_ts, origin)
		 SELECT replace(event_id, ':test.local', '-two:test.local'), '!two:test.local', type, sender, state_key, content,
		        coalesce((SELECT array_agg(replace(a, ':test.local', '-two:test.local')) FROM unnest(auth_events) a), '{}'),
		        depth, origin_server_ts, origin
		 FROM events WHERE room_id = '!one:test.local'`,
		`INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		 VALUES ('$alice-join-two:test.local', '!two:test.local', '@alice:test.local', '@alice:test.local', 'join')`,
		`INSERT INTO room_state (room_id, type, state_key, event_id)
		 SELECT room_id, type, state_key, event_id FROM events WHERE room_id = '!two:test.local'`,
	} {
		if _, err := env.server.db.Pool().Exec(ctx, sql); err != nil {
			t.Fatalf("seed two: %v", err)
		}
	}

	joinOne, idOne := env.member(t, "!one:test.local", "join", seedAuth, 4)
	joinTwo, idTwo := env.event(t, "@bob:remote.test", "!two:test.local", "m.room.member", strPtr("@bob:remote.test"),
		map[string]any{"membership": "join"},
		[]string{"$create-two:test.local", "$pl-two:test.local", "$invite-two:test.local"}, []string{"$invite-two:test.local"}, 4)

	results := env.sendTxn(t, "two-rooms", joinTwo, joinOne)
	if len(results[idOne]) != 0 || len(results[idTwo]) != 0 {
		t.Fatalf("pdus = %v", results)
	}

	for roomID, want := range map[string]string{"!one:test.local": idOne, "!two:test.local": idTwo} {
		state := env.queryStrings(t, `
			SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID)
		members := env.queryStrings(t, `
			SELECT membership FROM room_memberships WHERE room_id = $1 AND user_id = '@bob:remote.test'`, roomID)

		if !slices.Equal(state, []string{want}) || !slices.Equal(members, []string{"join"}) {
			t.Fatalf("%s: member bob %v, membership %v; ожидали %s и join", roomID, state, members, want)
		}
	}
}

// TestFederationSendRemoteKick - kick локального пользователя удаленным
// админом лишает его доступа - /send клиента получает 403.
func TestFederationSendRemoteKick(t *testing.T) {
	const roomID = "!kick:test.local"

	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICE")
	env.seedRoom(t, roomID)

	if _, err := env.server.db.Pool().Exec(context.Background(), `
		UPDATE events SET content = '{"users":{"@alice:test.local":50,"@bob:remote.test":100}}'
		WHERE event_id = '$pl:test.local'`); err != nil {
		t.Fatal(err)
	}

	if rec := do(t, env.server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/k1",
		`{"msgtype":"m.text","body":"до kick"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("send до kick: %d %s", rec.Code, rec.Body.String())
	}

	joinID := env.joinBob(t, roomID, "kick-join")

	kick, kickID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@alice:test.local"),
		map[string]any{"membership": "leave"},
		[]string{"$create:test.local", "$pl:test.local", joinID, "$alice-join:test.local"}, []string{joinID}, 6)

	if results := env.sendTxn(t, "kick", kick); len(results[kickID]) != 0 {
		t.Fatalf("kick: %v", results)
	}

	rec := do(t, env.server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/k2",
		`{"msgtype":"m.text","body":"после kick"}`, alice)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("send после kick: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}
}

// TestFederationSendTamperedContent - content изменен при валидной подписи
// (подпись - по редактированной копии) - content hash не совпадает, сохраняется
// редактированная копия, как в Synapse; event_id тот же.
func TestFederationSendTamperedContent(t *testing.T) {
	const roomID = "!tamper:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)
	joinID := env.joinBob(t, roomID, "tamper-join")

	msg, msgID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "original"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{joinID}, 5)
	msg["content"] = map[string]any{"body": "tampered"}

	if results := env.sendTxn(t, "tamper", msg); len(results[msgID]) != 0 {
		t.Fatalf("pdus = %v", results)
	}

	var content, pdu string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT content::text, pdu_json FROM events WHERE event_id = $1`, msgID).Scan(&content, &pdu); err != nil {
		t.Fatalf("событие не сохранено: %v", err)
	}

	if content != "{}" || strings.Contains(pdu, "tampered") {
		t.Fatalf("content %s, pdu %s; ожидали редактированную копию", content, pdu)
	}
}

// TestFederationSendForgedEventID - PDU с подставленным event_id отвергнут
// (id считается по содержимому; раньше брался из PDU как есть).
func TestFederationSendForgedEventID(t *testing.T) {
	const roomID = "!forged:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)

	join, _ := env.member(t, roomID, "join", seedAuth, 4)
	join["event_id"] = "$forged:remote.test"
	join = env.resign(t, join)

	joinID, err := core.EventID(join, core.DefaultRoomVersion)
	if err != nil {
		t.Fatal(err)
	}

	results := env.sendTxn(t, "forged", join)
	if msg, _ := results[joinID]["error"].(string); !strings.Contains(msg, "event_id") {
		t.Fatalf("pdus = %v, ожидали ошибку event_id по %s", results, joinID)
	}

	if env.eventCount(t, joinID)+env.eventCount(t, "$forged:remote.test") != 0 {
		t.Fatal("PDU с подставленным event_id записан")
	}
}

// TestFederationSendRejectedHidden - событие, не прошедшее auth (topic от bob без
// прав), сохраняется отклоненным: не в таймлайне клиентов, не в состоянии.
func TestFederationSendRejectedHidden(t *testing.T) {
	const roomID = "!rejected:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)
	joinID := env.joinBob(t, roomID, "rejected-join")

	topic, topicID := env.event(t, "@bob:remote.test", roomID, "m.room.topic", strPtr(""), map[string]any{"topic": "spam"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{joinID}, 5)

	if results := env.sendTxn(t, "rejected", topic); len(results[topicID]) != 0 {
		t.Fatalf("pdus = %v", results)
	}

	if got := env.queryStrings(t, `
		SELECT event_id FROM events WHERE event_id = $1 AND outlier AND rejected_reason IS NOT NULL`, topicID); len(got) != 1 {
		t.Fatal("отклоненное событие не сохранено отклоненным")
	}

	if slices.Contains(env.timelineIDs(t, roomID), topicID) {
		t.Fatal("отклоненное событие в таймлайне клиента")
	}

	if got := env.queryStrings(t, `SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.topic'`, roomID); len(got) != 0 {
		t.Fatalf("отклоненное событие в состоянии: %v", got)
	}
}

// TestFederationSendSoftFail - bob забанен локально, его сервер присылает
// сообщение из ветки до бана: по состоянию до события bob в комнате, по текущему -
// нет; soft-fail: событие сохранено, но не в таймлайне и не в extremities (Synapse).
func TestFederationSendSoftFail(t *testing.T) {
	const roomID = "!softfail:test.local"

	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICE")
	env.seedRoom(t, roomID)
	joinID := env.joinBob(t, roomID, "soft-join")

	if rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/ban",
		`{"user_id":"@bob:remote.test"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}

	extremities := env.queryStrings(t, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID)

	msg, msgID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "spam"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{joinID}, 5)

	if results := env.sendTxn(t, "soft", msg); len(results[msgID]) != 0 {
		t.Fatalf("pdus = %v", results)
	}

	if got := env.queryStrings(t, `
		SELECT event_id FROM events WHERE event_id = $1 AND soft_failed AND outlier AND rejected_reason IS NULL`, msgID); len(got) != 1 {
		t.Fatal("сообщение забаненного не soft-failed")
	}

	if slices.Contains(env.timelineIDs(t, roomID), msgID) {
		t.Fatal("soft-failed событие в таймлайне клиента")
	}

	if got := env.queryStrings(t, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID); !slices.Equal(got, extremities) {
		t.Fatalf("extremities %v, ожидали прежние %v", got, extremities)
	}
}

// TestFederationSendServerACL - m.room.server_acl запрещает remote.test: PDU
// комнаты получают ошибку, typing и receipt EDU отбрасываются.
func TestFederationSendServerACL(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := fedRoomWithRemote(t, env)
	eventID := sendText(t, env.server, alice, roomID, "acl1", "hi")

	if rec := do(t, env.server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/state/m.room.server_acl/",
		`{"allow":["*"],"deny":["remote.test"],"allow_ip_literals":true}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("server_acl: %d %s", rec.Code, rec.Body.String())
	}

	env.sendEDUs(t, "acl-edu",
		map[string]any{"edu_type": "m.typing", "content": map[string]any{
			"room_id": roomID, "user_id": "@bob:remote.test", "typing": true,
		}},
		map[string]any{"edu_type": "m.receipt", "content": map[string]any{
			roomID: map[string]any{"m.read": map[string]any{
				"@bob:remote.test": map[string]any{"event_ids": []string{eventID}, "data": map[string]any{"ts": 1}},
			}},
		}},
	)

	if typing := env.server.ephemeral.Typing(roomID); len(typing) != 0 {
		t.Fatalf("typing запрещенного сервера принят: %v", typing)
	}

	if receipts := env.server.ephemeral.UserReceipts(roomID, "@bob:remote.test"); len(receipts) != 0 {
		t.Fatalf("receipt запрещенного сервера принят: %v", receipts)
	}

	var latest string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT event_id FROM events WHERE room_id = $1 ORDER BY stream_ordering DESC LIMIT 1`, roomID).Scan(&latest); err != nil {
		t.Fatal(err)
	}

	msg, msgID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "x"},
		[]string{latest}, []string{latest}, 20)

	results := env.sendTxn(t, "acl-pdu", msg)
	if errMsg, _ := results[msgID]["error"].(string); !strings.Contains(errMsg, "banned") {
		t.Fatalf("pdus = %v, ожидали ошибку ACL", results)
	}

	if env.eventCount(t, msgID) != 0 {
		t.Fatal("PDU запрещенного сервера записан")
	}
}

// TestFederationSendNulContent - \u0000 в content не роняет транзакцию: событие
// принято (в jsonb NUL заменен, исходный PDU - как пришел), соседний PDU тоже
// (раньше SQLSTATE 22P05 откатывал всю транзакцию вместе с inbox).
func TestFederationSendNulContent(t *testing.T) {
	const roomID = "!nul:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)
	joinID := env.joinBob(t, roomID, "nul-join")
	auth := []string{"$create:test.local", "$pl:test.local", joinID}

	nul, nulID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "a\u0000b"},
		auth, []string{joinID}, 5)
	next, nextID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "next"},
		auth, []string{nulID}, 6)

	results := env.sendTxn(t, "nul", nul, next)
	if len(results[nulID]) != 0 || len(results[nextID]) != 0 {
		t.Fatalf("pdus = %v", results)
	}

	var body, pdu string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT content->>'body', pdu_json FROM events WHERE event_id = $1`, nulID).Scan(&body, &pdu); err != nil {
		t.Fatalf("событие с NUL: %v", err)
	}

	if body != "a�b" || !strings.Contains(pdu, `a\u0000b`) {
		t.Fatalf("body %q, pdu %s", body, pdu)
	}

	if got := env.timelineIDs(t, roomID); !slices.Contains(got, nextID) {
		t.Fatal("соседний PDU не принят")
	}
}

// TestFederationSendLimits - больше 50 PDU - 400 на транзакцию (Synapse); PDU
// больше 65536 байт - ошибка по нему; тело до 1 МиБ принимается (раньше
// резалось на 256 КиБ), больше - 413.
func TestFederationSendLimits(t *testing.T) {
	const roomID = "!limits:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)
	joinID := env.joinBob(t, roomID, "limits-join")
	auth := []string{"$create:test.local", "$pl:test.local", joinID}

	many := make([]any, 0, maxPDUsPerTxn+1)
	for i := 0; i <= maxPDUsPerTxn; i++ {
		many = append(many, map[string]any{"room_id": roomID})
	}

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/limits-many",
		map[string]any{"origin": "remote.test", "pdus": many}); rec.Code != http.StatusBadRequest {
		t.Fatalf("51 PDU: %d, ожидали 400", rec.Code)
	}

	big, bigID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil,
		map[string]any{"body": strings.Repeat("x", maxPDUSize)}, auth, []string{joinID}, 5)

	// Пять PDU по ~60 КБ - тело больше 256 КиБ.
	pdus := []map[string]any{big}
	prev := joinID

	var ids []string

	for i := 0; i < 5; i++ {
		msg, id := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil,
			map[string]any{"body": strings.Repeat("y", 60000)}, auth, []string{prev}, int64(6+i))
		pdus, prev, ids = append(pdus, msg), id, append(ids, id)
	}

	results := env.sendTxn(t, "limits-big", pdus...)
	if msg, _ := results[bigID]["error"].(string); !strings.Contains(msg, "too large") {
		t.Fatalf("PDU больше лимита: %v", results[bigID])
	}

	for _, id := range ids {
		if len(results[id]) != 0 {
			t.Fatalf("PDU %s: %v", id, results[id])
		}
	}

	huge := strings.Repeat("z", maxFederationBodySize)

	raw, err := json.Marshal(map[string]any{"origin": "remote.test", "pdus": []any{}, "padding": huge})
	if err != nil {
		t.Fatal(err)
	}

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/limits-huge", json.RawMessage(raw)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("тело больше 1 МиБ: %d, ожидали 413", rec.Code)
	}

}
