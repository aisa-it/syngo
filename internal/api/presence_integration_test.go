//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// presenceOf - presence пользователя из ответа /sync и next_batch.
func presenceOf(t *testing.T, server *Server, token, since, query, userID string) (presence string, found bool, next string) {
	t.Helper()

	path := "/_matrix/client/v3/sync?timeout=0" + query
	if since != "" {
		path += "&since=" + since
	}

	rec := do(t, server, http.MethodGet, path, "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		NextBatch string `json:"next_batch"`
		Presence  struct {
			Events []struct {
				Type    string `json:"type"`
				Sender  string `json:"sender"`
				Content struct {
					Presence string `json:"presence"`
				} `json:"content"`
			} `json:"events"`
		} `json:"presence"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("sync decode: %v", err)
	}

	for _, ev := range resp.Presence.Events {
		if ev.Type != "m.presence" || ev.Sender == "" {
			t.Fatalf("presence без type/sender: %s", rec.Body.String())
		}

		if ev.Sender == userID {
			presence, found = ev.Content.Presence, true
		}
	}

	return presence, found, resp.NextBatch
}

// TestSyncPresence (Complement TestPresence, TestMembersLocal,
// TestSync presence): изменения presence приходят участникам общих комнат с
// sender, set_presence из /sync меняет статус, новый участник комнаты видит
// статус остальных, а посторонний - нет.
func TestSyncPresence(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "pralice", "ADEV")
	bob := registerHTTP(t, server, "prbob", "BDEV")
	eve := registerHTTP(t, server, "preve", "EDEV")

	const aliceID = "@pralice:test.local"

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// alice онлайн (синк без set_presence), bob еще не в комнате.
	presenceOf(t, server, alice, "", "", aliceID)
	_, _, bobSince := presenceOf(t, server, bob, "", "", aliceID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	// Вошедший в комнату видит текущий статус участников.
	got, found, bobSince := presenceOf(t, server, bob, bobSince, "", aliceID)
	if !found || got != "online" {
		t.Fatalf("presence alice после join: %q %v", got, found)
	}

	// set_presence=unavailable из /sync alice доходит до bob.
	presenceOf(t, server, alice, "", "&set_presence=unavailable", aliceID)

	got, found, bobSince = presenceOf(t, server, bob, bobSince, "", aliceID)
	if !found || got != "unavailable" {
		t.Fatalf("presence alice после set_presence: %q %v", got, found)
	}

	// Повторный синк без изменений presence alice не несет.
	if _, found, _ = presenceOf(t, server, bob, bobSince, "", aliceID); found {
		t.Fatal("presence без изменений пришел повторно")
	}

	// PUT /presence тоже доходит; посторонний статус не видит.
	_, _, eveSince := presenceOf(t, server, eve, "", "", aliceID)

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/presence/"+aliceID+"/status",
		`{"presence":"online","status_msg":"тут"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("PUT presence: %d %s", rec.Code, rec.Body.String())
	}

	if got, found, _ = presenceOf(t, server, bob, bobSince, "", aliceID); !found || got != "online" {
		t.Fatalf("presence alice после PUT: %q %v", got, found)
	}

	if _, found, _ = presenceOf(t, server, eve, eveSince, "", aliceID); found {
		t.Fatal("посторонний видит presence alice")
	}

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/presence/"+aliceID+"/status",
		`{"presence":"busy"}`, alice); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT невалидного presence: %d", rec.Code)
	}
}

// TestPresenceWakesOnlyRoomMates (Complement TestPushSync): смена
// presence не отвечает досрочно на long-poll тех, с кем нет общей комнаты, и
// будит участника общей комнаты.
func TestPresenceWakesOnlyRoomMates(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "pwalice", "ADEV")
	bob := registerHTTP(t, server, "pwbob", "BDEV")
	eve := registerHTTP(t, server, "pweve", "EDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	presenceOf(t, server, bob, "", "", "")
	_, _, aliceSince := presenceOf(t, server, alice, "", "", "")
	_, _, eveSince := presenceOf(t, server, eve, "", "", "")

	longPoll := func(token, since string) (time.Duration, string) {
		started := time.Now()
		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=1500&since="+since, "", token)

		return time.Since(started), rec.Body.String()
	}

	type result struct {
		took time.Duration
		body string
	}

	aliceDone := make(chan result, 1)
	eveDone := make(chan result, 1)

	go func() { took, body := longPoll(alice, aliceSince); aliceDone <- result{took, body} }()
	go func() { took, body := longPoll(eve, eveSince); eveDone <- result{took, body} }()

	time.Sleep(300 * time.Millisecond)

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/presence/@pwbob:test.local/status",
		`{"presence":"unavailable"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("PUT presence: %d", rec.Code)
	}

	got := <-aliceDone
	if got.took >= 1400*time.Millisecond || !strings.Contains(got.body, `"unavailable"`) {
		t.Fatalf("alice: %s, %s", got.took, got.body)
	}

	if got = <-eveDone; got.took < 1400*time.Millisecond {
		t.Fatalf("eve разбужена чужим presence через %s: %s", got.took, got.body)
	}
}
