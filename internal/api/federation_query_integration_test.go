//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// decodeQueryBody разбирает JSON-ответ в map.
func decodeQueryBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}

	return body
}

// expectStatus проверяет код и errcode ответа.
func expectStatus(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, errcode string) map[string]any {
	t.Helper()

	if rec.Code != status {
		t.Fatalf("%s: HTTP %d (%s), ожидали %d", name, rec.Code, rec.Body.String(), status)
	}

	body := decodeQueryBody(t, rec)
	if errcode != "" && body["errcode"] != errcode {
		t.Fatalf("%s: errcode %v, ожидали %s (%s)", name, body["errcode"], errcode, rec.Body.String())
	}

	return body
}

// TestFederationQueryProfile - /query/profile отдает профиль нашего
// пользователя (field - одно поле), несуществующий - 404 M_NOT_FOUND, чужой -
// 400. Раньше эндпоинта не было (M_UNRECOGNIZED), и Synapse не отправлял
// приглашение нашему пользователю.
func TestFederationQueryProfile(t *testing.T) {
	env := newFedEnv(t)
	tok := registerHTTP(t, env.server, "alice", "DEV")

	if rec := do(t, env.server, http.MethodPut, "/_matrix/client/v3/profile/@alice:test.local/displayname",
		`{"displayname":"Алиса"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("displayname: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, env.server, http.MethodPut, "/_matrix/client/v3/profile/@alice:test.local/avatar_url",
		`{"avatar_url":"mxc://test.local/a"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("avatar_url: %d %s", rec.Code, rec.Body.String())
	}

	query := func(params url.Values) *httptest.ResponseRecorder {
		return env.request(t, http.MethodGet, "/_matrix/federation/v1/query/profile?"+params.Encode(), nil)
	}

	body := expectStatus(t, "профиль", query(url.Values{"user_id": {"@alice:test.local"}}), http.StatusOK, "")
	if body["displayname"] != "Алиса" || body["avatar_url"] != "mxc://test.local/a" {
		t.Fatalf("профиль: %v", body)
	}

	body = expectStatus(t, "field=displayname",
		query(url.Values{"user_id": {"@alice:test.local"}, "field": {"displayname"}}), http.StatusOK, "")
	if body["displayname"] != "Алиса" || len(body) != 1 {
		t.Fatalf("field=displayname: %v", body)
	}

	body = expectStatus(t, "field=avatar_url",
		query(url.Values{"user_id": {"@alice:test.local"}, "field": {"avatar_url"}}), http.StatusOK, "")
	if body["avatar_url"] != "mxc://test.local/a" || len(body) != 1 {
		t.Fatalf("field=avatar_url: %v", body)
	}

	expectStatus(t, "несуществующий", query(url.Values{"user_id": {"@ghost:test.local"}}), http.StatusNotFound, "M_NOT_FOUND")
	expectStatus(t, "чужой", query(url.Values{"user_id": {"@bob:remote.test"}}), http.StatusBadRequest, "M_INVALID_PARAM")
	expectStatus(t, "без user_id", query(url.Values{}), http.StatusBadRequest, "M_MISSING_PARAM")

	// Без X-Matrix - 401: эндпоинт под проверкой подписи.
	rec := env.requestUnsigned(t, http.MethodGet, "/_matrix/federation/v1/query/profile?user_id=%40alice%3Atest.local", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без подписи: %d %s", rec.Code, rec.Body.String())
	}
}

// TestFederationQueryDirectory - /query/directory резолвит наш алиас в
// room_id и servers; неизвестный - 404 M_NOT_FOUND, чужой - 400.
func TestFederationQueryDirectory(t *testing.T) {
	env := newFedEnv(t)
	tok := registerHTTP(t, env.server, "alice", "DEV")

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"room_alias_name":"q163"}`, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	roomID, _ := decodeQueryBody(t, rec)["room_id"].(string)

	query := func(alias string) *httptest.ResponseRecorder {
		params := url.Values{}
		if alias != "" {
			params.Set("room_alias", alias)
		}

		return env.request(t, http.MethodGet, "/_matrix/federation/v1/query/directory?"+params.Encode(), nil)
	}

	body := expectStatus(t, "алиас", query("#q163:test.local"), http.StatusOK, "")
	servers, _ := body["servers"].([]any)

	if body["room_id"] != roomID || len(servers) != 1 || servers[0] != "test.local" {
		t.Fatalf("алиас: %v, ожидали room_id %s и servers [test.local]", body, roomID)
	}

	expectStatus(t, "неизвестный", query("#nope:test.local"), http.StatusNotFound, "M_NOT_FOUND")
	expectStatus(t, "чужой", query("#q163:remote.test"), http.StatusBadRequest, "M_INVALID_PARAM")
	expectStatus(t, "без room_alias", query(""), http.StatusBadRequest, "M_MISSING_PARAM")
}
