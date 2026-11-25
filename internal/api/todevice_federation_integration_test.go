//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestSendToDeviceRemoteUser - to-device пользователю другого сервера уходит
// EDU m.direct_to_device его серверу через outbox, а не в локальную очередь.
// Повтор txn_id второй EDU не ставит; локальный получатель того
// же запроса получает сообщение как раньше.
func TestSendToDeviceRemoteUser(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	body := `{"messages":{
		"@carol:remote.test":{"CDEV":{"ciphertext":"to-remote"}},
		"@bob:test.local":{"BOB":{"ciphertext":"to-local"}}}}`

	for range 2 {
		rec := do(t, server, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room.encrypted/txn-r1", body, alice)
		if rec.Code != http.StatusOK {
			t.Fatalf("sendToDevice: %d %s", rec.Code, rec.Body.String())
		}
	}

	ctx := context.Background()

	var local int
	if err := server.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM device_inbox WHERE user_id = '@carol:remote.test'`).Scan(&local); err != nil {
		t.Fatalf("device_inbox: %v", err)
	}

	if local != 0 {
		t.Fatalf("сообщение удаленному легло в локальную очередь (%d)", local)
	}

	rows, err := server.db.Pool().Query(ctx,
		`SELECT destination, edu_json::text FROM federation_outbox WHERE edu_json IS NOT NULL`)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}

	defer rows.Close()

	var edus []string

	for rows.Next() {
		var destination, edu string
		if err := rows.Scan(&destination, &edu); err != nil {
			t.Fatalf("scan: %v", err)
		}

		if destination != "remote.test" {
			t.Fatalf("destination %s", destination)
		}

		edus = append(edus, edu)
	}

	if len(edus) != 1 {
		t.Fatalf("EDU в outbox: %d, ожидали 1", len(edus))
	}

	var edu struct {
		Type    string `json:"edu_type"`
		Content struct {
			Sender    string                                `json:"sender"`
			Type      string                                `json:"type"`
			MessageID string                                `json:"message_id"`
			Messages  map[string]map[string]json.RawMessage `json:"messages"`
		} `json:"content"`
	}

	_ = json.Unmarshal([]byte(edus[0]), &edu)

	if edu.Type != "m.direct_to_device" || edu.Content.Sender != "@alice:test.local" ||
		edu.Content.Type != "m.room.encrypted" || edu.Content.MessageID == "" ||
		edu.Content.Messages["@carol:remote.test"]["CDEV"] == nil {
		t.Fatalf("EDU: %s", edus[0])
	}

	if resp := syncOnce(t, server, bob); len(resp.ToDevice.Events) != 1 {
		t.Fatalf("локальный получатель: %d событий", len(resp.ToDevice.Events))
	}
}
