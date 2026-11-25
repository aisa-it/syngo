//go:build integration

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// queryRemote - мок чужих серверов: все server_name ведут на один httptest,
// кроме down.test (недоступен). Запоминает запросы: путь, query, destination.
type queryRemote struct {
	mu       sync.Mutex
	requests []queryRemoteRequest
}

type queryRemoteRequest struct {
	path        string
	query       url.Values
	destination string
}

// count - число запросов с путем path и значением параметра name.
func (q *queryRemote) count(path, name, value string) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := 0

	for _, req := range q.requests {
		if req.path == path && req.query.Get(name) == value {
			n++
		}
	}

	return n
}

func (q *queryRemote) all() []queryRemoteRequest {
	q.mu.Lock()
	defer q.mu.Unlock()

	return append([]queryRemoteRequest(nil), q.requests...)
}

// mockRemote подключает к серверу транспорт федерации на мок с обработчиком handler.
func (e *fedEnv) mockRemote(t *testing.T, handler http.HandlerFunc) *queryRemote {
	t.Helper()

	q := &queryRemote{}

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destination := ""
		if header, err := signing.ParseMatrixAuth(r.Header.Get("Authorization")); err == nil {
			destination = header.Destination
		}

		q.mu.Lock()
		q.requests = append(q.requests, queryRemoteRequest{path: r.URL.Path, query: r.URL.Query(), destination: destination})
		q.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(remote.Close)

	host := strings.TrimPrefix(remote.URL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.TargetsOverride = func(_ context.Context, serverName string) ([]transport.Target, error) {
		if serverName == "down.test" {
			return nil, errors.New("no such host")
		}

		return []transport.Target{{ServerName: serverName, Host: host, Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: e.server.federationKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	})
	e.server.SetFederationTransport(client)
	e.server.SetRemoteJoiner(join.New(client, e.server.db, e.server.federationKey, "test.local"))

	return q
}

// TestClientProfileRemote - клиентский профиль пользователя чужого
// сервера запрашивается у его сервера (/query/profile, подписанный запрос) и
// кэшируется; отказ и сбой федерации - 404 M_NOT_FOUND (не 5xx), отказ тоже
// кэшируется. Раньше - 404 без запроса, Element писал «may not exist».
func TestClientProfileRemote(t *testing.T) {
	env := newFedEnv(t)

	remote := env.mockRemote(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/query/profile" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED"}`))

			return
		}

		switch r.URL.Query().Get("user_id") {
		case "@bob:remote.test":
			_, _ = w.Write([]byte(`{"displayname":"Bob","avatar_url":"mxc://remote.test/b"}`))
		case "@empty:remote.test":
			_, _ = w.Write([]byte(`{"displayname":null}`))
		case "@broken:remote.test":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Profile was not found"}`))
		}
	})

	get := func(path string) *httptest.ResponseRecorder {
		return do(t, env.server, http.MethodGet, "/_matrix/client/v3/profile/"+path, "", "")
	}

	// Element шлет user_id percent-encoded.
	body := expectStatus(t, "профиль", get("%40bob%3Aremote.test"), http.StatusOK, "")
	if body["displayname"] != "Bob" || body["avatar_url"] != "mxc://remote.test/b" {
		t.Fatalf("профиль: %v", body)
	}

	body = expectStatus(t, "displayname", get("@bob:remote.test/displayname"), http.StatusOK, "")
	if body["displayname"] != "Bob" {
		t.Fatalf("displayname: %v", body)
	}

	body = expectStatus(t, "avatar_url", get("@bob:remote.test/avatar_url"), http.StatusOK, "")
	if body["avatar_url"] != "mxc://remote.test/b" {
		t.Fatalf("avatar_url: %v", body)
	}

	if n := remote.count("/_matrix/federation/v1/query/profile", "user_id", "@bob:remote.test"); n != 1 {
		t.Fatalf("запросов профиля bob: %d, ожидали 1 (кэш)", n)
	}

	if reqs := remote.all(); len(reqs) == 0 || reqs[0].destination != "remote.test" {
		t.Fatalf("запрос без подписи X-Matrix для remote.test: %+v", reqs)
	}

	body = expectStatus(t, "пустой профиль", get("@empty:remote.test"), http.StatusOK, "")
	if len(body) != 0 {
		t.Fatalf("пустой профиль: %v", body)
	}

	for i := range 2 {
		expectStatus(t, "нет пользователя", get("@ghost:remote.test"), http.StatusNotFound, "M_NOT_FOUND")
		_ = i
	}

	if n := remote.count("/_matrix/federation/v1/query/profile", "user_id", "@ghost:remote.test"); n != 1 {
		t.Fatalf("запросов профиля ghost: %d, ожидали 1 (отрицательный кэш)", n)
	}

	expectStatus(t, "5xx чужого сервера", get("@broken:remote.test"), http.StatusNotFound, "M_NOT_FOUND")
	expectStatus(t, "недоступный сервер", get("@bob:down.test"), http.StatusNotFound, "M_NOT_FOUND")
	expectStatus(t, "недоступный сервер, displayname", get("@bob:down.test/displayname"), http.StatusNotFound, "M_NOT_FOUND")
}

// TestClientDirectoryRemote - алиас чужого сервера резолвится через
// его /query/directory, join по такому алиасу идет make_join через servers из
// ответа. Раньше - 404 «Remote alias resolution is not supported».
func TestClientDirectoryRemote(t *testing.T) {
	env := newFedEnv(t)

	remote := env.mockRemote(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/federation/v1/query/directory" &&
			r.URL.Query().Get("room_alias") == "#room:remote.test":
			_, _ = w.Write([]byte(`{"room_id":"!r:other.test","servers":["remote.test","other.test"]}`))
		case strings.HasPrefix(r.URL.Path, "/_matrix/federation/v1/make_join/"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
		}
	})

	body := expectStatus(t, "алиас", do(t, env.server, http.MethodGet,
		"/_matrix/client/v3/directory/room/%23room%3Aremote.test", "", ""), http.StatusOK, "")
	servers, _ := body["servers"].([]any)

	if body["room_id"] != "!r:other.test" || len(servers) != 2 || servers[0] != "remote.test" {
		t.Fatalf("алиас: %v", body)
	}

	expectStatus(t, "неизвестный алиас", do(t, env.server, http.MethodGet,
		"/_matrix/client/v3/directory/room/%23nope%3Aremote.test", "", ""), http.StatusNotFound, "M_NOT_FOUND")
	expectStatus(t, "недоступный сервер", do(t, env.server, http.MethodGet,
		"/_matrix/client/v3/directory/room/%23room%3Adown.test", "", ""), http.StatusNotFound, "M_NOT_FOUND")

	if n := remote.count("/_matrix/federation/v1/query/directory", "room_alias", "#room:remote.test"); n != 1 {
		t.Fatalf("запросов алиаса: %d, ожидали 1", n)
	}

	// Join по алиасу: make_join идет первому серверу из servers, а не серверу room_id.
	tok := env.localUserToken(t)

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/join/%23room%3Aremote.test", `{}`, tok)
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusOK {
		t.Fatalf("join: %d %s, ожидали отказ чужого сервера", rec.Code, rec.Body.String())
	}

	var makeJoins []queryRemoteRequest

	for _, req := range remote.all() {
		if strings.HasPrefix(req.path, "/_matrix/federation/v1/make_join/") {
			makeJoins = append(makeJoins, req)
		}
	}

	if len(makeJoins) != 2 || makeJoins[0].destination != "remote.test" || makeJoins[1].destination != "other.test" ||
		!strings.Contains(makeJoins[0].path, "!r:other.test") {
		t.Fatalf("make_join: %+v, ожидали через remote.test, затем other.test", makeJoins)
	}

	expectStatus(t, "join по неизвестному алиасу", do(t, env.server, http.MethodPost,
		"/_matrix/client/v3/join/%23nope%3Aremote.test", `{}`, tok), http.StatusNotFound, "M_NOT_FOUND")
}
