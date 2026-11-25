//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestUnsignedTransactionID (Complement TestTxnInEvent,
// TestTxnScopeOnLocalEcho): свое событие несет unsigned.transaction_id в /sync,
// /event и /messages только для устройства-отправителя.
func TestUnsignedTransactionID(t *testing.T) {
	server := newIntegrationServer(t)
	first := registerHTTP(t, server, "txnuser", "FIRST")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","user":"txnuser","password":"1234567890","device_id":"SECOND"}`, "")

	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || login.AccessToken == "" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}

	second := login.AccessToken

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{}`, first)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/abcdefg",
		`{"msgtype":"m.text","body":"first"}`, first)

	var sent struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil || sent.EventID == "" {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	type unsignedView struct {
		EventID  string `json:"event_id"`
		Unsigned struct {
			TransactionID *string `json:"transaction_id"`
		} `json:"unsigned"`
	}

	txnOf := func(events []unsignedView) (string, bool) {
		for _, ev := range events {
			if ev.EventID == sent.EventID {
				if ev.Unsigned.TransactionID == nil {
					return "", true
				}

				return *ev.Unsigned.TransactionID, true
			}
		}

		return "", false
	}

	syncTxn := func(token string) string {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", token)

		var resp struct {
			Rooms struct {
				Join map[string]struct {
					Timeline struct {
						Events []unsignedView `json:"events"`
					} `json:"timeline"`
				} `json:"join"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v", err)
		}

		txn, found := txnOf(resp.Rooms.Join[created.RoomID].Timeline.Events)
		if !found {
			t.Fatalf("событие не пришло в sync: %s", rec.Body.String())
		}

		return txn
	}

	if got := syncTxn(first); got != "abcdefg" {
		t.Fatalf("sync отправителя: transaction_id %q", got)
	}

	if got := syncTxn(second); got != "" {
		t.Fatalf("sync другого устройства: transaction_id %q, ожидали отсутствие", got)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+created.RoomID+"/event/"+sent.EventID, "", first)

	var event unsignedView
	if err := json.Unmarshal(rec.Body.Bytes(), &event); err != nil || event.Unsigned.TransactionID == nil ||
		*event.Unsigned.TransactionID != "abcdefg" {
		t.Fatalf("GET /event: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+created.RoomID+"/messages?dir=b&limit=5", "", first)

	var messages struct {
		Chunk []unsignedView `json:"chunk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &messages); err != nil {
		t.Fatalf("messages: %v", err)
	}

	if got, _ := txnOf(messages.Chunk); got != "abcdefg" {
		t.Fatalf("/messages: transaction_id %q", got)
	}
}
