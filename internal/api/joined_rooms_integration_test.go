//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestJoinedRoomsListsMemberships - joined_rooms отдает комнаты, где пользователь
// в join, и не отдает покинутые (раньше маршрут проверялся только на «не 404»).
func TestJoinedRoomsListsMemberships(t *testing.T) {
	server := newIntegrationServer(t)
	token := registerHTTP(t, server, "joinedlist", "JDEV")

	create := func() string {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{}`, token)

		var resp struct {
			RoomID string `json:"room_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); rec.Code != http.StatusOK || err != nil || resp.RoomID == "" {
			t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
		}

		return resp.RoomID
	}

	kept, left := create(), create()

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+left+"/leave", `{}`, token); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/joined_rooms", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("joined_rooms: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("разбор joined_rooms: %v", err)
	}

	if !slices.Contains(resp.JoinedRooms, kept) || slices.Contains(resp.JoinedRooms, left) {
		t.Fatalf("joined_rooms = %v, ожидали %s без покинутой %s", resp.JoinedRooms, kept, left)
	}
}
