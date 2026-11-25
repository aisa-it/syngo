//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeRemote - сервер remote.test для исходящих федеративных запросов ключей.
type fakeRemote struct {
	mu       sync.Mutex
	streamID int64
	devices  map[string]map[string]any // device_id -> keys
	master   map[string]any
	calls    map[string]int
	claims   []map[string]any
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{devices: map[string]map[string]any{}, calls: map[string]int{}}
}

func remoteDeviceKeys(deviceID string) map[string]any {
	return map[string]any{
		"user_id": "@bob:remote.test", "device_id": deviceID,
		"algorithms": []string{"m.olm.v1.curve25519-aes-sha2"},
		"keys": map[string]string{
			"curve25519:" + deviceID: "curve-" + deviceID, "ed25519:" + deviceID: "ed-" + deviceID,
		},
		"signatures": map[string]any{"@bob:remote.test": map[string]string{"ed25519:" + deviceID: "sig"}},
	}
}

func (f *fakeRemote) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[path]
}

func (f *fakeRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	if strings.HasPrefix(path, "/_matrix/federation/v1/user/devices/") {
		path = "/_matrix/federation/v1/user/devices"
	}

	f.calls[path]++

	w.Header().Set("Content-Type", "application/json")

	switch path {
	case "/_matrix/federation/v1/user/devices":
		devices := []map[string]any{}
		for id, keys := range f.devices {
			devices = append(devices, map[string]any{"device_id": id, "keys": keys, "device_display_name": "Phone " + id})
		}

		resp := map[string]any{"user_id": "@bob:remote.test", "stream_id": f.streamID, "devices": devices}
		if f.master != nil {
			resp["master_key"] = f.master
		}

		_ = json.NewEncoder(w).Encode(resp)
	case "/_matrix/federation/v1/user/keys/query":
		byDevice := map[string]any{}
		for id, keys := range f.devices {
			byDevice[id] = keys
		}

		resp := map[string]any{"device_keys": map[string]any{
			"@bob:remote.test": byDevice,
			// Чужой пользователь в ответе remote.test отбрасывается.
			"@eve:evil.test": map[string]any{"E": map[string]any{"user_id": "@eve:evil.test", "device_id": "E"}},
		}}
		if f.master != nil {
			resp["master_keys"] = map[string]any{"@bob:remote.test": f.master}
		}

		_ = json.NewEncoder(w).Encode(resp)
	case "/_matrix/federation/v1/user/keys/claim":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.claims = append(f.claims, body)

		_ = json.NewEncoder(w).Encode(map[string]any{"one_time_keys": map[string]any{
			"@bob:remote.test": map[string]any{"RDEV": map[string]any{"signed_curve25519:AAAA": map[string]any{"key": "otk"}}},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED"}`))
	}
}

// fedRoomWithRemote - комната alice (наш) с joined @bob:remote.test.
func fedRoomWithRemote(t *testing.T, env *fedEnv) (alice, roomID string) {
	t.Helper()

	alice = registerHTTP(t, env.server, "alice", "ALICEDEV")
	roomID = createFedRoom(t, env.server, alice)
	setRemoteMember(t, env.server, roomID, "@bob:remote.test", "join")

	return alice, roomID
}

// sendEDUs - транзакция /send только с EDU.
func (e *fedEnv) sendEDUs(t *testing.T, txnID string, edus ...map[string]any) {
	t.Helper()

	rec := e.request(t, http.MethodPut, "/_matrix/federation/v1/send/"+txnID,
		map[string]any{"origin": "remote.test", "pdus": []any{}, "edus": edus})
	if rec.Code != http.StatusOK {
		t.Fatalf("/send: %d %s", rec.Code, rec.Body.String())
	}
}

// TestFederationTypingAndReceiptEDU - m.typing и m.receipt удаленного участника
// видны нашему клиенту; EDU от имени пользователя другого сервера и от не
// участника отбрасываются. Раньше EDU игнорировались целиком.
func TestFederationTypingAndReceiptEDU(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := fedRoomWithRemote(t, env)
	eventID := sendText(t, env.server, alice, roomID, "t1", "hi")

	env.sendEDUs(t, "edu-typing",
		map[string]any{"edu_type": "m.typing", "content": map[string]any{
			"room_id": roomID, "user_id": "@bob:remote.test", "typing": true,
		}},
		// Не участник комнаты.
		map[string]any{"edu_type": "m.typing", "content": map[string]any{
			"room_id": roomID, "user_id": "@carol:remote.test", "typing": true,
		}},
		// Пользователь другого сервера.
		map[string]any{"edu_type": "m.typing", "content": map[string]any{
			"room_id": roomID, "user_id": "@alice:test.local", "typing": true,
		}},
		map[string]any{"edu_type": "m.receipt", "content": map[string]any{
			roomID: map[string]any{"m.read": map[string]any{
				"@bob:remote.test": map[string]any{"event_ids": []string{eventID}, "data": map[string]any{"ts": 123}},
			}},
		}},
	)

	typing := env.server.ephemeral.Typing(roomID)
	if !slices.Equal(typing, []string{"@bob:remote.test"}) {
		t.Fatalf("typing = %v, ожидали только @bob:remote.test", typing)
	}

	receipts := env.server.ephemeral.UserReceipts(roomID, "@bob:remote.test")
	if len(receipts) != 1 || receipts[0].EventID != eventID || receipts[0].TS != 123 {
		t.Fatalf("receipts = %+v", receipts)
	}

	var stored int
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM receipts WHERE user_id = '@bob:remote.test' AND event_id = $1`, eventID).Scan(&stored); err != nil {
		t.Fatalf("receipts: %v", err)
	}

	if stored != 1 {
		t.Fatalf("receipt удаленного не сохранен: %d", stored)
	}
}

// TestFederationDirectToDeviceEDU - m.direct_to_device доходит до нашего
// устройства один раз (дедуп по message_id), sender чужого сервера отвергается.
func TestFederationDirectToDeviceEDU(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICEDEV")

	edu := map[string]any{"edu_type": "m.direct_to_device", "content": map[string]any{
		"sender": "@bob:remote.test", "type": "m.room.encrypted", "message_id": "m1",
		"messages": map[string]any{
			"@alice:test.local": map[string]any{"ALICEDEV": map[string]any{"ciphertext": "olm"}},
			// Чужой пользователь в сообщении: не наш - не доставляется.
			"@zed:other.test": map[string]any{"Z": map[string]any{"ciphertext": "x"}},
		},
	}}
	forged := map[string]any{"edu_type": "m.direct_to_device", "content": map[string]any{
		"sender": "@mallory:evil.test", "type": "m.room.encrypted", "message_id": "m2",
		"messages": map[string]any{"@alice:test.local": map[string]any{"*": map[string]any{"ciphertext": "forged"}}},
	}}

	env.sendEDUs(t, "edu-td-1", edu, forged)
	// Тот же message_id в другой транзакции.
	env.sendEDUs(t, "edu-td-2", edu)

	resp := syncOnce(t, env.server, alice)
	if len(resp.ToDevice.Events) != 1 {
		t.Fatalf("to-device: %d событий, ожидали 1", len(resp.ToDevice.Events))
	}

	if ev := resp.ToDevice.Events[0]; ev.Type != "m.room.encrypted" || ev.Sender != "@bob:remote.test" {
		t.Fatalf("to-device событие: %+v", ev)
	}
}

// TestFederationDeviceListUpdateResync - m.device_list_update участника общей
// комнаты: нет кэша - ресинк через /user/devices; следующее изменение с
// известным prev_id применяется без запроса; дыра в prev_id - снова ресинк.
// /keys/query отдает устройства из кэша, /keys/changes и /sync видят изменение.
func TestFederationDeviceListUpdateResync(t *testing.T) {
	env := newFedEnv(t)
	alice, _ := fedRoomWithRemote(t, env)

	remote := newFakeRemote()
	remote.streamID = 5
	remote.devices["RDEV"] = remoteDeviceKeys("RDEV")

	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	env.setTransport(srv.URL)

	before := syncOnce(t, env.server, alice)

	update := func(deviceID string, stream int64, prev []int64, deleted bool) map[string]any {
		content := map[string]any{
			"user_id": "@bob:remote.test", "device_id": deviceID,
			"stream_id": stream, "prev_id": prev, "deleted": deleted,
		}
		if !deleted {
			content["keys"] = remoteDeviceKeys(deviceID)
		}

		return map[string]any{"edu_type": "m.device_list_update", "content": content}
	}

	env.sendEDUs(t, "dl-1", update("RDEV", 5, []int64{4}, false))

	if n := remote.count("/_matrix/federation/v1/user/devices"); n != 1 {
		t.Fatalf("ресинк: %d запросов /user/devices, ожидали 1", n)
	}

	// Новое устройство с prev_id = последний stream_id - без ресинка.
	env.sendEDUs(t, "dl-2", update("RDEV2", 6, []int64{5}, false))

	if n := remote.count("/_matrix/federation/v1/user/devices"); n != 1 {
		t.Fatalf("известный prev_id вызвал ресинк (%d)", n)
	}

	keys := queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[]}}`)
	if _, ok := keys.DeviceKeys["@bob:remote.test"]["RDEV2"]; !ok || len(keys.DeviceKeys["@bob:remote.test"]) != 2 {
		t.Fatalf("keys/query из кэша: %+v", keys.DeviceKeys)
	}

	if n := remote.count("/_matrix/federation/v1/user/keys/query"); n != 0 {
		t.Fatalf("отслеживаемый пользователь запрошен по федерации (%d)", n)
	}

	// Дыра: prev_id 8 не видели - ресинк, кэш заменяется ответом сервера.
	env.sendEDUs(t, "dl-3", update("RDEV3", 9, []int64{8}, false))

	if n := remote.count("/_matrix/federation/v1/user/devices"); n != 2 {
		t.Fatalf("дыра в stream_id без ресинка (%d)", n)
	}

	keys = queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[]}}`)
	if len(keys.DeviceKeys["@bob:remote.test"]) != 1 {
		t.Fatalf("после ресинка ожидали одно устройство: %+v", keys.DeviceKeys)
	}

	after := syncWithSince(t, env.server, alice, before.NextBatch)
	if !slices.Contains(after.DeviceLists.Changed, "@bob:remote.test") {
		t.Fatalf("device_lists.changed = %v", after.DeviceLists.Changed)
	}

	rec := do(t, env.server, http.MethodGet, "/_matrix/client/v3/keys/changes?from="+before.NextBatch, "", alice)

	var changes keysChangesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &changes)

	if !slices.Contains(changes.Changed, "@bob:remote.test") {
		t.Fatalf("keys/changes = %s", rec.Body.String())
	}
}

// TestFederationDeviceListUpdateIgnoredWithoutSharedRoom - изменение устройств
// пользователя без общей комнаты не отслеживается (как в Synapse).
func TestFederationDeviceListUpdateIgnoredWithoutSharedRoom(t *testing.T) {
	env := newFedEnv(t)
	_ = registerHTTP(t, env.server, "alice", "ALICEDEV")

	remote := newFakeRemote()

	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	env.setTransport(srv.URL)

	env.sendEDUs(t, "dl-none", map[string]any{"edu_type": "m.device_list_update", "content": map[string]any{
		"user_id": "@bob:remote.test", "device_id": "RDEV", "stream_id": 1, "prev_id": []int64{},
	}})

	if n := remote.count("/_matrix/federation/v1/user/devices"); n != 0 {
		t.Fatalf("ресинк без общей комнаты (%d)", n)
	}
}

// queryKeysResponse - разбор ответа /keys/query.
type queryKeysResponse struct {
	DeviceKeys map[string]map[string]json.RawMessage `json:"device_keys"`
	MasterKeys map[string]json.RawMessage            `json:"master_keys"`
	Failures   map[string]json.RawMessage            `json:"failures"`
}

func queryKeys(t *testing.T, server *Server, token, body string) queryKeysResponse {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/query: %d %s", rec.Code, rec.Body.String())
	}

	var resp queryKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode keys/query: %v", err)
	}

	return resp
}

// TestFederationSigningKeyUpdateEDU - m.signing_key_update отслеживаемого
// пользователя меняет его master key в /keys/query.
func TestFederationSigningKeyUpdateEDU(t *testing.T) {
	env := newFedEnv(t)
	alice, _ := fedRoomWithRemote(t, env)

	remote := newFakeRemote()
	remote.streamID = 1
	remote.devices["RDEV"] = remoteDeviceKeys("RDEV")
	remote.master = map[string]any{
		"user_id": "@bob:remote.test", "usage": []string{"master"},
		"keys": map[string]string{"ed25519:MASTER1": "MASTER1"},
	}

	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	env.setTransport(srv.URL)

	// Первый запрос ключей участника общей комнаты - ресинк, дальше из кэша.
	keys := queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[]}}`)
	if !strings.Contains(string(keys.MasterKeys["@bob:remote.test"]), "MASTER1") {
		t.Fatalf("master key: %s", keys.MasterKeys["@bob:remote.test"])
	}

	env.sendEDUs(t, "sk-1", map[string]any{"edu_type": "m.signing_key_update", "content": map[string]any{
		"user_id": "@bob:remote.test",
		"master_key": map[string]any{
			"user_id": "@bob:remote.test", "usage": []string{"master"},
			"keys": map[string]string{"ed25519:MASTER2": "MASTER2"},
		},
	}})

	keys = queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[]}}`)
	if !strings.Contains(string(keys.MasterKeys["@bob:remote.test"]), "MASTER2") {
		t.Fatalf("master key после m.signing_key_update: %s", keys.MasterKeys["@bob:remote.test"])
	}

	if n := remote.count("/_matrix/federation/v1/user/devices"); n != 1 {
		t.Fatalf("/user/devices: %d, ожидали один ресинк", n)
	}
}

// TestKeysQueryClaimRemoteUsers - /keys/query и /keys/claim для пользователей
// других серверов идут к их серверу (без общей комнаты - без кэша), ответ
// чужого сервера о не своих пользователях отбрасывается, недоступный сервер -
// в failures. Раньше удаленные молча выбрасывались из запроса.
func TestKeysQueryClaimRemoteUsers(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICEDEV")

	remote := newFakeRemote()
	remote.devices["RDEV"] = remoteDeviceKeys("RDEV")

	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	env.setTransport(srv.URL)

	keys := queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[]}}`)
	if _, ok := keys.DeviceKeys["@bob:remote.test"]["RDEV"]; !ok {
		t.Fatalf("ключи удаленного: %+v", keys.DeviceKeys)
	}

	if _, ok := keys.DeviceKeys["@eve:evil.test"]; ok {
		t.Fatalf("ответ remote.test о пользователе evil.test принят")
	}

	if remote.count("/_matrix/federation/v1/user/devices") != 0 {
		t.Fatalf("пользователь без общей комнаты попал в кэш")
	}

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/keys/claim",
		`{"one_time_keys":{"@bob:remote.test":{"RDEV":"signed_curve25519"}}}`, alice)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "signed_curve25519:AAAA") {
		t.Fatalf("keys/claim: %d %s", rec.Code, rec.Body.String())
	}

	// Сервер недоступен: failures, а не 500 и не пустой ответ без причины.
	srv.Close()

	keys = queryKeys(t, env.server, alice, `{"device_keys":{"@bob:remote.test":[],"@alice:test.local":[]}}`)
	if _, ok := keys.Failures["remote.test"]; !ok {
		t.Fatalf("failures = %v, ожидали remote.test", keys.Failures)
	}

	if _, ok := keys.DeviceKeys["@alice:test.local"]; !ok {
		t.Fatalf("локальный пользователь пропал из ответа: %+v", keys.DeviceKeys)
	}
}
