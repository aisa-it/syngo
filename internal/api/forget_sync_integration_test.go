//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestForgottenRoomNotInInitialSync (Complement TestRoomForget):
// забытая комната не попадает в initial sync даже с include_leave, а повторный
// вход снимает отметку «забыто».
func TestForgottenRoomNotInInitialSync(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "fgalice", "ADEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	roomPath := "/_matrix/client/v3/rooms/" + created.RoomID

	if rec := do(t, server, http.MethodPost, roomPath+"/leave", `{}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, roomPath+"/forget", `{}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", rec.Code, rec.Body.String())
	}

	sections := func() (join, leave bool) {
		t.Helper()

		rec := do(t, server, http.MethodGet,
			`/_matrix/client/v3/sync?timeout=0&filter={"room":{"include_leave":true}}`, "", alice)

		var resp struct {
			Rooms struct {
				Join  map[string]json.RawMessage `json:"join"`
				Leave map[string]json.RawMessage `json:"leave"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		_, join = resp.Rooms.Join[created.RoomID]
		_, leave = resp.Rooms.Leave[created.RoomID]

		return join, leave
	}

	if join, leave := sections(); join || leave {
		t.Fatalf("забытая комната в initial sync: join=%v leave=%v", join, leave)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("rejoin: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, roomPath+"/leave", `{}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("leave again: %d %s", rec.Code, rec.Body.String())
	}

	if _, leave := sections(); !leave {
		t.Fatal("после повторного входа и выхода комната должна быть в leave")
	}
}
