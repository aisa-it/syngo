//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestReceiptsSurviveRestart — отметки прочтения сохраняются в БД и видны после
// перезапуска процесса (новый Server на той же БД).
func TestReceiptsSurviveRestart(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "rpalice", "ADEV")
	bob := registerHTTP(t, server, "rpbob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	room := url.PathEscape(created.RoomID)

	do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob)

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/send/m.room.message/m1", `{"msgtype":"m.text","body":"hi"}`, alice)

	var sent struct {
		EventID string `json:"event_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &sent)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/receipt/m.read/"+url.PathEscape(sent.EventID),
		`{"thread_id":"main"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("receipt: %d %s", rec.Code, rec.Body.String())
	}

	restarted := New(server.cfg, server.db, server.log)

	rec = do(t, restarted, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", alice)
	if !strings.Contains(rec.Body.String(), `"m.read":{"@rpbob:test.local":{"thread_id":"main"`) {
		t.Fatalf("после рестарта нет отметки bob: %s", rec.Body.String())
	}
}
