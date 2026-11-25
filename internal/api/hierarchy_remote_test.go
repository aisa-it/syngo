package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/transport"
)

// hierarchyRemote - мок чужих серверов для /hierarchy: все server_name ведут на
// один httptest, кроме down.test. Запоминает пути запросов.
type hierarchyRemote struct {
	mu    sync.Mutex
	paths []string
}

func (h *hierarchyRemote) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.paths)
}

// count - число запросов, путь которых содержит part.
func (h *hierarchyRemote) count(part string) int {
	n := 0

	for _, path := range h.all() {
		if strings.Contains(path, part) {
			n++
		}
	}

	return n
}

// remote подключает транспорт федерации к моку с ответами responses (путь
// без query - тело ответа 200); остальное - 404 M_NOT_FOUND или, для путей из
// unrecognized, 404 M_UNRECOGNIZED.
func (e *hierarchyEnv) remote(t *testing.T, responses map[string]string, unrecognized ...string) *hierarchyRemote {
	t.Helper()

	h := &hierarchyRemote{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.RequestURI())
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		if body, ok := responses[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))

			return
		}

		w.WriteHeader(http.StatusNotFound)

		if slices.Contains(unrecognized, r.URL.Path) {
			_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`))

			return
		}

		_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Unknown room"}`))
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(_ context.Context, serverName string) ([]transport.Target, error) {
		if serverName == "down.test" {
			return nil, errors.New("no such host")
		}

		return []transport.Target{{ServerName: serverName, Host: host, Scheme: "http"}}, nil
	}

	e.server.SetFederationTransport(transport.NewClient(transport.Options{
		Resolver: resolver, Key: e.server.federationKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	}))

	return h
}

// hierarchy - клиентский GET /v1/rooms/{roomId}/hierarchy.
func (e *hierarchyEnv) hierarchy(t *testing.T, token, roomID, query string) *httptest.ResponseRecorder {
	t.Helper()

	path := "/_matrix/client/v1/rooms/" + url.PathEscape(roomID) + "/hierarchy"
	if query != "" {
		path += "?" + query
	}

	return do(t, e.server, http.MethodGet, path, "", token)
}

// roomIDs - room_id комнат ответа по порядку.
func roomIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("hierarchy: %d %s", rec.Code, rec.Body.String())
	}

	resp := decodeHierarchyJSON[hierarchyResponse](t, rec)
	ids := make([]string, 0, len(resp.Rooms))

	for i := range resp.Rooms {
		ids = append(ids, resp.Rooms[i].RoomID)
	}

	return ids
}

const (
	hierarchyV1       = "/_matrix/federation/v1/hierarchy/"
	hierarchyUnstable = "/_matrix/federation/unstable/org.matrix.msc2946/hierarchy/"
)

// remoteSpaceBody - ответ чужого сервера о пространстве !rspace:remote.test.
const remoteSpaceBody = `{
	"room": {"room_id":"!rspace:remote.test","name":"RSpace","room_type":"m.space","join_rule":"public",
		"num_joined_members":3,"world_readable":false,"guest_can_join":false,"allowed_room_ids":["!x:remote.test"],
		"children_state":[
			{"type":"m.space.child","state_key":"!deep:remote.test","content":{"via":["remote.test"]},
			 "sender":"@bob:remote.test","origin_server_ts":5},
			{"type":"m.space.child","state_key":"!hidden:remote.test","content":{"via":["remote.test"]},
			 "sender":"@bob:remote.test","origin_server_ts":6},
			{"type":"m.space.child","state_key":"!closed:remote.test","content":{"via":["remote.test"]},
			 "sender":"@bob:remote.test","origin_server_ts":7}
		]},
	"children": [
		{"room_id":"!deep:remote.test","name":"Deep","join_rule":"public","num_joined_members":1},
		{"room_id":"!closed:remote.test","name":"Closed","join_rule":"invite","num_joined_members":1},
		{"name":"no id"}
	],
	"inaccessible_children": ["!hidden:remote.test"]
}`

// TestHierarchyRemoteChildren - клиентский /hierarchy спрашивает
// детей, в которых нас нет, федеративным /hierarchy у серверов из via (v1, при
// M_UNRECOGNIZED - unstable MSC2946). Внуки берутся из children ответа без
// повторного запроса, inaccessible_children не спрашиваются, закрытые для
// пользователя комнаты, битый ответ и недоступный сервер пропускаются; ответы и
// отказы кэшируются. Раньше чужие дети молча выпадали из ответа.
func TestHierarchyRemoteChildren(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)
	remote := env.remote(t, map[string]string{
		hierarchyV1 + "!rspace:remote.test":     remoteSpaceBody,
		hierarchyUnstable + "!old:remote.test":  `{"room":{"room_id":"!old:remote.test","name":"Old","world_readable":true}}`,
		hierarchyV1 + "!wrong:remote.test":      `{"room":{"room_id":"!other:remote.test","join_rule":"public"}}`,
		hierarchyV1 + "!private:remote.test":    `{"room":{"room_id":"!private:remote.test","join_rule":"invite"},"children":[]}`,
		hierarchyV1 + "!badstate:remote.test":   `{"room":{"room_id":"!badstate:remote.test","join_rule":"public","children_state":[{"type":"m.space.child"}]}}`,
		hierarchyV1 + "!array:remote.test":      `[]`,
		hierarchyV1 + "!secondvia:remote2.test": `{"room":{"room_id":"!secondvia:remote2.test","name":"Second","join_rule":"public"}}`,
	}, hierarchyV1+"!old:remote.test")

	space := env.room(t, `{"name":"Space","creation_content":{"type":"m.space"}}`)
	local := env.room(t, `{"name":"Local"}`)

	env.child(t, space, local, `{"via":["test.local"],"order":"0"}`)
	env.child(t, space, "!rspace:remote.test", `{"via":["remote.test"],"order":"1"}`)
	env.child(t, space, "!old:remote.test", `{"via":["remote.test"],"order":"2"}`)

	for i, id := range []string{"!wrong:remote.test", "!private:remote.test", "!badstate:remote.test", "!array:remote.test"} {
		env.child(t, space, id, fmt.Sprintf(`{"via":["remote.test"],"order":"3%d"}`, i))
	}

	env.child(t, space, "!down:down.test", `{"via":["down.test"],"order":"4"}`)
	// Свой сервер в via пропускается, отказ первого сервера - спрашиваем второй.
	env.child(t, space, "!secondvia:remote2.test", `{"via":["test.local","down.test","remote2.test"],"order":"5"}`)

	want := []string{space, local, "!rspace:remote.test", "!deep:remote.test", "!old:remote.test", "!secondvia:remote2.test"}

	rec := env.hierarchy(t, env.token, space, "")
	if got := roomIDs(t, rec); !slices.Equal(got, want) {
		t.Fatalf("rooms: %v, ожидали %v (%s)", got, want, rec.Body.String())
	}

	resp := decodeHierarchyJSON[hierarchyResponse](t, rec)

	rspace := resp.Rooms[2]
	if rspace.Name != "RSpace" || rspace.NumJoinedMembers != 3 || len(rspace.ChildrenState) != 3 ||
		rspace.AllowedRoomIDs != nil || rspace.ChildrenState[0].Sender != "@bob:remote.test" {
		t.Fatalf("чужое пространство: %+v", rspace)
	}

	if strings.Contains(rec.Body.String(), "allowed_room_ids") {
		t.Fatalf("allowed_room_ids в клиентском ответе: %s", rec.Body.String())
	}

	if deep := resp.Rooms[3]; deep.Name != "Deep" || deep.ChildrenState == nil {
		t.Fatalf("внук: %+v", deep)
	}

	for _, part := range []string{"deep", "hidden", "closed"} {
		if n := remote.count(part); n != 0 {
			t.Fatalf("запросов о %s: %d, ожидали 0 (%v)", part, n, remote.all())
		}
	}

	if remote.count(hierarchyV1+"%21old") != 1 || remote.count(hierarchyUnstable+"%21old") != 1 {
		t.Fatalf("fallback на unstable: %v", remote.all())
	}

	before := len(remote.all())

	if got := roomIDs(t, env.hierarchy(t, env.token, space, "")); !slices.Equal(got, want) {
		t.Fatalf("повтор: %v", got)
	}

	if after := len(remote.all()); after != before {
		t.Fatalf("повтор ушел в сеть: %d запросов, было %d (%v)", after, before, remote.all())
	}
}

// TestHierarchyRemoteRoot - узел, которого у нас нет, спрашивается у
// сервера из его room_id (предпросмотр чужого пространства); закрытое для
// пользователя или неизвестное чужому серверу - 403; suggested_only уходит в
// запрос к чужому серверу.
func TestHierarchyRemoteRoot(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)
	remote := env.remote(t, map[string]string{
		hierarchyV1 + "!rspace:remote.test":  remoteSpaceBody,
		hierarchyV1 + "!private:remote.test": `{"room":{"room_id":"!private:remote.test","join_rule":"invite"}}`,
	})

	want := []string{"!rspace:remote.test", "!deep:remote.test"}
	if got := roomIDs(t, env.hierarchy(t, env.token, "!rspace:remote.test", "")); !slices.Equal(got, want) {
		t.Fatalf("чужое пространство: %v, ожидали %v", got, want)
	}

	// Узел спрошен один раз: повторно в обходе - из кэша.
	if n := remote.count("rspace"); n != 1 {
		t.Fatalf("запросов узла: %d (%v)", n, remote.all())
	}

	for _, id := range []string{"!private:remote.test", "!nope:remote.test", "!nope:down.test"} {
		if rec := env.hierarchy(t, env.token, id, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s, ожидали 403", id, rec.Code, rec.Body.String())
		}
	}

	_ = env.hierarchy(t, env.token, "!sugg:remote.test", "suggested_only=true")

	if n := remote.count(hierarchyV1 + "%21sugg:remote.test?suggested_only=true"); n != 1 {
		t.Fatalf("suggested_only в запросе: %v", remote.all())
	}
}

// TestHierarchyPagination - limit режет страницу, next_batch
// продолжает обход с того же места; токен другой комнаты или с другими
// параметрами - 400. Раньше next_batch не выдавался, и остаток терялся.
func TestHierarchyPagination(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)
	space := env.room(t, `{"name":"Space","creation_content":{"type":"m.space"}}`)
	other := env.room(t, `{"name":"Other"}`)

	children := make([]string, 0, 3)

	for i := range 3 {
		child := env.room(t, fmt.Sprintf(`{"name":"C%d"}`, i))
		env.child(t, space, child, fmt.Sprintf(`{"via":["test.local"],"order":"%d"}`, i))
		children = append(children, child)
	}

	// Дубль: тот же ребенок через вложенное пространство не повторяется.
	sub := env.room(t, `{"name":"Sub","creation_content":{"type":"m.space"}}`)
	env.child(t, sub, children[0], `{"via":["test.local"]}`)
	env.child(t, space, sub, `{"via":["test.local"],"order":"9"}`)

	rec := env.hierarchy(t, env.token, space, "limit=2")
	if got := roomIDs(t, rec); !slices.Equal(got, []string{space, children[0]}) {
		t.Fatalf("первая страница: %v", got)
	}

	next := decodeHierarchyJSON[hierarchyResponse](t, rec).NextBatch
	if next == "" {
		t.Fatal("нет next_batch")
	}

	for _, bad := range []struct{ room, query string }{
		{other, "from=" + next},
		{space, "from=" + next + "&suggested_only=true"},
		{space, "from=" + next + "&max_depth=3"},
		{space, "from=nope"},
	} {
		if rec := env.hierarchy(t, env.token, bad.room, bad.query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, ожидали 400", bad.query, rec.Code)
		}
	}

	// Повтор с тем же from дает ту же страницу.
	for range 2 {
		rec = env.hierarchy(t, env.token, space, "limit=2&from="+next)
		if got := roomIDs(t, rec); !slices.Equal(got, []string{children[1], children[2]}) {
			t.Fatalf("вторая страница: %v", got)
		}
	}

	rec = env.hierarchy(t, env.token, space, "from="+decodeHierarchyJSON[hierarchyResponse](t, rec).NextBatch)
	resp := decodeHierarchyJSON[hierarchyResponse](t, rec)

	if len(resp.Rooms) != 1 || resp.Rooms[0].RoomID != sub || resp.NextBatch != "" {
		t.Fatalf("последняя страница: %s", rec.Body.String())
	}

	for _, query := range []string{"limit=0", "limit=x", "max_depth=-1", "suggested_only=1"} {
		if rec := env.hierarchy(t, env.token, space, query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, ожидали 400", query, rec.Code)
		}
	}
}

// TestHierarchyDepthAndAccess - без max_depth обход идет вглубь
// (раньше - только дети узла), max_depth=1 останавливается на детях;
// suggested_only оставляет suggested; посторонний видит публичное
// пространство и его публичных и world_readable детей, restricted - если
// состоит в комнате из allow; world_readable - по history_visibility (раньше
// по join_rule).
func TestHierarchyDepthAndAccess(t *testing.T) {
	t.Parallel()

	env := newHierarchyEnv(t)
	bob := registerUser(t, env.server, "bob", "correct horse battery")

	space := env.room(t, `{"name":"Space","preset":"public_chat","creation_content":{"type":"m.space"}}`)
	sub := env.room(t, `{"name":"Sub","preset":"public_chat","creation_content":{"type":"m.space"}}`)
	leaf := env.room(t, `{"name":"Leaf","initial_state":[{"type":"m.room.history_visibility","state_key":"",
		"content":{"history_visibility":"world_readable"}}]}`)
	private := env.room(t, `{"name":"Private"}`)
	restricted := env.room(t, fmt.Sprintf(`{"name":"Restr","initial_state":[{"type":"m.room.join_rules","state_key":"",
		"content":{"join_rule":"restricted","allow":[{"type":"m.room_membership","room_id":%q}]}}]}`, sub))

	env.child(t, space, sub, `{"via":["test.local"],"order":"a","suggested":true}`)
	env.child(t, space, private, `{"via":["test.local"],"order":"b"}`)
	env.child(t, space, restricted, `{"via":["test.local"],"order":"c"}`)
	env.child(t, sub, leaf, `{"via":["test.local"]}`)

	if got := roomIDs(t, env.hierarchy(t, env.token, space, "")); !slices.Equal(got, []string{space, sub, leaf, private, restricted}) {
		t.Fatalf("без max_depth: %v", got)
	}

	if got := roomIDs(t, env.hierarchy(t, env.token, space, "max_depth=1")); !slices.Equal(got, []string{space, sub, private, restricted}) {
		t.Fatalf("max_depth=1: %v", got)
	}

	if got := roomIDs(t, env.hierarchy(t, env.token, space, "suggested_only=true")); !slices.Equal(got, []string{space, sub}) {
		t.Fatalf("suggested_only: %v", got)
	}

	rec := env.hierarchy(t, bob.AccessToken, space, "")
	if got := roomIDs(t, rec); !slices.Equal(got, []string{space, sub, leaf}) {
		t.Fatalf("посторонний: %v", got)
	}

	if resp := decodeHierarchyJSON[hierarchyResponse](t, rec); !resp.Rooms[2].WorldReadable || resp.Rooms[1].WorldReadable {
		t.Fatalf("world_readable: %s", rec.Body.String())
	}

	if rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+sub+"/join", "", bob.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("join sub: %d %s", rec.Code, rec.Body.String())
	}

	if got := roomIDs(t, env.hierarchy(t, bob.AccessToken, space, "")); !slices.Equal(got, []string{space, sub, leaf, restricted}) {
		t.Fatalf("участник комнаты из allow: %v", got)
	}

	if rec := env.hierarchy(t, bob.AccessToken, private, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("закрытая комната: %d", rec.Code)
	}
}

// TestParseRemoteHierarchy - проверка ответа чужого сервера.
func TestParseRemoteHierarchy(t *testing.T) {
	t.Parallel()

	bad := []string{
		`not json`,
		`{}`,
		`{"room":"x"}`,
		`{"room":{"room_id":"!other:r"}}`,
		`{"room":{"room_id":"!r:r","children_state":{}}}`,
		`{"room":{"room_id":"!r:r","children_state":["x"]}}`,
		`{"room":{"room_id":"!r:r","children_state":[{"type":1,"state_key":"!c:r","content":{"via":[]}}]}}`,
		`{"room":{"room_id":"!r:r","children_state":[{"type":"m.space.child","state_key":"!c:r","content":{}}]}}`,
		`{"room":{"room_id":"!r:r","children_state":[{"type":"m.space.child","state_key":"!c:r","content":{"via":[1]}}]}}`,
		`{"room":{"room_id":"!r:r"},"children":["x"]}`,
		`{"room":{"room_id":"!r:r"},"inaccessible_children":[1]}`,
	}

	for _, body := range bad {
		if _, err := parseRemoteHierarchy([]byte(body), "!r:r"); err == nil {
			t.Fatalf("%s: ожидали ошибку", body)
		}
	}

	space, err := parseRemoteHierarchy([]byte(`{"room":{"room_id":"!r:r","name":42,"num_joined_members":"x",
		"children_state":[{"type":"m.space.child","state_key":"!c:r","content":{"via":[]}}]},
		"children":[{"room_id":"!c:r","topic":"T"}],"inaccessible_children":["!h:r"]}`), "!r:r")
	if err != nil || space.room.Name != "" || len(space.room.ChildrenState) != 1 ||
		space.children["!c:r"].Topic != "T" || !space.inaccessible["!h:r"] {
		t.Fatalf("мягкий разбор: %+v %v", space, err)
	}

	cases := []struct {
		status int
		err    error
		want   bool
	}{
		{http.StatusNotFound, nil, true},
		{http.StatusNotFound, errors.New(`HTTP 404: {"errcode":"M_UNRECOGNIZED"}`), true},
		{http.StatusNotFound, errors.New(`HTTP 404: {"errcode":"M_NOT_FOUND"}`), false},
		{http.StatusMethodNotAllowed, errors.New("HTTP 405"), true},
		{http.StatusBadRequest, errors.New(`HTTP 400: {"errcode":"M_UNRECOGNIZED"}`), true},
		{http.StatusBadRequest, errors.New(`HTTP 400: {"errcode":"M_INVALID_PARAM"}`), false},
		{http.StatusForbidden, errors.New(`HTTP 403: {"errcode":"M_FORBIDDEN"}`), false},
		{0, errors.New("network"), false},
	}

	for _, tc := range cases {
		if got := unrecognizedEndpoint(tc.status, tc.err); got != tc.want {
			t.Fatalf("unrecognizedEndpoint(%d, %v) = %v", tc.status, tc.err, got)
		}
	}
}

// TestHierarchySessionsEvict - переполнение сессий next_batch
// очищает хранилище, истекшая сессия не находится.
func TestHierarchySessionsEvict(t *testing.T) {
	t.Parallel()

	var sessions hierarchySessions

	first := sessions.put(&hierarchySession{roomID: "!a:t"})
	if s, ok := sessions.get(first); !ok || s.roomID != "!a:t" {
		t.Fatal("сессия не найдена")
	}

	for range hierarchySessionsMax {
		sessions.put(&hierarchySession{})
	}

	if len(sessions.sessions) > hierarchySessionsMax {
		t.Fatalf("сессий %d", len(sessions.sessions))
	}

	sessions.sessions[first] = &hierarchySession{}
	if _, ok := sessions.get(first); ok {
		t.Fatal("истекшая сессия найдена")
	}
}
