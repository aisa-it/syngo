//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Комнаты v12 по федерации: room_id - хеш m.room.create без домена,
// у create нет room_id, create не входит в auth_events, создатели - с
// бесконечным уровнем, state resolution v2.1.

const roomV12 = 12

// signV12 - событие remote.test по правилам v12: hashes, подпись по redaction
// v11+, event_id - reference hash. Возвращает PDU без event_id (generic) и id.
func (e *fedEnv) signV12(t *testing.T, object map[string]any) (map[string]any, string) {
	t.Helper()

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatalf("content hash: %v", err)
	}

	object["hashes"] = map[string]any{"sha256": hash}

	signed, err := eventjson.SignVersioned(e.remoteKey, "remote.test", object, roomV12)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	id, err := core.EventID(generic, roomV12)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	return generic, id
}

// remoteV12Room - комната v12 на remote.test: создатель @bob:remote.test,
// join_rules invite. События - как их отдал бы Synapse 1.165.
type remoteV12Room struct {
	env      *fedEnv
	roomID   string
	createID string
	// pdus - события по id (PDU без event_id); state - текущее состояние.
	pdus  map[string]map[string]any
	state map[string]string // type + "|" + state_key -> id
	last  string
	depth int
}

func newRemoteV12Room(t *testing.T, env *fedEnv) *remoteV12Room {
	t.Helper()

	room := &remoteV12Room{env: env, pdus: map[string]map[string]any{}, state: map[string]string{}}

	create, createID := env.signV12(t, map[string]any{
		"type": "m.room.create", "state_key": "", "sender": "@bob:remote.test",
		"content": map[string]any{"room_version": "12"}, "depth": 1, "origin_server_ts": 1_700_000_000_001,
		"prev_events": []string{}, "auth_events": []string{},
	})

	room.createID = createID
	room.roomID = "!" + createID[1:]
	room.pdus[createID] = create
	room.state["m.room.create|"] = createID
	room.last = createID
	room.depth = 1

	// Вход создателя: auth_events пуст - create в v12 в них не входит.
	room.add(t, "@bob:remote.test", "m.room.member", strPtr("@bob:remote.test"),
		map[string]any{"membership": "join"}, nil)
	room.add(t, "@bob:remote.test", "m.room.power_levels", strPtr(""),
		map[string]any{"users": map[string]any{}, "users_default": 0, "invite": 0}, []string{"m.room.member|@bob:remote.test"})
	room.add(t, "@bob:remote.test", "m.room.join_rules", strPtr(""),
		map[string]any{"join_rule": "invite"}, []string{"m.room.power_levels|", "m.room.member|@bob:remote.test"})
	room.add(t, "@bob:remote.test", "m.room.history_visibility", strPtr(""),
		map[string]any{"history_visibility": "shared"}, []string{"m.room.power_levels|", "m.room.member|@bob:remote.test"})

	return room
}

// add - событие комнаты поверх последнего; authKeys - ключи состояния для auth_events.
func (r *remoteV12Room) add(
	t *testing.T, sender, eventType string, stateKey *string, content map[string]any, authKeys []string,
) (map[string]any, string) {
	t.Helper()

	authIDs := []string{}
	for _, key := range authKeys {
		if id, ok := r.state[key]; ok {
			authIDs = append(authIDs, id)
		}
	}

	r.depth++

	object := map[string]any{
		"room_id": r.roomID, "type": eventType, "sender": sender, "content": content,
		"depth": r.depth, "origin_server_ts": 1_700_000_000_000 + int64(r.depth),
		"prev_events": []string{r.last}, "auth_events": authIDs,
	}

	if stateKey != nil {
		object["state_key"] = *stateKey
	}

	pdu, id := r.env.signV12(t, object)

	r.pdus[id] = pdu
	r.last = id

	if stateKey != nil {
		r.state[eventType+"|"+*stateKey] = id
	}

	return pdu, id
}

// statePDUs - текущее состояние комнаты (PDU).
func (r *remoteV12Room) statePDUs() []map[string]any {
	out := make([]map[string]any, 0, len(r.state))
	for _, id := range r.state {
		out = append(out, r.pdus[id])
	}

	return out
}

// TestFederationV12RemoteRoomInviteJoinChat - пользователь Synapse
// приглашает нашего пользователя в свою комнату v12 (версия по умолчанию в
// Synapse 1.165), тот входит и переписывается. Раньше /v2/invite отвечал
// M_UNSUPPORTED_ROOM_VERSION («The user's homeserver does not support the
// version of the room» в Element), make_join объявлял только ver=10&ver=11.
func TestFederationV12RemoteRoomInviteJoinChat(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICE")
	room := newRemoteV12Room(t, env)

	var (
		mu           sync.Mutex
		makeJoinPath string
	)

	mock := newFedMock(t, env, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case r.URL.Path == "/_matrix/key/v2/server":
			signed, err := signing.SignJSON(env.remoteKey, "remote.test", map[string]any{
				"server_name": "remote.test", "valid_until_ts": time.Now().Add(time.Hour).UnixMilli(),
				"verify_keys": map[string]any{env.remoteKey.KeyID: map[string]any{"key": env.remoteKey.Public()}},
			})
			if err != nil {
				t.Errorf("sign keys: %v", err)
			}

			_ = json.NewEncoder(w).Encode(signed)
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_join/"):
			mu.Lock()
			makeJoinPath = r.URL.RequestURI()
			mu.Unlock()

			if !slices.Contains(r.URL.Query()["ver"], "12") {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errcode": "M_INCOMPATIBLE_ROOM_VERSION", "room_version": "12",
				})

				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"room_version": "12",
				"event": map[string]any{
					"type": "m.room.member", "room_id": room.roomID, "sender": "@alice:test.local",
					"state_key": "@alice:test.local", "content": map[string]any{"membership": "join"},
					"depth": room.depth + 1, "origin_server_ts": 1_700_000_100_000,
					"prev_events": []string{room.last},
					"auth_events": []string{
						room.state["m.room.power_levels|"], room.state["m.room.join_rules|"],
						room.state["m.room.member|@alice:test.local"],
					},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v2/send_join/"):
			state := room.statePDUs()
			authChain := make([]map[string]any, 0, len(room.pdus))

			for _, pdu := range room.pdus {
				authChain = append(authChain, pdu)
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": state, "auth_chain": authChain, "event": json.RawMessage(body),
				"members_omitted": false, "servers_in_room": []string{"remote.test"},
			})
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_leave/"):
			mu.Lock()
			auth := []string{room.state["m.room.power_levels|"], room.state["m.room.member|@alice:test.local"]}
			mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{
				"room_version": "12",
				"event": map[string]any{
					"type": "m.room.member", "room_id": room.roomID, "sender": "@alice:test.local",
					"state_key": "@alice:test.local", "content": map[string]any{"membership": "leave"},
					"depth": 50, "origin_server_ts": 1_700_000_200_000,
					"prev_events": []string{room.last}, "auth_events": auth,
				},
			})
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v2/send_leave/"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// 1. Приглашение: /v2/invite c room_version 12 и invite_room_state из полных PDU (MSC4311).
	invite, inviteID := room.add(t, "@bob:remote.test", "m.room.member", strPtr("@alice:test.local"),
		map[string]any{"membership": "invite"}, []string{"m.room.power_levels|", "m.room.member|@bob:remote.test"})

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/invite/"+url.PathEscape(room.roomID)+"/"+inviteID,
		map[string]any{
			"room_version": "12", "event": invite,
			"invite_room_state": []any{
				room.pdus[room.createID], room.pdus[room.state["m.room.join_rules|"]],
				room.pdus[room.state["m.room.member|@bob:remote.test"]],
			},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("v2 invite v12: %d %s", rec.Code, rec.Body.String())
	}

	var invited struct {
		Event map[string]any `json:"event"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &invited); err != nil {
		t.Fatalf("invite response: %v", err)
	}

	if sigs, _ := invited.Event["signatures"].(map[string]any); sigs["test.local"] == nil {
		t.Fatalf("invite без нашей подписи: %s", rec.Body.String())
	}

	if id, err := core.EventID(invited.Event, roomV12); err != nil || id != inviteID {
		t.Fatalf("invite вернулся другим событием: %s != %s", id, inviteID)
	}

	if got := membershipRow(t, env, room.roomID, "@alice:test.local"); got != "invite" {
		t.Fatalf("membership после invite = %q", got)
	}

	if got := env.server.roomVersionOf(context.Background(), room.roomID); got != roomV12 {
		t.Fatalf("версия комнаты после invite = %d, ожидали 12", got)
	}

	// 2. Вход: make_join через сервер пригласившего (у room_id v12 нет домена) с ver=12.
	rec = do(t, env.server, http.MethodPost, "/_matrix/client/v3/join/"+url.PathEscape(room.roomID), `{}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("join v12: %d %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	joinPath := makeJoinPath
	mu.Unlock()

	if !strings.Contains(joinPath, "ver=12") {
		t.Fatalf("make_join без ver=12: %q", joinPath)
	}

	if got := membershipRow(t, env, room.roomID, "@alice:test.local"); got != "join" {
		t.Fatalf("membership после join = %q", got)
	}

	// Create комнаты сохранен под room_id комнаты, его PDU - без room_id.
	createPDU, err := env.server.db.EventPDU(context.Background(), room.createID)
	if err != nil || createPDU.RoomID != room.roomID || strings.Contains(string(createPDU.PDU), `"room_id"`) {
		t.Fatalf("create после join: %+v %v", createPDU, err)
	}

	// 3. Наше сообщение: auth_events без create, получатель - remote.test.
	ours := sendText(t, env.server, alice, room.roomID, "v12-1", "привет из synapse-go")

	if got := outboxFor(t, env.server, ours); !slices.Equal(got, []string{"remote.test"}) {
		t.Fatalf("получатели сообщения = %v", got)
	}

	pdus, err := env.server.FederationPDUs(context.Background(), []string{ours})
	if err != nil || len(pdus[ours]) == 0 {
		t.Fatalf("PDU сообщения: %v", err)
	}

	var message map[string]any
	if err := json.Unmarshal(pdus[ours], &message); err != nil {
		t.Fatalf("PDU сообщения: %v", err)
	}

	if message["room_id"] != room.roomID || slices.Contains(stringSlice(message["auth_events"]), room.createID) {
		t.Fatalf("PDU сообщения v12: %s", pdus[ours])
	}

	if id, err := core.EventID(withoutEventID(message), roomV12); err != nil || id != ours {
		t.Fatalf("event_id сообщения %s, по PDU %s", ours, id)
	}

	if err := eventjson.VerifyVersioned(withoutEventID(message), "test.local", roomV12,
		func(origin, keyID string) (string, int64, error) {
			return env.server.federationKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}); err != nil {
		t.Fatalf("подпись сообщения: %v", err)
	}

	// 4. Ответ bob по /send: принят и виден alice.
	var aliceJoin string
	_ = env.server.db.Pool().QueryRow(context.Background(),
		`SELECT event_id FROM room_memberships WHERE room_id = $1 AND user_id = '@alice:test.local'`,
		room.roomID).Scan(&aliceJoin)

	room.last = ours
	room.depth += 3

	reply, replyID := room.add(t, "@bob:remote.test", "m.room.message", nil,
		map[string]any{"msgtype": "m.text", "body": "ответ из synapse"},
		[]string{"m.room.power_levels|", "m.room.member|@bob:remote.test"})

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v1/send/v12-txn-1",
		map[string]any{"origin": "remote.test", "pdus": []any{reply}})
	if rec.Code != http.StatusOK {
		t.Fatalf("/send v12: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, env.server, http.MethodGet,
		"/_matrix/client/v3/rooms/"+url.PathEscape(room.roomID)+"/event/"+url.PathEscape(replyID), "", alice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ответ из synapse") {
		t.Fatalf("ответ bob не виден alice: %d %s", rec.Code, rec.Body.String())
	}

	if aliceJoin == "" {
		t.Fatal("нет join alice")
	}

	// 5. Выход: make_leave/send_leave через сервер комнаты (по участникам и create).
	mu.Lock()
	room.state["m.room.member|@alice:test.local"] = aliceJoin
	mu.Unlock()

	rec = do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(room.roomID)+"/leave", `{}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave v12: %d %s", rec.Code, rec.Body.String())
	}

	if got := membershipRow(t, env, room.roomID, "@alice:test.local"); got != "leave" ||
		!strings.Contains(mock.paths(), "PUT /_matrix/federation/v2/send_leave/") {
		t.Fatalf("membership после leave = %q, запросы %s", got, mock.paths())
	}
}

// v12IDs - event_id PDU ответа (по правилам v12) и PDU по id.
func v12IDs(t *testing.T, items any) ([]string, map[string]map[string]any) {
	t.Helper()

	list, _ := items.([]any)
	ids := make([]string, 0, len(list))
	byID := make(map[string]map[string]any, len(list))

	for _, item := range list {
		pdu, _ := item.(map[string]any)

		id, err := core.EventID(pdu, roomV12)
		if err != nil {
			t.Fatalf("event id: %v", err)
		}

		ids = append(ids, id)
		byID[id] = pdu
	}

	return ids, byID
}

// createV12Room - комната v12 alice (public_chat) и id ее create.
func createV12Room(t *testing.T, env *fedEnv, alice string) (roomID, createID string) {
	t.Helper()

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","room_version":"12","name":"v12"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom v12: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if !strings.HasPrefix(created.RoomID, "!") || strings.Contains(created.RoomID, ":") {
		t.Fatalf("room_id v12 = %q", created.RoomID)
	}

	return created.RoomID, "$" + created.RoomID[1:]
}

// TestFederationV12OurRoomRemoteJoin - пользователь Synapse входит в
// нашу комнату v12 и переписывается. make_join отдает шаблон v12 без create в
// auth_events (раньше - M_INCOMPATIBLE_ROOM_VERSION при любом ver),
// send_join, /state_ids, /event_auth и /backfill отдают create без room_id и
// с ним в auth-цепочке, /send принимает PDU v12, make_leave/send_leave - выход.
func TestFederationV12OurRoomRemoteJoin(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICE")
	roomID, createID := createV12Room(t, env, alice)
	room := url.PathEscape(roomID)
	ourKey := func(origin, keyID string) (string, int64, error) {
		return env.server.federationKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
	}

	// Сервер без v12 в ver - явный отказ с версией комнаты.
	rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/make_join/"+room+"/@bob:remote.test?ver=10&ver=11", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "M_INCOMPATIBLE_ROOM_VERSION") ||
		!strings.Contains(rec.Body.String(), `"room_version":"12"`) {
		t.Fatalf("make_join без ver=12: %d %s", rec.Code, rec.Body.String())
	}

	rec = env.request(t, http.MethodGet, "/_matrix/federation/v1/make_join/"+room+"/@bob:remote.test?ver=10&ver=11&ver=12", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("make_join v12: %d %s", rec.Code, rec.Body.String())
	}

	var template struct {
		RoomVersion string         `json:"room_version"`
		Event       map[string]any `json:"event"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &template); err != nil {
		t.Fatalf("make_join: %v", err)
	}

	if template.RoomVersion != "12" || len(stringSlice(template.Event["auth_events"])) == 0 ||
		slices.Contains(stringSlice(template.Event["auth_events"]), createID) {
		t.Fatalf("шаблон make_join v12: %s", rec.Body.String())
	}

	joinEvent, joinID := env.signV12(t, template.Event)

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+room+"/"+joinID, joinEvent)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_join v12: %d %s", rec.Code, rec.Body.String())
	}

	sendJoin := fedJSON(t, rec)

	stateIDs, state := v12IDs(t, sendJoin["state"])
	authIDs, _ := v12IDs(t, sendJoin["auth_chain"])

	if !slices.Contains(stateIDs, createID) || !slices.Contains(authIDs, createID) {
		t.Fatalf("send_join без create: state %v, auth_chain %v", stateIDs, authIDs)
	}

	if _, has := state[createID]["room_id"]; has {
		t.Fatalf("create в send_join с room_id: %v", state[createID])
	}

	for id, pdu := range state {
		if err := eventjson.VerifyVersioned(pdu, "test.local", roomV12, ourKey); err != nil {
			t.Fatalf("подпись %s в send_join: %v", id, err)
		}
	}

	if got := membershipRow(t, env, roomID, "@bob:remote.test"); got != "join" {
		t.Fatalf("membership bob = %q", got)
	}

	// /state_ids и /event_auth: create - в auth-цепочке join bob (в auth_events его нет).
	obj := fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/state_ids/"+room+"?event_id="+joinID, nil))
	if chain := stringSlice(obj["auth_chain_ids"]); !slices.Contains(chain, createID) ||
		!slices.Contains(stringSlice(obj["pdu_ids"]), createID) {
		t.Fatalf("state_ids v12: %v", obj)
	}

	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event_auth/"+room+"/"+joinID, nil))
	if chain, _ := v12IDs(t, obj["auth_chain"]); !slices.Contains(chain, createID) {
		t.Fatalf("event_auth v12 без create: %v", chain)
	}

	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event/"+createID, nil))
	if ids, pdus := v12IDs(t, obj["pdus"]); !slices.Equal(ids, []string{createID}) || pdus[createID]["room_id"] != nil {
		t.Fatalf("/event create v12: %v", obj)
	}

	obj = fedJSON(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/backfill/"+room+"?v="+joinID+"&limit=50", nil))
	if ids, _ := v12IDs(t, obj["pdus"]); !slices.Contains(ids, createID) || !slices.Contains(ids, joinID) {
		t.Fatalf("backfill v12: %v", ids)
	}

	// Сообщение bob по /send: принято и видно alice. create в auth_events и create
	// с room_id - отказ (формат PDU v12).
	plID := ""
	for id, pdu := range state {
		if pdu["type"] == "m.room.power_levels" {
			plID = id
		}
	}

	message := func(authEvents []string, body string) (map[string]any, string) {
		return env.signV12(t, map[string]any{
			"room_id": roomID, "type": "m.room.message", "sender": "@bob:remote.test",
			"content": map[string]any{"msgtype": "m.text", "body": body}, "depth": 100,
			"origin_server_ts": time.Now().UnixMilli(), "prev_events": []string{joinID}, "auth_events": authEvents,
		})
	}

	// Некорректный PDU - ошибка по этому PDU, транзакция 200.
	bad, badID := message([]string{createID, plID, joinID}, "с create в auth_events")
	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/v12-bad-1",
		map[string]any{"origin": "remote.test", "pdus": []any{bad}}); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), badID) || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("/send с create в auth_events: %d %s", rec.Code, rec.Body.String())
	}

	badCreate, badCreateID := env.signV12(t, map[string]any{
		"room_id": roomID, "type": "m.room.create", "state_key": "", "sender": "@bob:remote.test",
		"content": map[string]any{"room_version": "12"}, "depth": 1, "origin_server_ts": 1,
		"prev_events": []string{}, "auth_events": []string{},
	})
	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/v12-bad-2",
		map[string]any{"origin": "remote.test", "pdus": []any{badCreate}}); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), badCreateID+`":{}`) {
		t.Fatalf("/send create v12 с room_id: %d %s", rec.Code, rec.Body.String())
	}

	good, goodID := message([]string{plID, joinID}, "привет от bob")
	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/v12-1",
		map[string]any{"origin": "remote.test", "pdus": []any{good}}); rec.Code != http.StatusOK {
		t.Fatalf("/send v12: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, env.server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/event/"+url.PathEscape(goodID), "", alice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "привет от bob") {
		t.Fatalf("сообщение bob не видно alice: %d %s", rec.Code, rec.Body.String())
	}

	// make_leave/send_leave.
	rec = env.request(t, http.MethodGet, "/_matrix/federation/v1/make_leave/"+room+"/@bob:remote.test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("make_leave v12: %d %s", rec.Code, rec.Body.String())
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &template); err != nil {
		t.Fatalf("make_leave: %v", err)
	}

	if template.RoomVersion != "12" || slices.Contains(stringSlice(template.Event["auth_events"]), createID) {
		t.Fatalf("шаблон make_leave v12: %s", rec.Body.String())
	}

	leaveEvent, leaveID := env.signV12(t, template.Event)

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v2/send_leave/"+room+"/"+leaveID, leaveEvent)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_leave v12: %d %s", rec.Code, rec.Body.String())
	}

	if got := membershipRow(t, env, roomID, "@bob:remote.test"); got != "leave" {
		t.Fatalf("membership bob после send_leave = %q", got)
	}
}

// TestFederationV12OutboundInvite - приглашение пользователя Synapse
// в нашу комнату v12 уходит с room_version 12 и invite_room_state из полных
// подписанных PDU (MSC4311), create - без room_id.
func TestFederationV12OutboundInvite(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICE")
	roomID, createID := createV12Room(t, env, alice)

	var (
		mu      sync.Mutex
		request map[string]any
	)

	newFedMock(t, env, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(r.URL.Path, "/v2/invite/") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)

		mu.Lock()
		request = parsed
		mu.Unlock()

		event, _ := parsed["event"].(map[string]any)

		signed, err := eventjson.SignVersioned(env.remoteKey, "remote.test", cloneObject(event), roomV12)
		if err != nil {
			t.Errorf("countersign: %v", err)
		}

		event["signatures"].(map[string]any)["remote.test"] = signed["signatures"].(map[string]any)["remote.test"]

		_ = json.NewEncoder(w).Encode(map[string]any{"event": event})
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/invite",
		`{"user_id":"@carol:remote.test"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite v12: %d %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()

	if request["room_version"] != "12" {
		t.Fatalf("room_version приглашения = %v", request["room_version"])
	}

	event, _ := request["event"].(map[string]any)
	if slices.Contains(stringSlice(event["auth_events"]), createID) {
		t.Fatalf("invite v12 с create в auth_events: %v", event)
	}

	ids, pdus := v12IDs(t, request["invite_room_state"])
	if !slices.Contains(ids, createID) || pdus[createID]["room_id"] != nil {
		t.Fatalf("invite_room_state v12 без create-PDU: %v", request["invite_room_state"])
	}

	for id, pdu := range pdus {
		if pdu["signatures"] == nil || pdu["hashes"] == nil {
			t.Fatalf("invite_room_state %s не полный PDU: %v", id, pdu)
		}
	}

	if got := membershipRow(t, env, roomID, "@carol:remote.test"); got != "invite" {
		t.Fatalf("membership carol = %q", got)
	}
}
