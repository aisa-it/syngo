//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMemberEventContent (Complement TestRoomState, TestRoomMembers):
// displayname по умолчанию - localpart, m.room.member при создании комнаты и join
// несет профиль участника, поля тела /join попадают в content, joined_members
// отдает display_name.
func TestMemberEventContent(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "mcalice", "ADEV")
	bob := registerHTTP(t, server, "mcbob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec = do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID,
		`{"foo":"bar","reason":"hello","membership":"leave"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	member := func(userID string) map[string]any {
		t.Helper()

		rec := do(t, server, http.MethodGet,
			"/_matrix/client/v3/rooms/"+created.RoomID+"/state/m.room.member/"+userID, "", alice)
		if rec.Code != http.StatusOK {
			t.Fatalf("state member %s: %d %s", userID, rec.Code, rec.Body.String())
		}

		var content map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &content)

		return content
	}

	if content := member("@mcalice:test.local"); content["displayname"] != "mcalice" {
		t.Fatalf("member создателя: %v", content)
	}

	content := member("@mcbob:test.local")
	if content["displayname"] != "mcbob" || content["foo"] != "bar" || content["reason"] != "hello" ||
		content["membership"] != "join" {
		t.Fatalf("member после join: %v", content)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+created.RoomID+"/joined_members", "", alice)

	var joined struct {
		Joined map[string]struct {
			DisplayName string `json:"display_name"`
		} `json:"joined"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &joined); err != nil || joined.Joined["@mcbob:test.local"].DisplayName != "mcbob" {
		t.Fatalf("joined_members: %d %s", rec.Code, rec.Body.String())
	}
}
