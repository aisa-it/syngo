//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// seedFederationRoom создаёт комнату с линейным DAG (id - "$<имя>:<roomID>"):
//
//	d1 create, d2 alice join, d3 power_levels, d4 m1, d5 topic, d6 invite bob,
//	d7 bob join, d8 m2 (bob), d9 bob join (displayname), d10 m3 (alice).
//
// Текущее состояние: create, alice, power_levels, topic и членство bob -
// приглашение (сервер remote.test не в комнате) или последний join.
func (e *fedEnv) seedFederationRoom(t *testing.T, roomID string, bobJoined bool) {
	t.Helper()

	ctx := context.Background()

	if _, err := e.server.db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1,'@alice:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	id := func(name string) string { return "$" + name + ":" + roomID }

	const (
		alice = "@alice:test.local"
		bob   = "@bob:remote.test"
	)

	rows := []struct {
		name, typ, sender string
		state             *string
		content           string
		auth              []string
	}{
		{"create", "m.room.create", alice, strPtr(""), `{"creator":"@alice:test.local"}`, nil},
		{"alice", "m.room.member", alice, strPtr(alice), `{"membership":"join"}`, []string{"create"}},
		{"pl", "m.room.power_levels", alice, strPtr(""), `{"users":{"@alice:test.local":100}}`, []string{"create", "alice"}},
		{"m1", "m.room.message", alice, nil, `{"body":"1"}`, []string{"create", "pl", "alice"}},
		{"topic", "m.room.topic", alice, strPtr(""), `{"topic":"t"}`, []string{"create", "pl", "alice"}},
		{"invite", "m.room.member", alice, strPtr(bob), `{"membership":"invite"}`, []string{"create", "pl", "alice"}},
		{"bobjoin", "m.room.member", bob, strPtr(bob), `{"membership":"join"}`, []string{"create", "pl", "invite"}},
		{"m2", "m.room.message", bob, nil, `{"body":"2"}`, []string{"create", "pl", "bobjoin"}},
		{"bobjoin2", "m.room.member", bob, strPtr(bob), `{"membership":"join","displayname":"Bob"}`, []string{"create", "pl", "bobjoin"}},
		{"m3", "m.room.message", alice, nil, `{"body":"3"}`, []string{"create", "pl", "alice"}},
	}

	prev := []string{}

	for i, row := range rows {
		auth := make([]string, 0, len(row.auth))
		for _, name := range row.auth {
			auth = append(auth, id(name))
		}

		if _, err := e.server.db.Pool().Exec(ctx, `
			INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
			                    depth, origin_server_ts, origin, stream_ordering)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,1700000000000,$10,$9)`,
			id(row.name), roomID, row.typ, row.sender, row.state, []byte(row.content), prev, auth,
			int64(i+1), domainOf(row.sender)); err != nil {
			t.Fatalf("insert event %s: %v", row.name, err)
		}

		prev = []string{id(row.name)}
	}

	bobState := "invite"
	if bobJoined {
		bobState = "bobjoin2"
	}

	for _, name := range []string{"create", "alice", "pl", "topic", bobState} {
		if _, err := e.server.db.Pool().Exec(ctx, `
			INSERT INTO room_state (room_id, type, state_key, event_id)
			SELECT room_id, type, state_key, event_id FROM events WHERE event_id = $1`, id(name)); err != nil {
			t.Fatalf("state: %v", err)
		}
	}
}

// fedJSON - тело ответа 200 федерации как объект.
func fedJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// По спеке ответы - обычные объекты, не v1-обёртка [200, {...}]: Synapse не
	// парсит обёртку («Failed to parse response»).
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}

	return obj
}

// seededNames - имена событий seedFederationRoom по глубине (depth = индекс + 1).
var seededNames = []string{"create", "alice", "pl", "m1", "topic", "invite", "bobjoin", "m2", "bobjoin2", "m3"}

// pduNames - короткие имена событий seedFederationRoom: id ("$m1:!r" -> "m1")
// или PDU (в PDU v3+ нет event_id - имя по depth).
func pduNames(t *testing.T, roomID string, items any) []string {
	t.Helper()

	list, _ := items.([]any)
	names := make([]string, 0, len(list))

	for _, item := range list {
		if id, ok := item.(string); ok {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(id, "$"), ":"+roomID))

			continue
		}

		object, _ := item.(map[string]any)
		depth, _ := object["depth"].(float64)

		if depth < 1 || int(depth) > len(seededNames) {
			t.Fatalf("PDU не из seedFederationRoom: %v", object)
		}

		names = append(names, seededNames[int(depth)-1])
	}

	return names
}

// TestFederationStateEndpoints — state/state_ids/event/event_auth/
// get_missing_events/backfill только серверу с участником комнаты; состояние
// на событии; рекурсивная auth-цепочка; обход DAG.
func TestFederationStateEndpoints(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!state:test.local"
	env.seedFederationRoom(t, roomID, false)

	id := func(name string) string { return "$" + name + ":" + roomID }
	missingBody := map[string]any{"earliest_events": []string{id("m1")}, "latest_events": []string{id("m3")}, "limit": 10}

	endpoints := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/_matrix/federation/v1/state/" + roomID, nil},
		{http.MethodGet, "/_matrix/federation/v1/state_ids/" + roomID + "?event_id=" + id("m3"), nil},
		{http.MethodGet, "/_matrix/federation/v1/event/" + id("m1"), nil},
		{http.MethodGet, "/_matrix/federation/v1/event_auth/" + roomID + "/" + id("m2"), nil},
		{http.MethodPost, "/_matrix/federation/v1/get_missing_events/" + roomID, missingBody},
		{http.MethodGet, "/_matrix/federation/v1/backfill/" + roomID + "?v=" + id("m2") + "&limit=3", nil},
	}

	// remote.test только приглашен - не в комнате: 403 на всех ручках (раньше 200).
	for _, ep := range endpoints {
		if rec := env.request(t, ep.method, ep.path, ep.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s для сервера не из комнаты: %d %s", ep.method, ep.path, rec.Code, rec.Body.String())
		}
	}

	// bob вошел - сервер в комнате.
	if _, err := env.server.db.Pool().Exec(context.Background(), `
		UPDATE room_state SET event_id = $2 WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`,
		roomID, id("bobjoin2")); err != nil {
		t.Fatal(err)
	}

	// /state без event_id - текущее состояние.
	obj := fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/state/"+roomID, nil))
	if got := pduNames(t, roomID, obj["pdus"]); len(got) != 5 {
		t.Fatalf("state pdus = %v, ожидали 5", got)
	}

	// /state_ids?event_id=topic - состояние ДО topic (без него).
	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/state_ids/"+roomID+"?event_id="+id("topic"), nil))
	if got := pduNames(t, roomID, obj["pdu_ids"]); !slices.Equal(got, []string{"create", "alice", "pl"}) {
		t.Fatalf("state_ids на topic = %v", got)
	}

	// /state_ids?event_id=m3: последнее членство bob; auth-цепочка рекурсивная -
	// invite bob доступен только через bobjoin2 -> bobjoin -> invite.
	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/state_ids/"+roomID+"?event_id="+id("m3"), nil))
	if got := pduNames(t, roomID, obj["pdu_ids"]); !slices.Equal(got, []string{"create", "alice", "bobjoin2", "pl", "topic"}) {
		t.Fatalf("state_ids на m3 = %v", got)
	}

	if got := pduNames(t, roomID, obj["auth_chain_ids"]); !slices.Equal(got, []string{"alice", "bobjoin", "create", "invite", "pl"}) {
		t.Fatalf("auth_chain_ids на m3 = %v", got)
	}

	if rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/state_ids/"+roomID+"?event_id=$nope:test.local", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("state_ids на неизвестном событии: %d", rec.Code)
	}

	// /event/{id}: Transaction {origin, origin_server_ts, pdus}, не [200, …].
	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event/"+id("m1"), nil))
	if got, _ := obj["pdus"].([]any); len(got) != 1 || obj["origin"] != "test.local" {
		t.Fatalf("event: %v", obj)
	}

	// /event_auth - рекурсивная цепочка.
	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event_auth/"+roomID+"/"+id("m2"), nil))
	if got := pduNames(t, roomID, obj["auth_chain"]); len(got) != 5 {
		t.Fatalf("event_auth = %v, ожидали create, alice, pl, invite, bobjoin", got)
	}

	// get_missing_events: между m1 и m3, без самих m1 и m3, от старых к новым.
	missing := func(body map[string]any) []string {
		t.Helper()

		var reply struct {
			Events []any `json:"events"`
		}

		rec := env.request(t, http.MethodPost, "/_matrix/federation/v1/get_missing_events/"+roomID, body)
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reply) != nil {
			t.Fatalf("get_missing_events: %d %s", rec.Code, rec.Body.String())
		}

		return pduNames(t, roomID, reply.Events)
	}

	if got := missing(missingBody); !slices.Equal(got, []string{"topic", "invite", "bobjoin", "m2", "bobjoin2"}) {
		t.Fatalf("get_missing_events = %v", got)
	}

	withDepth := map[string]any{"earliest_events": missingBody["earliest_events"], "latest_events": missingBody["latest_events"], "min_depth": 7}
	if got := missing(withDepth); !slices.Equal(got, []string{"bobjoin", "m2", "bobjoin2"}) {
		t.Fatalf("get_missing_events min_depth = %v", got)
	}

	limited := map[string]any{"earliest_events": missingBody["earliest_events"], "latest_events": missingBody["latest_events"], "limit": 2}
	if got := missing(limited); !slices.Equal(got, []string{"m2", "bobjoin2"}) {
		t.Fatalf("get_missing_events limit = %v", got)
	}

	// backfill от m2 по DAG назад, с самим m2, не больше limit.
	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/backfill/"+roomID+"?v="+id("m2")+"&limit=3", nil))
	if got := pduNames(t, roomID, obj["pdus"]); !slices.Equal(got, []string{"m2", "bobjoin", "invite"}) {
		t.Fatalf("backfill = %v", got)
	}
}

// TestFederationHistoryVisibilityAndACL — событие, которое сервер не
// мог видеть (history_visibility joined до входа его участника), отдается
// отредактированным; сервер из deny в m.room.server_acl получает 403.
func TestFederationHistoryVisibilityAndACL(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!hv:test.local"
	env.seedFederationRoom(t, roomID, true)

	ctx := context.Background()
	id := func(name string) string { return "$" + name + ":" + roomID }

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ($1,$2,'m.room.history_visibility','@alice:test.local','','{"history_visibility":"joined"}',2,1700000000000,'test.local',100)`,
		id("hv"), roomID); err != nil {
		t.Fatal(err)
	}

	content := func(name string) map[string]any {
		t.Helper()

		obj := fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event/"+id(name), nil))
		pdus, _ := obj["pdus"].([]any)
		pdu, _ := pdus[0].(map[string]any)
		body, _ := pdu["content"].(map[string]any)

		return body
	}

	// m1 (до входа bob) - без содержимого; m2 (после) - целиком.
	if got := content("m1"); len(got) != 0 {
		t.Fatalf("m1 до входа сервера отдан с содержимым: %v", got)
	}

	if got := content("m2"); got["body"] != "2" {
		t.Fatalf("m2 после входа: %v", got)
	}

	// ACL: remote.test в deny - 403, даже будучи в комнате.
	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ($1,$2,'m.room.server_acl','@alice:test.local','','{"allow":["*"],"deny":["remote.test"]}',11,1700000000000,'test.local',101)`,
		id("acl"), roomID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,'m.room.server_acl','',$2)`, roomID, id("acl")); err != nil {
		t.Fatal(err)
	}

	if rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/state_ids/"+roomID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("state_ids при ACL deny: %d %s", rec.Code, rec.Body.String())
	}
}

// TestFederationInviteIncoming — приглашение в нашу комнату от
// сервера, чей пользователь в ней не участник, отклоняется и не пишется
// (раньше принималось и закреплялось этим тестом).
func TestFederationInviteIncoming(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!invite:test.local"
	env.seedFederationRoom(t, roomID, false)

	invite, eventID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@carol:test.local"),
		map[string]any{"membership": "invite"},
		[]string{"$create:" + roomID, "$pl:" + roomID, "$invite:" + roomID}, []string{"$m3:" + roomID}, 11)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/invite/"+roomID+"/"+eventID, invite)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invite от не-участника: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}

	var count int
	if err := env.server.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM room_memberships WHERE room_id=$1 AND user_id='@carol:test.local'", roomID).Scan(&count); err != nil {
		t.Fatalf("membership: %v", err)
	}

	if count != 0 {
		t.Fatal("отклоненное приглашение записано в room_memberships")
	}
}

// TestFederationOutboxFanOut — локальное событие попадает в outbox для удалённого destination.
func TestFederationOutboxFanOut(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"name":"fed","preset":"public_chat","visibility":"public"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Есть удалённый участник комнаты (joined-сервер - получатель).
	setRemoteMember(t, server, created.RoomID, "@bob:remote.test", "join")

	rec = do(t, server, http.MethodPut,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/txn-1",
		`{"msgtype":"m.text","body":"hello","txn_id":"t1"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := server.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM federation_outbox WHERE destination='remote.test'").Scan(&count); err != nil {
		t.Fatalf("outbox count: %v", err)
	}

	if count != 1 {
		t.Fatalf("в outbox %d записей, ожидали 1", count)
	}

	batch, err := server.db.ClaimOutboxBatch(context.Background(), store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: time.Now(), Lease: time.Minute, NewTxnID: "t1",
	})
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("claim: %v err=%v", batch, err)
	}

	// В очереди - ссылка на событие; PDU собирается и подписывается при отправке.
	pdus, err := server.FederationPDUs(context.Background(), []string{batch.Items[0].EventID})
	if err != nil {
		t.Fatalf("FederationPDUs: %v", err)
	}

	pdu := pdus[batch.Items[0].EventID]

	if !strings.Contains(string(pdu), "hello") {
		t.Fatalf("PDU без тела сообщения: %s", pdu)
	}

	var object map[string]any
	if err := json.Unmarshal(pdu, &object); err != nil {
		t.Fatalf("pdu json: %v", err)
	}

	if object["signatures"] == nil {
		t.Fatal("PDU в outbox должен быть подписан")
	}

	_ = signing.ErrNoSignature
	_ = time.Second
}
