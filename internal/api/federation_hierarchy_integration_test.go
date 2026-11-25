//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestFederationHierarchyStore - федеративный /hierarchy на
// состоянии из PostgreSQL: публичное пространство отдает себя, m.space.child и
// сводку ребенка; закрытая комната без участников запросившего сервера - 404.
func TestFederationHierarchyStore(t *testing.T) {
	env := newFedEnv(t)
	tok := registerHTTP(t, env.server, "alice", "DEV")

	create := func(body string) string {
		rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/createRoom", body, tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
		}

		roomID, _ := decodeQueryBody(t, rec)["room_id"].(string)

		return roomID
	}

	space := create(`{"name":"Space","preset":"public_chat","creation_content":{"type":"m.space"}}`)
	child := create(`{"name":"Child","preset":"public_chat"}`)
	private := create(`{"name":"Private"}`)

	for _, id := range []string{child, private} {
		rec := do(t, env.server, http.MethodPut,
			fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.space.child/%s", space, id), `{"via":["test.local"]}`, tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("m.space.child: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/hierarchy/"+space, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("hierarchy: %d %s", rec.Code, rec.Body.String())
	}

	var resp federationHierarchyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Room.RoomID != space || resp.Room.Name != "Space" || resp.Room.RoomType != "m.space" ||
		resp.Room.NumJoinedMembers != 1 || resp.Room.JoinRule != "public" || len(resp.Room.ChildrenState) != 2 {
		t.Fatalf("room: %s", rec.Body.String())
	}

	if len(resp.Children) != 1 || resp.Children[0].RoomID != child || resp.Children[0].Name != "Child" ||
		len(resp.InaccessibleChildren) != 1 || resp.InaccessibleChildren[0] != private {
		t.Fatalf("children: %s", rec.Body.String())
	}

	expectStatus(t, "закрытая", env.request(t, http.MethodGet, "/_matrix/federation/v1/hierarchy/"+private, nil),
		http.StatusNotFound, "M_NOT_FOUND")
}
