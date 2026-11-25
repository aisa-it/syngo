package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRoomV12HTTP - Element создает комнату v12 (room_id без домена) и
// работает с ней через /sync, /messages и /state; capabilities объявляют v12.
// Раньше createRoom с room_version 12 отвечал M_UNSUPPORTED_ROOM_VERSION.
func TestRoomV12HTTP(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	caps := do(t, server, http.MethodGet, "/_matrix/client/v3/capabilities", "", alice.AccessToken)
	if !strings.Contains(caps.Body.String(), `"12":"stable"`) {
		t.Fatalf("capabilities без v12: %s", caps.Body.String())
	}

	roomID := createRoomWith(t, server, alice.AccessToken, `{"room_version":"12","name":"v12"}`)
	if strings.Contains(roomID, ":") {
		t.Fatalf("room_id v12 с доменом: %s", roomID)
	}

	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID)
	sendMessage(t, server, alice.AccessToken, roomID, "t1", `{"msgtype":"m.text","body":"привет v12"}`)

	resp := decodeSync(t, do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken).Body.Bytes())
	if _, ok := resp.Rooms.Join[roomID]; !ok {
		t.Fatalf("/sync без комнаты v12 %s", roomID)
	}

	messages := do(t, server, http.MethodGet, path+"/messages?dir=b&limit=10", "", alice.AccessToken)
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), "привет v12") {
		t.Fatalf("/messages: %d %s", messages.Code, messages.Body.String())
	}

	create := do(t, server, http.MethodGet, path+"/state/m.room.create", "", alice.AccessToken)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), `"room_version":"12"`) {
		t.Fatalf("/state/m.room.create: %d %s", create.Code, create.Body.String())
	}

	// Создатель вне power_levels меняет название (нужно 50).
	name := do(t, server, http.MethodPut, path+"/state/m.room.name", `{"name":"новое"}`, alice.AccessToken)
	if name.Code != http.StatusOK {
		t.Fatalf("название от создателя: %d %s", name.Code, name.Body.String())
	}
}
