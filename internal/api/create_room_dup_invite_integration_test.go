//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestCreateRoomDuplicateInvite - повтор пользователя в invite дает одно
// приглашение, а не 500 (найдено генератором нагрузки).
func TestCreateRoomDuplicateInvite(t *testing.T) {
	srv := newIntegrationServer(t)

	alice := registerUser(t, srv, "dupalice", "dupalice-password")
	registerUser(t, srv, "dupbob", "dupbob-password")
	bob := "@dupbob:test.local"

	rec := do(t, srv, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"private_chat","invite":["`+bob+`","`+bob+`"]}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom с повтором в invite: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("room_id: %v %s", err, rec.Body.String())
	}

	rec = do(t, srv, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(created.RoomID)+
		"/state/m.room.member/"+url.PathEscape(bob), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("членство bob: %d %s", rec.Code, rec.Body.String())
	}

	var member struct {
		Membership string `json:"membership"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &member); err != nil || member.Membership != "invite" {
		t.Fatalf("членство bob = %q (%v)", member.Membership, err)
	}
}
