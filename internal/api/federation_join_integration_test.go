//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// fedEnv — окружение теста: свой сервер + ключ «чужого» сервера remote.test.
type fedEnv struct {
	server    *Server
	remoteKey signing.Key
}

func newFedEnv(t *testing.T) *fedEnv {
	t.Helper()

	server := newIntegrationServer(t)

	remoteKey, err := signing.Generate("a_remote")
	if err != nil {
		t.Fatalf("remote key: %v", err)
	}

	server.keyLookup = func(origin, keyID string) (string, int64, error) {
		if origin == "remote.test" && keyID == remoteKey.KeyID {
			return remoteKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		return "", 0, signing.ErrInvalidKey
	}

	return &fedEnv{server: server, remoteKey: remoteKey}
}

// sendFed выполняет подписанный запрос к серверу.
func (e *fedEnv) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.requestAs(t, "remote.test", e.remoteKey, method, path, body)
}

// requestAs - запрос, подписанный ключом сервера origin.
func (e *fedEnv) requestAs(t *testing.T, origin string, key signing.Key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload []byte

	if body != nil {
		var err error

		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}

	header, err := signing.MatrixAuthHeader(key, origin, "test.local", method, path, payload)
	if err != nil {
		t.Fatalf("sign request: %v", err)
	}

	req := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
	req.Header.Set("Authorization", header)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)

	return rec
}

// requestUnsigned — запрос без X-Matrix (должен получать 401).
func (e *fedEnv) requestUnsigned(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload []byte

	if body != nil {
		var err error

		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)

	return rec
}

// event - событие sender без явного event_id (room v3+), подписанное ключом
// remote.test; event_id считается по содержимому, как у получателя.
func (e *fedEnv) event(
	t *testing.T, sender, roomID, typ string, stateKey *string, content map[string]any,
	authEvents, prevEvents []string, depth int64,
) (map[string]any, string) {
	t.Helper()

	if prevEvents == nil {
		prevEvents = []string{}
	}

	object := map[string]any{
		"room_id": roomID, "type": typ, "sender": sender, "content": content,
		"depth": depth, "origin_server_ts": 1_700_000_000_000,
		"prev_events": prevEvents, "auth_events": authEvents,
	}

	if stateKey != nil {
		object["state_key"] = *stateKey
	}

	signed := e.resign(t, object)

	eventID, err := core.EventID(signed, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	return signed, eventID
}

// member - членство @bob:remote.test за себя (join/leave).
func (e *fedEnv) member(t *testing.T, roomID, membership string, authEvents []string, depth int64) (map[string]any, string) {
	t.Helper()

	return e.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@bob:remote.test"),
		map[string]any{"membership": membership}, authEvents, []string{"$invite:test.local"}, depth)
}

// seedRoom создаёт комнату и авторизационное состояние (create, join alice,
// power_levels, invite bob@remote.test): alice - наш участник, без него
// комната не наша (make_join/send_join отвечают 404).
func (e *fedEnv) seedRoom(t *testing.T, roomID string) {
	t.Helper()

	ctx := context.Background()

	statements := []struct {
		args []any
		sql  string
	}{
		{[]any{roomID, "@alice:test.local"}, "INSERT INTO rooms (room_id, creator) VALUES ($1,$2)"},
		{[]any{"$create:test.local", roomID, "@alice:test.local"}, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.create',$3,'','{"creator":"@alice:test.local"}',1,1,'test.local',1)`},
		{[]any{"$pl:test.local", roomID, "@alice:test.local"}, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, auth_events, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.power_levels',$3,'','{"users":{"@alice:test.local":100}}',
			        '{$create:test.local,$alice-join:test.local}',2,2,'test.local',2)`},
		{[]any{"$invite:test.local", roomID}, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, auth_events, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.member','@alice:test.local','@bob:remote.test','{"membership":"invite"}',
			        '{$create:test.local,$pl:test.local,$alice-join:test.local}',3,3,'test.local',3)`},
		{[]any{"$alice-join:test.local", roomID}, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, auth_events, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.member','@alice:test.local','@alice:test.local','{"membership":"join"}',
			        '{$create:test.local}',2,2,'test.local',0)`},
		{[]any{"$alice-join:test.local", roomID}, `
			INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			VALUES ($1,$2,'@alice:test.local','@alice:test.local','join')`},
	}

	for _, st := range statements {
		if _, err := e.server.db.Pool().Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Состояние комнаты: create/power_levels/invite → room_state.
	if _, err := e.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT room_id, type, state_key, event_id FROM events WHERE room_id = $1
		ON CONFLICT DO NOTHING`, roomID); err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// sendTxn отправляет транзакцию remote.test и возвращает pdus ответа (200 обязателен).
func (e *fedEnv) sendTxn(t *testing.T, txnID string, pdus ...map[string]any) map[string]map[string]any {
	t.Helper()

	list := make([]any, 0, len(pdus))
	for _, pdu := range pdus {
		list = append(list, pdu)
	}

	rec := e.request(t, http.MethodPut, "/_matrix/federation/v1/send/"+txnID, map[string]any{
		"origin": "remote.test", "pdus": list, "edus": []any{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("send %s: %d %s", txnID, rec.Code, rec.Body.String())
	}

	var reply struct {
		PDUs map[string]map[string]any `json:"pdus"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("send %s: ответ %s: %v", txnID, rec.Body.String(), err)
	}

	return reply.PDUs
}

// eventCount - число событий с id в events.
func (e *fedEnv) eventCount(t *testing.T, eventID string) int {
	t.Helper()

	var count int
	if err := e.server.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM events WHERE event_id = $1", eventID).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", eventID, err)
	}

	return count
}

// TestFederationSendBadSignature — невалидная подпись PDU: ошибка по этому PDU,
// транзакция 200, событие не записано (раньше 403 на всю транзакцию,
// тест закреплял его).
func TestFederationSendBadSignature(t *testing.T) {
	env := newFedEnv(t)
	env.seedRoom(t, "!sig:test.local")

	pdu, id := env.member(t, "!sig:test.local", "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)
	// Ломаем подпись.
	pdu["signatures"].(map[string]any)["remote.test"].(map[string]any)[env.remoteKey.KeyID] = "AAAA"

	results := env.sendTxn(t, "txn-bad", pdu)

	if msg, _ := results[id]["error"].(string); !strings.Contains(msg, "signature") {
		t.Fatalf("pdus = %v, ожидали ошибку подписи по %s", results, id)
	}

	if env.eventCount(t, id) != 0 {
		t.Fatal("событие с невалидной подписью записано")
	}
}

// TestFederationSendRejectsNoAuthEvents — событие без auth_events: ошибка по PDU.
func TestFederationSendRejectsNoAuthEvents(t *testing.T) {
	env := newFedEnv(t)
	env.seedRoom(t, "!noauth:test.local")

	pdu, id := env.event(t, "@bob:remote.test", "!noauth:test.local", "m.room.message", nil,
		map[string]any{"body": "x"}, []string{}, []string{"$invite:test.local"}, 4)

	if results := env.sendTxn(t, "txn-noauth", pdu); results[id]["error"] == nil || env.eventCount(t, id) != 0 {
		t.Fatalf("без auth_events: pdus = %v", results)
	}
}

// TestFederationSendDedupe — корректно подписанная транзакция применяется один раз.
func TestFederationSendDedupe(t *testing.T) {
	env := newFedEnv(t)
	env.seedRoom(t, "!dedupe:test.local")

	pdu, id := env.member(t, "!dedupe:test.local", "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)

	if results := env.sendTxn(t, "txn-1", pdu); len(results[id]) != 0 {
		t.Fatalf("первая транзакция: %v", results)
	}

	// Повтор txn — 200 без повторного применения.
	env.sendTxn(t, "txn-1", pdu)

	if count := env.eventCount(t, id); count != 1 {
		t.Fatalf("событий %d, ожидали 1", count)
	}

	// auth-цепочка сохранена.
	var authCount int
	if err := env.server.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM event_auth WHERE event_id=$1", id).Scan(&authCount); err != nil {
		t.Fatalf("count auth: %v", err)
	}

	if authCount != 3 {
		t.Fatalf("auth-ссылок %d, ожидали 3", authCount)
	}
}

// TestFederationSendOneBadPDU - транзакция из 50 PDU с одним плохим
// (поддельная подпись) - 49 приняты, в ответе ошибка ровно по одному, транзакция
// отмечена в inbox. Битый PDU без подписи и hashes - тоже ошибка по нему. Раньше
// один плохой PDU давал 400/403 на всю транзакцию, и отправитель повторял ее
// бесконечно.
func TestFederationSendOneBadPDU(t *testing.T) {
	const roomID = "!atomic:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)

	join, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)
	auth := []string{"$create:test.local", "$pl:test.local", joinID}

	pdus := []map[string]any{join}
	prev := joinID

	var badID string

	for i := 0; len(pdus) < 50; i++ {
		if i == 20 {
			bad, id := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil,
				map[string]any{"body": "bad"}, auth, []string{joinID}, 5)
			bad["signatures"].(map[string]any)["remote.test"].(map[string]any)[env.remoteKey.KeyID] = "AAAA"
			pdus, badID = append(pdus, bad), id

			continue
		}

		msg, id := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil,
			map[string]any{"body": fmt.Sprintf("m%d", i)}, auth, []string{prev}, int64(5+i))
		pdus, prev = append(pdus, msg), id
	}

	// Порядок в транзакции перемешан: сообщения раньше join, которым авторизованы.
	pdus[0], pdus[len(pdus)-1] = pdus[len(pdus)-1], pdus[0]

	results := env.sendTxn(t, "txn-atomic", pdus...)

	var errorsCount int

	for id, result := range results {
		if result["error"] != nil {
			errorsCount++

			if id != badID {
				t.Fatalf("ошибка по %s: %v", id, result)
			}
		}
	}

	if len(results) != 50 || errorsCount != 1 {
		t.Fatalf("результатов %d, ошибок %d; ожидали 50 и 1", len(results), errorsCount)
	}

	var accepted, inbox int
	if err := env.server.db.Pool().QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM events WHERE room_id = $1 AND origin = 'remote.test' AND NOT outlier),
		       (SELECT count(*) FROM federation_inbox WHERE txn_id = 'txn-atomic')`, roomID).Scan(&accepted, &inbox); err != nil {
		t.Fatalf("count: %v", err)
	}

	if accepted != 49 || inbox != 1 {
		t.Fatalf("принято %d, inbox %d; ожидали 49 и 1", accepted, inbox)
	}

	// PDU без подписи и hashes - ошибка по нему, транзакция 200.
	broken := map[string]any{
		"room_id": roomID, "type": "m.room.message", "sender": "@bob:remote.test", "content": map[string]any{},
		"auth_events": auth, "prev_events": []string{prev}, "depth": 60, "origin_server_ts": 1,
	}
	brokenID, err := core.EventID(broken, core.DefaultRoomVersion)
	if err != nil {
		t.Fatal(err)
	}

	if results := env.sendTxn(t, "txn-broken", broken); results[brokenID]["error"] == nil {
		t.Fatalf("битый PDU: %v", results)
	}
}

// TestFederationMakeJoinAndSendJoin — join-флоу: make_join отдаёт шаблон, send_join применяет join.
func TestFederationMakeJoinAndSendJoin(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!joinflow:test.local"
	env.seedRoom(t, roomID)

	path := "/_matrix/federation/v1/make_join/" + roomID + "/@bob:remote.test?ver=10&ver=11"
	rec := env.request(t, http.MethodGet, path, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("make_join: %d %s", rec.Code, rec.Body.String())
	}

	var joined struct {
		RoomVersion string         `json:"room_version"`
		Event       map[string]any `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &joined); err != nil {
		t.Fatalf("decode make_join: %v", err)
	}

	if joined.RoomVersion != "10" || joined.Event["type"] != "m.room.member" {
		t.Fatalf("шаблон make_join: %+v", joined)
	}

	// send_join: вступающий подписывает шаблон и присылает его.
	joinEvent := env.resign(t, joined.Event)

	joinID, err := core.EventID(joinEvent, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID, joinEvent)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_join: %d %s", rec.Code, rec.Body.String())
	}

	// v2-ответ обязан содержать event (без event_id), state, auth_chain,
	// members_omitted и servers_in_room — иначе Synapse бракует 200.
	var sendJoin struct {
		Event          map[string]any `json:"event"`
		State          []any          `json:"state"`
		AuthChain      []any          `json:"auth_chain"`
		MembersOmitted bool           `json:"members_omitted"`
		ServersInRoom  []string       `json:"servers_in_room"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &sendJoin); err != nil {
		t.Fatalf("unmarshal send_join: %v", err)
	}

	// event_id в event быть НЕ должно: для room v3+ Synapse падает с
	// «v2/v3 events must not have an explicit event_id» (проверено на стенде).
	if sendJoin.Event["event_id"] != nil {
		t.Fatalf("send_join v2 event содержит event_id: %s", rec.Body.String())
	}

	if len(sendJoin.State) == 0 || len(sendJoin.AuthChain) == 0 {
		t.Fatalf("send_join v2 без state/auth_chain: %s", rec.Body.String())
	}

	// servers_in_room - серверы с участниками до join: только мы (bob приглашен).
	if !slices.Equal(sendJoin.ServersInRoom, []string{"test.local"}) {
		t.Fatalf("servers_in_room = %v", sendJoin.ServersInRoom)
	}

	// Membership и состояние обновились; событие записано под посчитанным id.
	var membership, eventID string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		"SELECT membership, event_id FROM room_memberships WHERE room_id=$1 AND user_id='@bob:remote.test'", roomID).
		Scan(&membership, &eventID); err != nil {
		t.Fatalf("membership: %v", err)
	}

	if membership != "join" || eventID != joinID {
		t.Fatalf("membership = %q (%s), ожидали join (%s)", membership, eventID, joinID)
	}

	var stateMembership string
	if err := env.server.db.Pool().QueryRow(context.Background(), `
		SELECT e.content->>'membership' FROM room_state rs JOIN events e ON e.event_id=rs.event_id
		WHERE rs.room_id=$1 AND rs.type='m.room.member' AND rs.state_key='@bob:remote.test'`, roomID).Scan(&stateMembership); err != nil {
		t.Fatalf("state membership: %v", err)
	}

	if stateMembership != "join" {
		t.Fatalf("room_state membership = %q", stateMembership)
	}

	rooms, err := env.server.db.DestinationRooms(context.Background(), "remote.test")
	if err != nil || len(rooms) == 0 {
		t.Fatalf("destination_rooms: %v, err=%v", rooms, err)
	}

	// Подделанная подпись send_join — отказ.
	bad, badID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 5)
	bad["signatures"].(map[string]any)["remote.test"].(map[string]any)[env.remoteKey.KeyID] = "AAAA"

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send_join/"+roomID+"/"+badID, bad); rec.Code != http.StatusForbidden {
		t.Fatalf("поддельный send_join: %d, ожидали 403", rec.Code)
	}
}

// TestFederationMakeJoinChecks — make_join только с подписью
// X-Matrix, для пользователя сервера-отправителя, с версией комнаты из ver и
// для того, кому вход разрешен.
func TestFederationMakeJoinChecks(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!makejoin:test.local"
	env.seedRoom(t, roomID)

	base := "/_matrix/federation/v1/make_join/" + roomID

	// Без подписи - 401 (раньше маршрут был вне X-Matrix и отдавал шаблон любому).
	if rec := env.requestUnsigned(t, http.MethodGet, base+"/@bob:remote.test?ver=10", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("make_join без подписи: %d %s", rec.Code, rec.Body.String())
	}

	// Пользователь чужого сервера - 403.
	if rec := env.request(t, http.MethodGet, base+"/@eve:other.test?ver=10", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("make_join за чужого пользователя: %d %s", rec.Code, rec.Body.String())
	}

	// Версии комнаты нет в ver (по умолчанию ["1"]) - M_INCOMPATIBLE_ROOM_VERSION.
	for _, query := range []string{"", "?ver=11"} {
		rec := env.request(t, http.MethodGet, base+"/@bob:remote.test"+query, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "M_INCOMPATIBLE_ROOM_VERSION") {
			t.Fatalf("make_join ver %q: %d %s", query, rec.Code, rec.Body.String())
		}
	}

	// Не приглашенный в комнату по приглашениям (join_rule по умолчанию invite) - 403.
	if rec := env.request(t, http.MethodGet, base+"/@carol:remote.test?ver=10", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("make_join без приглашения: %d %s", rec.Code, rec.Body.String())
	}

	// Неизвестная комната - 404.
	if rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/make_join/!nope:test.local/@bob:remote.test?ver=10", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("make_join неизвестной комнаты: %d %s", rec.Code, rec.Body.String())
	}

	// make_leave: версия комнаты из rooms, а не всегда "10".
	if _, err := env.server.db.Pool().Exec(context.Background(),
		`UPDATE rooms SET room_version = '11' WHERE room_id = $1`, roomID); err != nil {
		t.Fatal(err)
	}

	rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/make_leave/"+roomID+"/@bob:remote.test", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"room_version":"11"`) {
		t.Fatalf("make_leave: %d %s", rec.Code, rec.Body.String())
	}
}

// TestFederationSendJoinChecks — send_join авторизуется по текущему
// состоянию комнаты, а не только по auth_events отправителя; принимается только
// join за себя от сервера отправителя.
func TestFederationSendJoinChecks(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!sendjoin:test.local"
	env.seedRoom(t, roomID)

	ctx := context.Background()
	authEvents := []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}

	send := func(t *testing.T, object map[string]any, eventID string) int {
		t.Helper()

		rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+eventID, object)

		return rec.Code
	}

	// join не за себя - 400.
	other, otherID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@carol:remote.test"),
		map[string]any{"membership": "join"}, authEvents, nil, 4)
	if code := send(t, other, otherID); code != http.StatusBadRequest {
		t.Fatalf("join за другого: %d", code)
	}

	// Не join - 400.
	leave, leaveID := env.member(t, roomID, "leave", authEvents, 4)
	if code := send(t, leave, leaveID); code != http.StatusBadRequest {
		t.Fatalf("leave в send_join: %d", code)
	}

	// event_id пути не совпадает с содержимым - 400.
	join, joinID := env.member(t, roomID, "join", authEvents, 4)
	if code := send(t, join, "$forged:remote.test"); code != http.StatusBadRequest {
		t.Fatalf("event_id пути не совпал: %d", code)
	}

	// Пользователь другого сервера (origin remote.test, sender other.test) - 403.
	foreign, foreignID := env.event(t, "@eve:other.test", roomID, "m.room.member", strPtr("@eve:other.test"),
		map[string]any{"membership": "join"}, authEvents, nil, 4)
	if code := send(t, foreign, foreignID); code != http.StatusForbidden {
		t.Fatalf("join чужого сервера: %d", code)
	}

	// bob забанен в текущем состоянии; его join ссылается на старое приглашение
	// (auth_events отправителя его пропускают) - 403.
	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ('$ban:test.local',$1,'m.room.member','@alice:test.local','@bob:remote.test','{"membership":"ban"}',4,4,'test.local',4)`,
		roomID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		UPDATE room_state SET event_id = '$ban:test.local'
		WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID); err != nil {
		t.Fatal(err)
	}

	if code := send(t, join, joinID); code != http.StatusForbidden {
		t.Fatalf("join забаненного: %d, ожидали 403", code)
	}

	var count int
	if err := env.server.db.Pool().QueryRow(ctx, `SELECT count(*) FROM events WHERE event_id = $1`, joinID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("join забаненного записан: %d %v", count, err)
	}
}

// TestFederationSendJoinInviteOnlyByStaleAuthEvents — вход в
// комнату по приглашениям по чужим (устаревшим) auth_events - 403. Отправитель
// ссылается на прежние join_rules public, текущие - invite.
func TestFederationSendJoinInviteOnlyByStaleAuthEvents(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!stalejoin:test.local"
	env.seedRoom(t, roomID)

	ctx := context.Background()

	for _, row := range []struct {
		id, rule string
		depth    int64
	}{{"$public:test.local", "public", 4}, {"$inviteonly:test.local", "invite", 5}} {
		if _, err := env.server.db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,'m.room.join_rules','@alice:test.local','',jsonb_build_object('join_rule', $3::text),$4,$4,'test.local',$4)`,
			row.id, roomID, row.rule, row.depth); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,'m.room.join_rules','','$inviteonly:test.local')`,
		roomID); err != nil {
		t.Fatal(err)
	}

	// Приглашение bob отозвано (kick): в текущем состоянии leave, а join
	// ссылается на прежнее приглашение - по auth_events он прошел бы.
	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ('$revoke:test.local',$1,'m.room.member','@alice:test.local','@bob:remote.test','{"membership":"leave"}',6,6,'test.local',6)`,
		roomID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		UPDATE room_state SET event_id = '$revoke:test.local'
		WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID); err != nil {
		t.Fatal(err)
	}

	revoked, revokedID := env.member(t, roomID, "join",
		[]string{"$create:test.local", "$pl:test.local", "$inviteonly:test.local", "$invite:test.local"}, 7)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+revokedID, revoked)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("вход по отозванному приглашению: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}

	join, joinID := env.event(t, "@carol:remote.test", roomID, "m.room.member", strPtr("@carol:remote.test"),
		map[string]any{"membership": "join"}, []string{"$create:test.local", "$pl:test.local", "$public:test.local"},
		[]string{"$public:test.local"}, 7)

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID, join)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("вход по устаревшим join_rules: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}

	// Когда комната действительно публичная, тот же вход без членства в
	// auth_events разрешен (первый join).
	if _, err := env.server.db.Pool().Exec(ctx, `
		UPDATE room_state SET event_id = '$public:test.local' WHERE room_id = $1 AND type = 'm.room.join_rules'`, roomID); err != nil {
		t.Fatal(err)
	}

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID, join)
	if rec.Code != http.StatusOK {
		t.Fatalf("вход в публичную комнату: %d %s", rec.Code, rec.Body.String())
	}
}

func strPtr(s string) *string { return &s }

// TestSendJoinFieldOrder — Synapse парсит ответ v2 send_join потоково и ждёт
// state → auth_chain → event → members_omitted → servers_in_room. Если state
// окажется не первым, Synapse отвечает «No create event in state».
func TestSendJoinFieldOrder(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!order:test.local"
	env.seedRoom(t, roomID)

	// auth_events обязаны включать invite и power_levels: иначе auth rules
	// отвергают join (403) и тест красный на main.
	joinEvent, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID, joinEvent)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_join: %d %s", rec.Code, rec.Body.String())
	}

	// Проверяем порядок ключей в сыром JSON: state должен идти первым.
	body := rec.Body.String()
	idx := func(key string) int { return strings.Index(body, `"`+key+`"`) }
	if !(idx("state") < idx("auth_chain") && idx("auth_chain") < idx("event") && idx("event") < idx("members_omitted") && idx("members_omitted") < idx("servers_in_room")) {
		t.Fatalf("порядок полей send_join неверный: %s", body[:min(200, len(body))])
	}
}

// TestSendJoinOmitMembers - MSC3706: при omit_members=true state без
// membership-событий и флаг members_omitted=true.
func TestSendJoinOmitMembers(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!omit:test.local"
	env.seedRoom(t, roomID)

	joinEvent, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)

	rec := env.request(t, http.MethodPut,
		"/_matrix/federation/v2/send_join/"+roomID+"/"+joinID+"?omit_members=true", joinEvent)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_join: %d %s", rec.Code, rec.Body.String())
	}

	var body struct {
		State          []map[string]any `json:"state"`
		MembersOmitted bool             `json:"members_omitted"`
		ServersInRoom  []string         `json:"servers_in_room"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !body.MembersOmitted {
		t.Fatal("members_omitted = false, ожидали true")
	}

	// При members_omitted servers_in_room обязателен (спека).
	if !slices.Equal(body.ServersInRoom, []string{"test.local"}) {
		t.Fatalf("servers_in_room = %v", body.ServersInRoom)
	}

	// Как Synapse (_get_event_ids_for_partial_state_join): при partial state join
	// отдаются все не-member события плюс ТЕКУЩЕЕ членство вступающего — оно
	// auth-событие его join. Остальные membership-события вырезаются.
	const joiner = "@bob:remote.test"

	joinerMembership := false

	for _, ev := range body.State {
		if ev["type"] != "m.room.member" {
			continue
		}

		if ev["state_key"] == joiner {
			joinerMembership = true

			continue
		}

		t.Fatalf("в state при omit_members остался чужой m.room.member: %v", ev["state_key"])
	}

	if !joinerMembership {
		t.Fatal("в state нет членства вступающего (нужно как auth-событие join)")
	}
}
