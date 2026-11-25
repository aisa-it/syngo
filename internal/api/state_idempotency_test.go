package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestStateAndModerationHTTPNotDeduplicated — PUT /state и kick больше не
// дедуплицируются по телу запроса: обработчик строил txn_id из типа,
// ключа и хеша content, и возврат к прежнему значению или повторный kick молча
// отвечали 200 без нового события.
func TestStateAndModerationHTTPNotDeduplicated(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"preset":"public_chat"}`)
	room := url.PathEscape(roomID)

	var lastID string

	for _, topic := range []string{"A", "B", "A"} {
		rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/state/m.room.topic/",
			`{"topic":"`+topic+`"}`, alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT topic %s: %d %s", topic, rec.Code, rec.Body.String())
		}

		var resp struct {
			EventID string `json:"event_id"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		if resp.EventID == lastID {
			t.Fatalf("PUT topic %s вернул прежний event_id %s", topic, resp.EventID)
		}

		lastID = resp.EventID
	}

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/state/m.room.topic/", "", alice.AccessToken)
	if rec.Body.String() != "{\"topic\":\"A\"}\n" {
		t.Fatalf("topic после A -> B -> A: %s", rec.Body.String())
	}

	for round := 0; round < 2; round++ {
		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob.AccessToken); rec.Code != http.StatusOK {
			t.Fatalf("раунд %d join: %d %s", round, rec.Code, rec.Body.String())
		}

		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/kick",
			`{"user_id":"`+bob.UserID+`","reason":"тест"}`, alice.AccessToken); rec.Code != http.StatusOK {
			t.Fatalf("раунд %d kick: %d %s", round, rec.Code, rec.Body.String())
		}

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/state/m.room.member/"+url.PathEscape(bob.UserID), "", alice.AccessToken)
		if rec.Body.String() == "" || !json.Valid(rec.Body.Bytes()) || !containsMembership(rec.Body.Bytes(), "leave") {
			t.Fatalf("раунд %d: после kick membership не leave: %s", round, rec.Body.String())
		}
	}
}

func containsMembership(raw []byte, want string) bool {
	var body struct {
		Membership string `json:"membership"`
	}

	return json.Unmarshal(raw, &body) == nil && body.Membership == want
}
