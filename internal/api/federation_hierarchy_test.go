package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// hierarchyEnv - сервер на памяти с ключом федерации и известным ключом
// remote.test.
type hierarchyEnv struct {
	server    *Server
	store     *coretest.MemRoomStore
	remoteKey signing.Key
	token     string
	seq       int
}

func newHierarchyEnv(t *testing.T) *hierarchyEnv {
	t.Helper()

	server := newAuthServer(t, core.AuthOptions{})
	store := coretest.NewMemRoomStore()
	server.rooms = core.NewRoomService(store, "test.local")

	key, err := signing.Generate("a_local")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	server.federationKey = &key

	remoteKey, err := signing.Generate("a_remote")
	if err != nil {
		t.Fatalf("remote key: %v", err)
	}

	server.keyLookup = func(origin, keyID string) (string, int64, error) {
		if origin == "remote.test" && keyID == remoteKey.KeyID {
			return remoteKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		return "", 0, signing.ErrInvalidKey
	}

	alice := registerUser(t, server, "alice", "correct horse battery")

	return &hierarchyEnv{server: server, store: store, remoteKey: remoteKey, token: alice.AccessToken}
}

// fedGet - GET, подписанный remote.test.
func (e *hierarchyEnv) fedGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	header, err := signing.MatrixAuthHeader(e.remoteKey, "remote.test", "test.local", http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", header)

	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)

	return rec
}

// room создает комнату от alice.
func (e *hierarchyEnv) room(t *testing.T, body string) string {
	t.Helper()

	return createRoomWith(t, e.server, e.token, body)
}

// putState - событие состояния от alice.
func (e *hierarchyEnv) putState(t *testing.T, roomID, eventType, stateKey, content string) {
	t.Helper()

	rec := do(t, e.server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s", roomID, eventType, stateKey), content, e.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("state %s %s: %d %s", eventType, stateKey, rec.Code, rec.Body.String())
	}
}

// child добавляет m.space.child.
func (e *hierarchyEnv) child(t *testing.T, spaceID, childID, content string) {
	t.Helper()

	e.putState(t, spaceID, "m.space.child", childID, content)
}

// member пишет в состояние членство пользователя другого сервера (как
// принятое по федерации событие).
func (e *hierarchyEnv) member(t *testing.T, roomID, userID, membership string) {
	t.Helper()

	e.seq++

	stateKey := userID
	if _, _, err := e.store.AppendEvent(context.Background(), core.Event{
		EventID: fmt.Sprintf("$m%d:remote.test", e.seq), RoomID: roomID, Type: "m.room.member",
		Sender: userID, StateKey: &stateKey, Content: json.RawMessage(`{"membership":"` + membership + `"}`),
		OriginServerTS: 1, Origin: domainOf(userID),
	}, nil); err != nil {
		t.Fatalf("member: %v", err)
	}
}

// decodeHierarchyJSON разбирает тело ответа.
func decodeHierarchyJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}

	return out
}

// TestFederationHierarchy - федеративный /hierarchy отдает сводку
// пространства с m.space.child (порядок по order, без via - нет), известных
// нам детей, доступных серверу (public, world_readable, с его участником,
// restricted через пространство с его участником), и inaccessible_children
// для закрытых; неизвестные нам дети пропускаются. suggested_only оставляет
// suggested. Раньше маршрута не было - 404 M_UNRECOGNIZED.
func TestFederationHierarchy(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)

	space := env.room(t, `{"name":"Space","topic":"T","creation_content":{"type":"m.space"}}`)
	env.member(t, space, "@bob:remote.test", "join")

	pub := env.room(t, `{"name":"Pub","preset":"public_chat"}`)
	priv := env.room(t, `{"name":"Priv"}`)
	shared := env.room(t, `{"name":"Shared"}`)
	env.member(t, shared, "@bob:remote.test", "join")

	readable := env.room(t, `{"name":"WR","initial_state":[{"type":"m.room.history_visibility","state_key":"",
		"content":{"history_visibility":"world_readable"}}]}`)
	restricted := env.room(t, fmt.Sprintf(`{"name":"Restr","initial_state":[{"type":"m.room.join_rules","state_key":"",
		"content":{"join_rule":"restricted","allow":[{"type":"m.room_membership","room_id":%q}]}}]}`, space))

	env.child(t, space, pub, `{"via":["test.local"],"order":"b","suggested":true}`)
	env.child(t, space, priv, `{"via":["test.local"]}`)
	env.child(t, space, shared, `{"via":["test.local"],"order":"a"}`)
	env.child(t, space, readable, `{"via":["test.local"]}`)
	env.child(t, space, restricted, `{"via":["test.local"]}`)
	env.child(t, space, "!unknown:remote.test", `{"via":["remote.test"]}`)
	env.child(t, space, "!novia:test.local", `{"via":[]}`)
	env.child(t, space, "!removed:test.local", `{}`)

	rec := env.fedGet(t, "/_matrix/federation/v1/hierarchy/"+space)
	if rec.Code != http.StatusOK {
		t.Fatalf("hierarchy: %d %s", rec.Code, rec.Body.String())
	}

	resp := decodeHierarchyJSON[federationHierarchyResponse](t, rec)

	if resp.Room.RoomID != space || resp.Room.Name != "Space" || resp.Room.Topic != "T" ||
		resp.Room.RoomType != "m.space" || resp.Room.NumJoinedMembers != 2 || resp.Room.RoomVersion == "" {
		t.Fatalf("room: %+v", resp.Room.spaceSummary)
	}

	stateKeys := make([]string, 0, len(resp.Room.ChildrenState))
	for _, ev := range resp.Room.ChildrenState {
		if ev.Type != "m.space.child" || ev.Sender != "@alice:test.local" || ev.OriginServerTS == 0 || len(ev.Content) == 0 {
			t.Fatalf("children_state: %+v", ev)
		}

		stateKeys = append(stateKeys, ev.StateKey)
	}

	if len(stateKeys) != 6 || stateKeys[0] != shared || stateKeys[1] != pub {
		t.Fatalf("children_state: %v, ожидали 6 с order a, b первыми (shared %s, pub %s)", stateKeys, shared, pub)
	}

	if slices.Contains(stateKeys, "!novia:test.local") || slices.Contains(stateKeys, "!removed:test.local") {
		t.Fatalf("m.space.child без via в children_state: %v", stateKeys)
	}

	got := map[string]spaceSummary{}
	for _, c := range resp.Children {
		got[c.RoomID] = c
	}

	if len(got) != 4 || got[pub].Name != "Pub" || got[pub].JoinRule != "public" || got[shared].Name != "Shared" ||
		!got[readable].WorldReadable || got[restricted].JoinRule != "restricted" {
		t.Fatalf("children: %+v", resp.Children)
	}

	if ids := got[restricted].AllowedRoomIDs; len(ids) != 1 || ids[0] != space {
		t.Fatalf("allowed_room_ids: %v", ids)
	}

	if len(resp.InaccessibleChildren) != 1 || resp.InaccessibleChildren[0] != priv {
		t.Fatalf("inaccessible_children: %v, ожидали [%s]", resp.InaccessibleChildren, priv)
	}

	// Дети - без children_state (только сводка), room - с ним.
	var raw struct {
		Children []map[string]any `json:"children"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, c := range raw.Children {
		if _, ok := c["children_state"]; ok {
			t.Fatalf("children_state у ребенка: %v", c)
		}
	}

	// suggested_only: только suggested дети; unstable-путь MSC2946 - тот же ответ.
	rec = env.fedGet(t, "/_matrix/federation/unstable/org.matrix.msc2946/hierarchy/"+space+"?suggested_only=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("unstable suggested_only: %d %s", rec.Code, rec.Body.String())
	}

	resp = decodeHierarchyJSON[federationHierarchyResponse](t, rec)
	if len(resp.Room.ChildrenState) != 1 || resp.Room.ChildrenState[0].StateKey != pub ||
		len(resp.Children) != 1 || resp.Children[0].RoomID != pub || len(resp.InaccessibleChildren) != 0 {
		t.Fatalf("suggested_only: %s", rec.Body.String())
	}
}

// TestFederationHierarchyAccess - закрытая комната без участников
// сервера и неизвестная комната - 404 M_NOT_FOUND; запрет m.room.server_acl -
// 403; битый suggested_only - 400; без подписи - 401. Комната не-пространство
// отдается без children_state.
func TestFederationHierarchyAccess(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)

	private := env.room(t, `{"name":"Private","creation_content":{"type":"m.space"}}`)
	env.child(t, private, "!x:test.local", `{"via":["test.local"]}`)

	invited := env.room(t, `{"name":"Invited"}`)
	env.member(t, invited, "@carol:remote.test", "invite")

	public := env.room(t, `{"name":"Public","preset":"public_chat"}`)
	env.child(t, public, "!x:test.local", `{"via":["test.local"]}`)

	banned := env.room(t, `{"preset":"public_chat"}`)
	env.putState(t, banned, "m.room.server_acl", "", `{"allow":["*"],"deny":["remote.test"]}`)

	cases := []struct {
		path    string
		status  int
		errcode string
	}{
		{"/_matrix/federation/v1/hierarchy/" + private, http.StatusNotFound, "M_NOT_FOUND"},
		{"/_matrix/federation/v1/hierarchy/!nope:test.local", http.StatusNotFound, "M_NOT_FOUND"},
		{"/_matrix/federation/v1/hierarchy/" + banned, http.StatusForbidden, "M_FORBIDDEN"},
		{"/_matrix/federation/v1/hierarchy/" + public + "?suggested_only=yes", http.StatusBadRequest, "M_INVALID_PARAM"},
		{"/_matrix/federation/v1/hierarchy/" + invited, http.StatusOK, ""},
	}

	for _, tc := range cases {
		rec := env.fedGet(t, tc.path)
		if rec.Code != tc.status {
			t.Fatalf("%s: %d %s, ожидали %d", tc.path, rec.Code, rec.Body.String(), tc.status)
		}

		if tc.errcode != "" && decodeMatrixError(t, rec).Code != tc.errcode {
			t.Fatalf("%s: %s, ожидали %s", tc.path, rec.Body.String(), tc.errcode)
		}
	}

	// Не пространство: m.space.child не раскрываются.
	resp := decodeHierarchyJSON[federationHierarchyResponse](t, env.fedGet(t, "/_matrix/federation/v1/hierarchy/"+public))
	if resp.Room.RoomID != public || len(resp.Room.ChildrenState) != 0 || resp.Room.ChildrenState == nil {
		t.Fatalf("не пространство: %+v", resp.Room)
	}

	req := httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/hierarchy/"+public, http.NoBody)
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без подписи: %d", rec.Code)
	}
}
