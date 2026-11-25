//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// outboxEDUs - EDU типа eduType в outbox для destination (content).
func outboxEDUs(t *testing.T, server *Server, destination, eduType string) []map[string]any {
	t.Helper()

	rows, err := server.db.Pool().Query(context.Background(), `
		SELECT edu_json->'content' FROM federation_outbox
		WHERE destination = $1 AND edu_json->>'edu_type' = $2 ORDER BY id`, destination, eduType)
	if err != nil {
		t.Fatalf("outbox edus: %v", err)
	}

	defer rows.Close()

	var out []map[string]any

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}

		var content map[string]any
		_ = json.Unmarshal(raw, &content)
		out = append(out, content)
	}

	return out
}

// TestFederationOutgoingEDUs - typing, receipt, presence, ключи устройства и
// кросс-подписи нашего пользователя уходят EDU серверу участника общей комнаты.
// Раньше исходящих EDU не было: Synapse не узнавал о наших
// устройствах и не мог зашифровать для них сообщение.
func TestFederationOutgoingEDUs(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := fedRoomWithRemote(t, env)
	server := env.server
	eventID := sendText(t, server, alice, roomID, "t1", "hi")

	rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/typing/@alice:test.local",
		`{"typing":true,"timeout":30000}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("typing: %d %s", rec.Code, rec.Body.String())
	}

	if edus := outboxEDUs(t, server, "remote.test", "m.typing"); len(edus) != 1 ||
		edus[0]["room_id"] != roomID || edus[0]["user_id"] != "@alice:test.local" || edus[0]["typing"] != true {
		t.Fatalf("m.typing: %+v", edus)
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read/"+eventID, `{}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt: %d %s", rec.Code, rec.Body.String())
	}

	// Приватная отметка по федерации не уходит.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read.private/"+eventID, `{}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("private receipt: %d", rec.Code)
	}

	receipts := outboxEDUs(t, server, "remote.test", "m.receipt")
	if len(receipts) != 1 {
		t.Fatalf("m.receipt: %+v", receipts)
	}

	read, _ := receipts[0][roomID].(map[string]any)["m.read"].(map[string]any)
	if _, ok := read["@alice:test.local"]; !ok {
		t.Fatalf("m.receipt content: %+v", receipts[0])
	}

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/presence/@alice:test.local/status",
		`{"presence":"unavailable","status_msg":"away"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("presence: %d", rec.Code)
	}

	// Presence ставится в фоне.
	deadline := time.Now().Add(5 * time.Second)

	var presence []map[string]any
	for time.Now().Before(deadline) {
		if presence = outboxEDUs(t, server, "remote.test", "m.presence"); len(presence) > 0 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	if len(presence) != 1 {
		t.Fatalf("m.presence: %+v", presence)
	}

	push, _ := presence[0]["push"].([]any)
	if len(push) != 1 || push[0].(map[string]any)["presence"] != "unavailable" {
		t.Fatalf("m.presence push: %+v", presence[0])
	}

	upload := `{"device_keys":{"user_id":"@alice:test.local","device_id":"ALICEDEV",
		"algorithms":["m.olm.v1.curve25519-aes-sha2"],"keys":{"curve25519:ALICEDEV":"c1","ed25519:ALICEDEV":"e1"}}}`
	for range 2 {
		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload", upload, alice); rec.Code != http.StatusOK {
			t.Fatalf("keys/upload: %d %s", rec.Code, rec.Body.String())
		}
	}

	updates := outboxEDUs(t, server, "remote.test", "m.device_list_update")
	if len(updates) != 2 {
		t.Fatalf("m.device_list_update: %+v", updates)
	}

	first, second := updates[0], updates[1]
	if first["device_id"] != "ALICEDEV" || first["keys"] == nil || len(first["prev_id"].([]any)) != 0 {
		t.Fatalf("первое изменение: %+v", first)
	}

	if prev := second["prev_id"].([]any); len(prev) != 1 || prev[0] != first["stream_id"] {
		t.Fatalf("prev_id второго изменения %v, ожидали [%v]", second["prev_id"], first["stream_id"])
	}

	// /user/devices отдает тот же stream_id.
	rec = env.request(t, http.MethodGet, "/_matrix/federation/v1/user/devices/@alice:test.local", nil)

	var devices userDevicesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &devices)

	if float64(devices.StreamID) != second["stream_id"] {
		t.Fatalf("stream_id /user/devices %d, ожидали %v", devices.StreamID, second["stream_id"])
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload", `{"master_key":{
		"user_id":"@alice:test.local","usage":["master"],"keys":{"ed25519:MPUB":"MPUB"}}}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("device_signing/upload: %d %s", rec.Code, rec.Body.String())
	}

	if edus := outboxEDUs(t, server, "remote.test", "m.signing_key_update"); len(edus) != 1 || edus[0]["master_key"] == nil {
		t.Fatalf("m.signing_key_update: %+v", edus)
	}

	// Удаление устройства (logout) - deleted=true.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/logout", `{}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}

	updates = outboxEDUs(t, server, "remote.test", "m.device_list_update")
	if last := updates[len(updates)-1]; last["deleted"] != true || last["device_id"] != "ALICEDEV" {
		t.Fatalf("удаление устройства: %+v", last)
	}
}
