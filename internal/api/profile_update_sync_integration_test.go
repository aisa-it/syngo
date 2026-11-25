//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestProfileUpdateAffectsMemberEvents — Complement TestDisplayNameUpdate /
// TestAvatarUrlUpdate: после PUT профиля в /sync приходит обновлённое
// m.room.member-событие с новым полем.
func TestProfileUpdateAffectsMemberEvents(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat"}`, alice)
	if rec.Code != 200 {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode createRoom: %v %s", err, rec.Body.String())
	}

	base := syncWithSince(t, server, alice, "")

	// Complement гоняет один и тот же сценарий для displayname и avatar_url.
	for _, tc := range []struct{ field, value string }{
		{"displayname", "LemurLover"},
		{"avatar_url", "mxc://example.com/LemurAvatar"},
	} {
		checkProfilePropagation(t, server, alice, created.RoomID, base.NextBatch, tc.field, tc.value)
	}
}

// checkProfilePropagation меняет поле профиля и ждёт m.room.member с новым
// значением в /sync.
func checkProfilePropagation(t *testing.T, server *Server, token, roomID, since, field, value string) {
	t.Helper()

	if rec := do(t, server, http.MethodPut,
		"/_matrix/client/v3/profile/%40alice%3Atest.local/"+field,
		`{"`+field+`":"`+value+`"}`, token); rec.Code != 200 {
		t.Fatalf("put %s: %d %s", field, rec.Code, rec.Body.String())
	}

	// Рассылка профиля идёт в фоне (propagateProfileAsync), поэтому опрашиваем
	// /sync до 5 секунд — как MustSyncUntil в Complement.
	deadline := time.Now().Add(5 * time.Second)

	for {
		after := syncWithSince(t, server, token, since)

		if room, ok := after.Rooms.Join[roomID]; ok {
			for _, ev := range room.Timeline.Events {
				if ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == "@alice:test.local" {
					var content map[string]json.RawMessage
					if err := json.Unmarshal(ev.Content, &content); err != nil {
						t.Fatal(err)
					}
					var got string
					if json.Unmarshal(content[field], &got) == nil && got == value {
						return
					}
				}
			}
		}

		since = after.NextBatch

		if time.Now().After(deadline) {
			t.Fatalf("за 5 с в /sync не пришло m.room.member с %s=%q", field, value)
		}

		time.Sleep(100 * time.Millisecond)
	}
}
