package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestDecodePathParam — percent-encoded state_key и eventType.
func TestDecodePathParam(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"@alice%3Atest.local": "@alice:test.local",
		"m.room.member":       "m.room.member",
		"a%20b":               "a b",
		"bad%zz":              "bad%zz",
	}

	for raw, want := range cases {
		if got := decodePathParam(raw); got != want {
			t.Fatalf("decodePathParam(%q) = %q, ожидали %q", raw, got, want)
		}
	}

	// Маршрут с encoded state_key доходит до обработчика и отдает membership
	// пользователя (раньше тест проверял только «не 404»).
	srv := newRoomsServer(t)
	token := registerUser(t, srv, "alice", "1234567890").AccessToken
	roomID := createRoomWith(t, srv, token, `{}`)

	rec := do(t, srv, http.MethodGet,
		"/_matrix/client/v3/rooms/"+roomID+"/state/m.room.member/@alice%3Atest.local", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("state с encoded state_key: %d %s", rec.Code, rec.Body.String())
	}

	var member struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &member); err != nil || member.Membership != "join" {
		t.Fatalf("state с encoded state_key: %s (%v), ожидали membership join", rec.Body.String(), err)
	}
}
