package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestJoinByAliasRoute — POST /join/{roomIdOrAlias} должен
// существовать и резолвить алиас: без него Element не принимает приглашение
// и не входит по алиасу.
func TestJoinByAliasRoute(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","name":"join me"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}

	// Алиас создаём через directory API (не через room_alias_name в createRoom).
	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/directory/room/%23join-me%3Atest.local",
		`{"room_id":"`+created.RoomID+`"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("create alias: %d %s", rec.Code, rec.Body.String())
	}

	// Вход по алиасу через маршрут спеки.
	rec = do(t, server, http.MethodPost,
		"/_matrix/client/v3/join/%23join-me%3Atest.local", `{}`, bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join by alias: %d %s", rec.Code, rec.Body.String())
	}

	var joined struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &joined); err != nil || joined.RoomID != created.RoomID {
		t.Fatalf("join by alias вернул room_id %q, ожидали %q", joined.RoomID, created.RoomID)
	}

	// Вход по room_id тем же маршрутом.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join by id: %d %s", rec.Code, rec.Body.String())
	}
}

// TestV1RelationsAndHierarchy — hierarchy и relations спека держит под
// /v1; на /v3 клиенты получали 404.
func TestV1RelationsAndHierarchy(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","name":"v1 room"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}

	for _, path := range []string{
		"/_matrix/client/v1/rooms/" + created.RoomID + "/hierarchy",
		"/_matrix/client/v1/rooms/" + created.RoomID + "/relations/$event:test.local",
		"/_matrix/client/v1/rooms/" + created.RoomID + "/threads/$thread:test.local",
	} {
		rec := do(t, server, http.MethodGet, path, "", alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: код %d, тело %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestJoinRemoteAlias — алиас чужого сервера даёт
// явную ошибку 404 с понятным текстом, а не падение 500.
func TestJoinRemoteAlias(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost,
		"/_matrix/client/v3/join/%23room%3Aremote.test", `{}`, alice.AccessToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("join remote alias: %d %s, ожидали 404", rec.Code, rec.Body.String())
	}

	var body struct {
		Errcode string `json:"errcode"`
		Error   string `json:"error"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Errcode != "M_NOT_FOUND" {
		t.Fatalf("errcode = %q, ожидали M_NOT_FOUND", body.Errcode)
	}
}

// TestPushRulesRequireAuth — pushrules — пользовательские данные,
// вне requireAuth всегда отдавались дефолтные правила.
func TestPushRulesRequireAuth(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/pushrules/", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d, ожидали 401", rec.Code)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/pushrules/", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("с токеном: %d %s", rec.Code, rec.Body.String())
	}
}

// TestForgetRoomRoute — POST /rooms/{roomId}/forget существует.
// После выхода комната забывается, а вошедшую забыть нельзя (403).
func TestForgetRoomRoute(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}

	roomPath := "/_matrix/client/v3/rooms/" + created.RoomID

	if rec := do(t, server, http.MethodPost, roomPath+"/join", `{}`, bob.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	// Вошедшую комнату забыть нельзя: 400 M_UNKNOWN, как в Synapse
	// (Complement TestRoomForget).
	if rec := do(t, server, http.MethodPost, roomPath+"/forget", `{}`, bob.AccessToken); rec.Code != http.StatusBadRequest ||
		decodeMatrixError(t, rec).Code != ErrCodeUnknown {
		t.Fatalf("forget joined: %d %s, ожидали 400 M_UNKNOWN", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, roomPath+"/leave", `{}`, bob.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	// После выхода — можно.
	if rec := do(t, server, http.MethodPost, roomPath+"/forget", `{}`, bob.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("forget after leave: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMembersFilters — `at` принимается как sync-токен (не курсор по
// state_key), фильтры membership/not_membership работают.
func TestMembersFilters(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`, bob.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	// membership=join: оба участника.
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/members?membership=join", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("members: %d %s", rec.Code, rec.Body.String())
	}

	var page struct {
		Chunk []struct {
			StateKey string `json:"state_key"`
		} `json:"chunk"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(page.Chunk) != 2 {
		t.Fatalf("membership=join: %d событий, ожидали 2", len(page.Chunk))
	}

	// not_membership=join: никого.
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/members?not_membership=join", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("members not_membership: %d %s", rec.Code, rec.Body.String())
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(page.Chunk) != 0 {
		t.Fatalf("not_membership=join: %d событий, ожидали 0", len(page.Chunk))
	}
}

// TestUserDirectorySearch — GET /user_directory/search ищет
// локальных пользователей по localpart/display_name; маршрута не было вовсе.
func TestUserDirectorySearch(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")
	registerUser(t, server, "bobby", "1234567890")

	rec := do(t, server, http.MethodPost,
		"/_matrix/client/v3/user_directory/search", `{"search_term":"bob"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("user_directory/search: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []struct {
			UserID string `json:"user_id"`
		} `json:"results"`
		Limited bool `json:"limited"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Results) != 1 || resp.Results[0].UserID != "@bobby:test.local" {
		t.Fatalf("results = %+v, ожидали одного @bobby:test.local", resp.Results)
	}

	// Себя в выдаче не показываем.
	rec = do(t, server, http.MethodPost,
		"/_matrix/client/v3/user_directory/search", `{"search_term":"alice"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("self search: %d %s", rec.Code, rec.Body.String())
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Results) != 0 {
		t.Fatalf("свой профиль попал в выдачу: %+v", resp.Results)
	}
}
