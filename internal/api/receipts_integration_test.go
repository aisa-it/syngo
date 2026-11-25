//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestReceiptsPrivacyFullyReadTyping - через HTTP: m.read.private
// виден только владельцу, m.fully_read попадает в room account data, неизвестный
// тип receipt - 400, typing=false приходит собеседнику пустым m.typing.
func TestReceiptsPrivacyFullyReadTyping(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "rcalice", "ADEV")
	bob := registerHTTP(t, server, "rcbob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	room := url.PathEscape(created.RoomID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/send/m.room.message/m1", `{"msgtype":"m.text","body":"hi"}`, alice)

	var sent struct {
		EventID string `json:"event_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &sent)
	ev := url.PathEscape(sent.EventID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/receipt/m.read.private/"+ev, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("m.read.private: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/read_markers",
		`{"m.fully_read":"`+sent.EventID+`"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("read_markers: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/receipt/m.bogus/"+ev, `{}`, bob); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестный тип: %d, ожидали 400", rec.Code)
	}

	roomOf := func(tok string) string {
		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", tok)

		var resp struct {
			Rooms struct {
				Join map[string]json.RawMessage `json:"join"`
			} `json:"rooms"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return string(resp.Rooms.Join[created.RoomID])
	}

	if got := roomOf(alice); strings.Contains(got, "m.read.private") || strings.Contains(got, `"m.fully_read"`) {
		t.Fatalf("alice видит приватные отметки bob: %s", got)
	}

	bobRoom := roomOf(bob)
	if !strings.Contains(bobRoom, "m.read.private") || !strings.Contains(bobRoom, `"account_data":{"events":[{"type":"m.fully_read","content":{"event_id":"`) {
		t.Fatalf("bob не видит свои отметки: %s", bobRoom)
	}

	// Публичная отметка видна всем, приватная ее не затерла.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/receipt/m.read/"+ev, `{"thread_id":"main"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("m.read: %d", rec.Code)
	}

	if got := roomOf(alice); !strings.Contains(got, `"m.read":{"@rcbob:test.local":{"thread_id":"main"`) {
		t.Fatalf("alice не видит m.read bob: %s", got)
	}

	if !strings.Contains(roomOf(bob), "m.read.private") {
		t.Fatal("m.read затер m.read.private")
	}

	// typing=false: собеседник получает пустой m.typing.
	path := "/_matrix/client/v3/rooms/" + room + "/typing/" + url.PathEscape("@rcbob:test.local")
	do(t, server, http.MethodPut, path, `{"typing":true,"timeout":30000}`, bob)
	do(t, server, http.MethodPut, path, `{"typing":false}`, bob)

	if got := roomOf(alice); !strings.Contains(got, `{"type":"m.typing","content":{"user_ids":[]}}`) {
		t.Fatalf("после typing=false нет пустого m.typing: %s", got)
	}
}
