package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestRoomHierarchy — узел пространства и его дети по m.space.child.
func TestRoomHierarchy(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	spaceID := createRoomWith(t, server, alice.AccessToken,
		`{"name":"Space","creation_content":{"type":"m.space"}}`)
	childID := createRoomWith(t, server, alice.AccessToken, `{"name":"Child"}`)

	// m.space.child: state_key = дочерняя комната, тело PUT - content события
	// (via на верхнем уровне; раньше тест слал событие целиком, и без via
	// ребенок все равно засчитывался).
	state := `{"via":["test.local"]}`

	rec := do(t, server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.space.child/%s", spaceID, childID), state, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("state child: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/hierarchy", spaceID), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("hierarchy: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp hierarchyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Rooms) != 2 {
		t.Fatalf("комнат в ответе %d, ожидали 2 (узел + ребёнок): %s", len(resp.Rooms), rec.Body.String())
	}

	if resp.Rooms[0].RoomID != spaceID || resp.Rooms[0].Name != "Space" {
		t.Fatalf("узел = %+v", resp.Rooms[0])
	}

	if resp.Rooms[0].RoomType != "m.space" {
		t.Fatalf("room_type = %q, ожидали m.space", resp.Rooms[0].RoomType)
	}

	if len(resp.Rooms[0].ChildrenState) != 1 {
		t.Fatalf("children_state = %d, ожидали 1", len(resp.Rooms[0].ChildrenState))
	}

	if resp.Rooms[1].RoomID != childID || resp.Rooms[1].Name != "Child" {
		t.Fatalf("ребёнок = %+v", resp.Rooms[1])
	}
}

// TestRoomHierarchyMaxDepthZero — max_depth=0 отдаёт только сам узел.
func TestRoomHierarchyMaxDepthZero(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	spaceID := createRoomWith(t, server, alice.AccessToken, `{"name":"S"}`)

	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/hierarchy?max_depth=0", spaceID), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp hierarchyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Rooms) != 1 {
		t.Fatalf("max_depth=0 вернул %d комнат", len(resp.Rooms))
	}
}

// TestRoomHierarchyRequiresMembership — посторонний получает отказ.
func TestRoomHierarchyRequiresMembership(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")
	spaceID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/hierarchy", spaceID), "", bob.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, ожидали 403", rec.Code)
	}
}
