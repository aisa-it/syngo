package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestCreateRoomWithAliasName (Complement TestRoomCanonicalAlias):
// room_alias_name создаёт алиас вместе с комнатой и ставит canonical_alias.
func TestCreateRoomWithAliasName(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"room_alias_name":"my-alias","name":"alias room"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID    string `json:"room_id"`
		RoomAlias string `json:"room_alias"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode createRoom: %v %s", err, rec.Body.String())
	}

	if created.RoomAlias != "#my-alias:test.local" {
		t.Fatalf("room_alias = %q", created.RoomAlias)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/directory/room/%23my-alias%3Atest.local", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("directory resolve: %d %s", rec.Code, rec.Body.String())
	}

	var resolved struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil || resolved.RoomID != created.RoomID {
		t.Fatalf("resolved room_id = %q, ожидали %q", resolved.RoomID, created.RoomID)
	}

	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/state/m.room.canonical_alias", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("canonical_alias state: %d %s", rec.Code, rec.Body.String())
	}

	var canonical struct {
		Alias string `json:"alias"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &canonical); err != nil || canonical.Alias != "#my-alias:test.local" {
		t.Fatalf("canonical_alias = %q", canonical.Alias)
	}
}

// TestCanonicalAliasBadAlias - несуществующий алиас в m.room.canonical_alias дает
// 400 M_BAD_ALIAS, синтаксически неверный - 400 M_INVALID_PARAM
// (Complement TestRoomCanonicalAlias).
func TestCanonicalAliasBadAlias(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for body, want := range map[string]string{
		`{"alias":"#missing:test.local"}`: "M_BAD_ALIAS",
		`{"alias":"not-an-alias"}`:        ErrCodeInvalidParam,
	} {
		rec = do(t, server, http.MethodPut,
			"/_matrix/client/v3/rooms/"+created.RoomID+"/state/m.room.canonical_alias", body, alice.AccessToken)
		if rec.Code != http.StatusBadRequest || decodeMatrixError(t, rec).Code != want {
			t.Fatalf("%s: %d %s, ожидали 400 %s", body, rec.Code, rec.Body.String(), want)
		}
	}
}
