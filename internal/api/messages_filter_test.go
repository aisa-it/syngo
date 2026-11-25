package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestMessagesFilter - /messages применяет RoomEventFilter (contains_url, types)
// и с lazy_load_members отдает state с m.room.member отправителей
// (Complement TestRoomImageRoundtrip, TestRoomMessagesLazyLoading).
func TestMessagesFilter(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	for txn, body := range map[string]string{
		"t1": `{"msgtype":"m.text","body":"plain"}`,
		"t2": `{"msgtype":"m.file","body":"test.png","url":"mxc://test.local/abc"}`,
	} {
		if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/"+txn,
			body, alice.AccessToken); rec.Code != http.StatusOK {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
	}

	type response struct {
		Chunk []eventView `json:"chunk"`
		State []eventView `json:"state"`
	}

	messages := func(filter string) response {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/messages?dir=b&limit=50&filter="+
			url.QueryEscape(filter), "", alice.AccessToken)

		var resp response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("messages %s: %d %s", filter, rec.Code, rec.Body.String())
		}

		return resp
	}

	if resp := messages(`{"contains_url":true}`); len(resp.Chunk) != 1 || resp.Chunk[0].Type != "m.room.message" {
		t.Fatalf("contains_url: %+v", resp.Chunk)
	}

	if resp := messages(`{"types":["m.room.message"]}`); len(resp.Chunk) != 2 {
		t.Fatalf("types: %d событий", len(resp.Chunk))
	}

	// MSC3874: фильтр по rel_type.
	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/t3",
		`{"msgtype":"m.text","body":"in thread","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`,
		alice.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("send thread: %d %s", rec.Code, rec.Body.String())
	}

	if resp := messages(`{"org.matrix.msc3874.rel_types":["m.thread"]}`); len(resp.Chunk) != 1 {
		t.Fatalf("rel_types: %d событий", len(resp.Chunk))
	}

	if resp := messages(`{"types":["m.room.message"],"org.matrix.msc3874.not_rel_types":["m.thread"]}`); len(resp.Chunk) != 2 {
		t.Fatalf("not_rel_types: %d событий", len(resp.Chunk))
	}

	resp := messages(`{"lazy_load_members":true,"types":["m.room.message"]}`)
	if len(resp.State) != 1 || resp.State[0].Type != "m.room.member" || *resp.State[0].StateKey != alice.UserID {
		t.Fatalf("lazy_load_members: state %+v", resp.State)
	}
}
