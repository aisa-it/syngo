package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHS - минимальный homeserver в памяти: столько CS API, сколько нужно
// генератору (регистрация, комнаты, send, /sync с long-poll, медиа, ключи).
// Проверяет сценарий целиком без syngod и БД.
type fakeHS struct {
	t      *testing.T
	secret string

	mu      sync.Mutex
	wake    chan struct{}
	users   map[string]string // localpart -> token
	tokens  map[string]string // token -> user_id
	rooms   map[string]*fakeRoom
	aliases map[string]string
	events  []fakeEvent
	media   map[string]int
	nonces  int
	// send429 - сколько первых PUT /send ответить 429 (проверка паузы по retry_after).
	send429 int
	sends   int
	whoami  int
}

type fakeRoom struct {
	members map[string]bool
	invites map[string]bool
}

type fakeEvent struct {
	room, id, sender, typ string
	content               json.RawMessage
}

func newFakeHS(t *testing.T, secret string) *fakeHS {
	return &fakeHS{
		t: t, secret: secret, wake: make(chan struct{}), users: map[string]string{}, tokens: map[string]string{},
		rooms: map[string]*fakeRoom{}, aliases: map[string]string{}, media: map[string]int{},
	}
}

const fakeServer = "fake.test"

func writeJSONT(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func merr(w http.ResponseWriter, status int, code string) {
	writeJSONT(w, status, map[string]any{"errcode": code, "error": code, "retry_after_ms": 20})
}

// notify будит long-poll (под h.mu).
func (h *fakeHS) notify() {
	close(h.wake)
	h.wake = make(chan struct{})
}

func (h *fakeHS) user(r *http.Request) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
}

func (h *fakeHS) handler() http.Handler {
	mux := http.NewServeMux()

	auth := func(fn func(w http.ResponseWriter, r *http.Request, user string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user := h.user(r)
			if user == "" {
				merr(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")

				return
			}

			fn(w, r, user)
		}
	}

	mux.HandleFunc("GET /_matrix/client/versions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "FakeHS/1")
		writeJSONT(w, http.StatusOK, map[string]any{"versions": []string{"v1.11"}})
	})
	mux.HandleFunc("HEAD /_matrix/client/versions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "FakeHS/1")
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		n := len(h.events)
		h.mu.Unlock()

		fmt.Fprintf(w, "syngo_build_info{version=\"fake\",commit=\"abc\",goversion=\"go\"} 1\n")
		fmt.Fprintf(w, "syngo_http_requests_total{method=\"PUT\",route=\"/send\",code=\"200\"} %d\n", n)
		fmt.Fprintf(w, "syngo_http_request_duration_seconds_bucket{method=\"PUT\",route=\"/send\",le=\"0.1\"} %d\n", n)
		fmt.Fprintf(w, "syngo_http_request_duration_seconds_bucket{method=\"PUT\",route=\"/send\",le=\"+Inf\"} %d\n", n)
		fmt.Fprintf(w, "syngo_sync_duration_seconds_bucket{le=\"0.05\"} %d\n", n)
		fmt.Fprintf(w, "syngo_sync_duration_seconds_bucket{le=\"+Inf\"} %d\n", n)
		fmt.Fprintf(w, "syngo_sync_longpoll_waiters 3\ngo_goroutines 42\n")
	})

	mux.HandleFunc("GET /_synapse/admin/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		h.nonces++
		n := h.nonces
		h.mu.Unlock()

		writeJSONT(w, http.StatusOK, map[string]string{"nonce": "n" + strconv.Itoa(n)})
	})

	mux.HandleFunc("POST /_synapse/admin/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Nonce, Username, Password, Mac string
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Mac != sharedSecretMAC(h.secret, req.Nonce, req.Username, req.Password, false) {
			merr(w, http.StatusForbidden, "M_FORBIDDEN")

			return
		}

		h.register(w, req.Username)
	})

	mux.HandleFunc("POST /_matrix/client/v3/register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string
			Auth     map[string]string
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Auth["type"] != "m.login.dummy" || req.Auth["session"] != "s1" {
			writeJSONT(w, http.StatusUnauthorized, map[string]any{"session": "s1", "flows": []any{}})

			return
		}

		h.register(w, req.Username)
	})

	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Identifier map[string]string
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		h.mu.Lock()
		defer h.mu.Unlock()

		local := req.Identifier["user"]
		if _, ok := h.users[local]; !ok {
			merr(w, http.StatusForbidden, "M_FORBIDDEN")

			return
		}

		token := "tok-" + local + "-" + strconv.Itoa(len(h.tokens))
		h.users[local] = token
		h.tokens[token] = "@" + local + ":" + fakeServer
		writeJSONT(w, http.StatusOK, map[string]string{"user_id": h.tokens[token], "access_token": token, "device_id": "D" + local})
	})

	mux.HandleFunc("GET /_matrix/client/v3/account/whoami", auth(func(w http.ResponseWriter, _ *http.Request, user string) {
		h.mu.Lock()
		h.whoami++
		h.mu.Unlock()
		writeJSONT(w, http.StatusOK, map[string]string{"user_id": user})
	}))

	ok := func(w http.ResponseWriter, _ *http.Request, _ string) { writeJSONT(w, http.StatusOK, map[string]any{}) }

	mux.HandleFunc("POST /_matrix/client/v3/keys/upload", auth(func(w http.ResponseWriter, _ *http.Request, _ string) {
		writeJSONT(w, http.StatusOK, map[string]any{"one_time_key_counts": map[string]int{"signed_curve25519": 20}})
	}))
	mux.HandleFunc("POST /_matrix/client/v3/keys/query", auth(ok))
	mux.HandleFunc("POST /_matrix/client/v3/user/{user}/filter", auth(func(w http.ResponseWriter, _ *http.Request, _ string) {
		writeJSONT(w, http.StatusOK, map[string]string{"filter_id": "7"})
	}))
	mux.HandleFunc("PUT /_matrix/client/v3/rooms/{room}/typing/{user}", auth(ok))
	mux.HandleFunc("POST /_matrix/client/v3/rooms/{room}/receipt/{type}/{event}", auth(ok))
	mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/messages", auth(func(w http.ResponseWriter, _ *http.Request, _ string) {
		writeJSONT(w, http.StatusOK, map[string]any{"chunk": []any{}})
	}))

	mux.HandleFunc("GET /_matrix/client/v3/directory/room/{alias}", auth(func(w http.ResponseWriter, r *http.Request, _ string) {
		h.mu.Lock()
		id, found := h.aliases[r.PathValue("alias")]
		h.mu.Unlock()

		if !found {
			merr(w, http.StatusNotFound, "M_NOT_FOUND")

			return
		}

		writeJSONT(w, http.StatusOK, map[string]string{"room_id": id})
	}))

	mux.HandleFunc("POST /_matrix/client/v3/createRoom", auth(func(w http.ResponseWriter, r *http.Request, user string) {
		var req struct {
			RoomAliasName string   `json:"room_alias_name"`
			Invite        []string `json:"invite"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		h.mu.Lock()
		defer h.mu.Unlock()

		id := fmt.Sprintf("!r%d:%s", len(h.rooms), fakeServer)
		room := &fakeRoom{members: map[string]bool{user: true}, invites: map[string]bool{}}

		for _, u := range req.Invite {
			room.invites[u] = true
		}

		h.rooms[id] = room

		if req.RoomAliasName != "" {
			h.aliases["#"+req.RoomAliasName+":"+fakeServer] = id
		}

		h.notify()
		writeJSONT(w, http.StatusOK, map[string]string{"room_id": id})
	}))

	membership := func(join bool) func(w http.ResponseWriter, r *http.Request, user string) {
		return func(w http.ResponseWriter, r *http.Request, user string) {
			h.mu.Lock()
			defer h.mu.Unlock()

			room := h.rooms[r.PathValue("room")]
			if room == nil {
				merr(w, http.StatusNotFound, "M_NOT_FOUND")

				return
			}

			delete(room.invites, user)

			if join {
				room.members[user] = true
			} else {
				delete(room.members, user)
			}

			h.notify()
			writeJSONT(w, http.StatusOK, map[string]string{"room_id": r.PathValue("room")})
		}
	}

	mux.HandleFunc("POST /_matrix/client/v3/join/{room}", auth(membership(true)))
	mux.HandleFunc("POST /_matrix/client/v3/rooms/{room}/join", auth(membership(true)))
	mux.HandleFunc("POST /_matrix/client/v3/rooms/{room}/leave", auth(membership(false)))

	mux.HandleFunc("POST /_matrix/client/v3/rooms/{room}/invite", auth(func(w http.ResponseWriter, r *http.Request, _ string) {
		var req struct {
			UserID string `json:"user_id"`
		}

		_ = json.NewDecoder(r.Body).Decode(&req)

		h.mu.Lock()
		defer h.mu.Unlock()

		if room := h.rooms[r.PathValue("room")]; room != nil && !room.members[req.UserID] {
			room.invites[req.UserID] = true
			h.notify()
		}

		writeJSONT(w, http.StatusOK, map[string]any{})
	}))

	mux.HandleFunc("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}", auth(func(w http.ResponseWriter, r *http.Request, user string) {
		var content json.RawMessage

		_ = json.NewDecoder(r.Body).Decode(&content)

		h.mu.Lock()
		defer h.mu.Unlock()

		h.sends++
		if h.sends <= h.send429 {
			merr(w, http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")

			return
		}

		room := h.rooms[r.PathValue("room")]
		if room == nil || !room.members[user] {
			merr(w, http.StatusForbidden, "M_FORBIDDEN")

			return
		}

		id := fmt.Sprintf("$e%d", len(h.events))
		h.events = append(h.events, fakeEvent{room: r.PathValue("room"), id: id, sender: user, typ: r.PathValue("type"), content: content})
		h.notify()
		writeJSONT(w, http.StatusOK, map[string]string{"event_id": id})
	}))

	mux.HandleFunc("POST /_matrix/media/v3/upload", auth(func(w http.ResponseWriter, r *http.Request, _ string) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)

		h.mu.Lock()
		id := "m" + strconv.Itoa(len(h.media))
		h.media[id] = n
		h.mu.Unlock()

		writeJSONT(w, http.StatusOK, map[string]string{"content_uri": "mxc://" + fakeServer + "/" + id})
	}))

	mux.HandleFunc("GET /_matrix/client/v1/media/download/{server}/{id}", auth(func(w http.ResponseWriter, r *http.Request, _ string) {
		h.mu.Lock()
		n, found := h.media[r.PathValue("id")]
		h.mu.Unlock()

		if !found {
			merr(w, http.StatusNotFound, "M_NOT_FOUND")

			return
		}

		_, _ = w.Write(make([]byte, n))
	}))

	mux.HandleFunc("GET /_matrix/client/v3/sync", auth(h.sync))

	return mux
}

func (h *fakeHS) register(w http.ResponseWriter, local string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.users[local]; exists {
		merr(w, http.StatusBadRequest, "M_USER_IN_USE")

		return
	}

	token := "tok-" + local
	h.users[local] = token
	h.tokens[token] = "@" + local + ":" + fakeServer
	writeJSONT(w, http.StatusOK, map[string]string{"user_id": h.tokens[token], "access_token": token, "device_id": "D" + local})
}

// sync - события комнат пользователя после since (позиция - индекс в h.events),
// приглашения; long-poll до timeout или нового события.
func (h *fakeHS) sync(w http.ResponseWriter, r *http.Request, user string) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	timeoutMS, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)

	for {
		h.mu.Lock()
		resp, changed := h.buildSync(user, since)
		wake := h.wake
		h.mu.Unlock()

		if changed || r.URL.Query().Get("since") == "" || time.Now().After(deadline) {
			writeJSONT(w, http.StatusOK, resp)

			return
		}

		select {
		case <-wake:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			return
		}
	}
}

func (h *fakeHS) buildSync(user string, since int) (map[string]any, bool) {
	join := map[string]any{}
	invite := map[string]any{}
	changed := false

	for id, room := range h.rooms {
		if room.invites[user] {
			invite[id] = map[string]any{}
			changed = true
		}

		if !room.members[user] {
			continue
		}

		events := []any{}

		for _, ev := range h.events[min(since, len(h.events)):] {
			if ev.room == id {
				events = append(events, map[string]any{"event_id": ev.id, "sender": ev.sender, "type": ev.typ, "content": ev.content})
			}
		}

		if len(events) > 0 || since == 0 {
			changed = changed || len(events) > 0
			join[id] = map[string]any{"timeline": map[string]any{"events": events, "limited": false, "prev_batch": "p" + strconv.Itoa(since)}}
		}
	}

	return map[string]any{
		"next_batch": strconv.Itoa(len(h.events)),
		"rooms":      map[string]any{"join": join, "invite": invite, "leave": map[string]any{}},
	}, changed
}

// TestRunAgainstFakeServer - прогон целиком: подготовка, нагрузка, замер, отчет.
// Второй прогон берет пользователей из кеша токенов и комнаты по алиасам.
func TestRunAgainstFakeServer(t *testing.T) {
	const secret = "fake-shared-secret"

	t.Setenv(envSharedSecret, secret)
	t.Setenv(envPGDSN, "")

	hs := newFakeHS(t, secret)
	hs.send429 = 2
	srv := httptest.NewServer(hs.handler())

	t.Cleanup(srv.Close)

	dir := t.TempDir()
	profile := filepath.Join(dir, "p.json")

	if err := os.WriteFile(profile, []byte(`{"base":"S","users":6,"rooms":3,"rooms_per_user":2,
		"think_time":"40ms","ramp":"200ms","warmup":"400ms","duration":"1500ms","drain":"400ms",
		"sync_timeout":"1s","media_kb":2,"weights":{"send":30,"typing":5,"receipt":5,"messages":5,
		"keys_query":5,"keys_upload":5,"media_upload":5,"media_download":5,"create_room":5,"join_leave":10,"invite":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"-target", srv.URL, "-profile-file", profile, "-token-cache", filepath.Join(dir, "tokens.json"),
		"-metrics-url", srv.URL + "/metrics", "-server-pid", strconv.Itoa(os.Getpid()),
		"-out-dir", filepath.Join(dir, "out"), "-report-name", "first", "-label", "rate_limit=off",
		"-sample-interval", "200ms", "-commit", "test",
	}

	code, err := run(args)
	if err != nil || code != 0 {
		t.Fatalf("первый прогон: код %d, %v", code, err)
	}

	var rep report

	data, err := os.ReadFile(filepath.Join(dir, "out", "first", "report.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}

	if rep.Setup.Registered != 6 || rep.Setup.RoomsCreated != 3 || rep.Target.Kind != "syngo" || rep.Target.ServerHeader != "FakeHS/1" {
		t.Fatalf("подготовка %+v, сервер %+v", rep.Setup, rep.Target)
	}

	if rep.Delivery.Messages == 0 || rep.Delivery.Completeness < 0.99 || rep.Delivery.FromAck.Count == 0 {
		t.Fatalf("доставка %+v", rep.Delivery)
	}

	if rep.Endpoints["sync_longpoll"].Count == 0 || rep.Summary.Actions == 0 || rep.Config["rate_limit"] != "off" {
		t.Fatalf("эндпоинты %v, итог %+v", sortedEndpoints(rep.Endpoints), rep.Summary)
	}

	if rep.Server.Build != "fake abc go" || rep.Process.PID != os.Getpid() || rep.Process.RSSEndKB == 0 || len(rep.Samples) == 0 {
		t.Fatalf("сервер %+v, процесс %+v", rep.Server, rep.Process)
	}

	md, err := os.ReadFile(filepath.Join(dir, "out", "first", "report.md"))
	if err != nil || !strings.Contains(string(md), "## Доставка сообщений") {
		t.Fatalf("report.md: %v", err)
	}

	// Повтор: пользователи из кеша (whoami), комнаты по алиасам, без регистрации.
	for i, v := range args {
		if v == "first" {
			args[i] = "second"
		}
	}

	code, err = run(append(args, "-mode", "rps", "-rps", "40"))
	if err != nil || code != 0 {
		t.Fatalf("второй прогон: код %d, %v", code, err)
	}

	data, _ = os.ReadFile(filepath.Join(dir, "out", "second", "report.json"))

	var rep2 report
	if err := json.Unmarshal(data, &rep2); err != nil {
		t.Fatal(err)
	}

	if rep2.Setup.Cached != 6 || rep2.Setup.RoomsReused != 3 || rep2.Profile.Mode != modeRPS || rep2.Delivery.Messages == 0 {
		t.Fatalf("второй прогон: подготовка %+v, профиль %s, доставка %+v", rep2.Setup, rep2.Profile.Mode, rep2.Delivery)
	}
}

// TestRunRegistrationModes - открытая регистрация (UIA dummy) и вход уже
// существующих; ошибки запуска - код 1.
func TestRunRegistrationModes(t *testing.T) {
	hs := newFakeHS(t, "x")
	srv := httptest.NewServer(hs.handler())

	t.Cleanup(srv.Close)

	e := &engine{
		prof: Profile{TimelineLimit: 20, LazyLoadMembers: true}, target: srv.URL, runID: "r",
		setup: setupOptions{register: regOpen, prefix: "lg", password: "pw"}, cache: &tokenCache{data: map[string]map[string]cachedUser{}},
	}
	e.setupC = &client{base: srv.URL, http: srv.Client(), rec: newRecorder()}

	st := &setupStats{}
	a := newAgent(0, 1)
	a.localpart = "lg-1"

	if err := e.ensureUser(t.Context(), a, st); err != nil || a.token == "" || st.Registered != 1 {
		t.Fatalf("open: %v %+v", err, st)
	}

	// Повторная регистрация - M_USER_IN_USE - вход.
	e.cache = &tokenCache{data: map[string]map[string]cachedUser{}}
	b := newAgent(1, 1)
	b.localpart = "lg-1"

	if err := e.ensureUser(t.Context(), b, st); err != nil || st.LoggedIn != 1 {
		t.Fatalf("in use -> login: %v %+v", err, st)
	}

	// Только вход: неизвестного пользователя нет.
	e.setup.register = regLogin
	c := newAgent(2, 1)
	c.localpart = "nobody"

	if err := e.ensureUser(t.Context(), c, st); err == nil {
		t.Fatal("вход несуществующего")
	}

	e.setup.register = "magic"
	if err := e.ensureUser(t.Context(), c, st); err == nil {
		t.Fatal("неизвестный способ регистрации")
	}

	// Протухший токен в кеше - заново (whoami 401).
	e.setup.register = regOpen
	e.cache.put(srv.URL, "lg-2", cachedUser{UserID: "@lg-2:" + fakeServer, Token: "stale"})

	d := newAgent(3, 1)
	d.localpart = "lg-2"

	if err := e.ensureUser(t.Context(), d, st); err != nil || d.token == "stale" {
		t.Fatalf("протухший токен: %v, %s", err, d.token)
	}

	for _, args := range [][]string{
		{"-profile", "XXL"},
		{"-register", "shared-secret"}, // без LOADGEN_SHARED_SECRET
		{"-users", "1"},
		{"-bogus"},
	} {
		t.Setenv(envSharedSecret, "")

		if code, err := run(args); code != 1 || err == nil {
			t.Errorf("%v: код %d, %v", args, code, err)
		}
	}

	if code, err := run([]string{"-h"}); code != 0 || err != nil {
		t.Fatalf("-h: %d %v", code, err)
	}
}
