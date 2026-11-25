//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// fedMock - удаленный сервер remote.test: обработчик по пути и журнал запросов.
type fedMock struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string][]byte
	handle   func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFedMock(t *testing.T, env *fedEnv, handle func(w http.ResponseWriter, r *http.Request, body []byte)) *fedMock {
	t.Helper()

	mock := &fedMock{bodies: map[string][]byte{}, handle: handle}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mock.mu.Lock()
		mock.requests = append(mock.requests, r.Method+" "+r.URL.Path)
		mock.bodies[r.URL.Path] = body
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		mock.handle(w, r, body)
	}))
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: host, Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: env.server.federationKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	})

	env.server.SetFederationTransport(client)
	env.server.SetRemoteJoiner(join.New(client, env.server.db, env.server.federationKey, "test.local"))

	return mock
}

func (m *fedMock) paths() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return strings.Join(m.requests, " ")
}

func membershipRow(t *testing.T, env *fedEnv, roomID, userID string) string {
	t.Helper()

	var membership string

	_ = env.server.db.Pool().QueryRow(context.Background(),
		`SELECT membership FROM room_memberships WHERE room_id = $1 AND user_id = $2`, roomID, userID).Scan(&membership)

	return membership
}

// remoteRoomWith заводит локально комнату remote.test с членством нашего
// пользователя (как после входящего invite или исходящего join).
func remoteRoomWith(t *testing.T, env *fedEnv, roomID, userID, membership string) {
	t.Helper()

	ctx := context.Background()

	if _, err := env.server.db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:remote.test')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	statements := []string{
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		 VALUES ('$rcreate', $1, 'm.room.create', '@alice:remote.test', '', '{"creator":"@alice:remote.test","room_version":"10"}', 1, 1, 'remote.test')`,
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		 VALUES ('$ralice', $1, 'm.room.member', '@alice:remote.test', '@alice:remote.test', '{"membership":"join"}', 2, 2, 'remote.test')`,
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		 VALUES ('$rjr', $1, 'm.room.join_rules', '@alice:remote.test', '', '{"join_rule":"invite"}', 3, 3, 'remote.test')`,
	}

	for _, statement := range statements {
		if _, err := env.server.db.Pool().Exec(ctx, statement, roomID); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	sender := userID
	content := `{"membership":"join"}`

	if membership == "invite" {
		sender = "@alice:remote.test"
		content = `{"membership":"invite"}`
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin,
		                    prev_events, auth_events)
		VALUES ('$rours', $1, 'm.room.member', $2, $3, $4, 4, 4, $5, '{$rjr}', '{$rcreate,$ralice,$rjr}')`,
		roomID, sender, userID, content, domainOfID(sender)); err != nil {
		t.Fatalf("our membership: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id)
		SELECT room_id, type, state_key, event_id FROM events WHERE room_id = $1`, roomID); err != nil {
		t.Fatalf("state: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		SELECT event_id, room_id, state_key, sender, content->>'membership' FROM events
		WHERE room_id = $1 AND type = 'm.room.member'`, roomID); err != nil {
		t.Fatalf("memberships: %v", err)
	}
}

// TestRejectInviteRemoteUnavailable - отказ от приглашения, когда сервер
// комнаты недоступен: пишется локальный leave поверх invite, клиент получает
// 200 (out-of-band rejection, как Synapse). Раньше - 502 и приглашение навсегда.
func TestRejectInviteRemoteUnavailable(t *testing.T) {
	env := newFedEnv(t)
	token := env.localUserToken(t)
	roomID := "!invited:remote.test"

	remoteRoomWith(t, env, roomID, "@bob:test.local", "invite")

	mock := newFedMock(t, env, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(mock.paths(), "make_leave") {
		t.Fatalf("make_leave не запрошен: %s", mock.paths())
	}

	if got := membershipRow(t, env, roomID, "@bob:test.local"); got != "leave" {
		t.Fatalf("membership = %q, ожидали leave", got)
	}

	var prev, auth []string
	if err := env.server.db.Pool().QueryRow(context.Background(), `
		SELECT e.prev_events, e.auth_events FROM room_memberships rm JOIN events e ON e.event_id = rm.event_id
		WHERE rm.room_id = $1 AND rm.user_id = '@bob:test.local'`, roomID).Scan(&prev, &auth); err != nil {
		t.Fatalf("leave event: %v", err)
	}

	if len(prev) != 1 || prev[0] != "$rours" || len(auth) != 1 || auth[0] != "$rours" {
		t.Fatalf("out-of-band leave: prev=%v auth=%v, ожидали invite", prev, auth)
	}
}

// TestLeaveJoinedRemoteUnavailable - выход из комнаты, где мы состоим, при
// недоступном сервере комнаты: обычное локальное событие, уходящее участникам
// через outbox. Раньше - 502 и пользователь оставался в комнате.
func TestLeaveJoinedRemoteUnavailable(t *testing.T) {
	env := newFedEnv(t)
	token := env.localUserToken(t)
	roomID := "!joined:remote.test"

	remoteRoomWith(t, env, roomID, "@bob:test.local", "join")

	newFedMock(t, env, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	if got := membershipRow(t, env, roomID, "@bob:test.local"); got != "leave" {
		t.Fatalf("membership = %q, ожидали leave", got)
	}

	var eventID string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT event_id FROM room_memberships WHERE room_id = $1 AND user_id = '@bob:test.local'`, roomID).
		Scan(&eventID); err != nil {
		t.Fatalf("leave event: %v", err)
	}

	if got := outboxFor(t, env.server, eventID); len(got) != 1 || got[0] != "remote.test" {
		t.Fatalf("leave в outbox: %v, ожидали [remote.test]", got)
	}
}

// TestOutgoingLeavePDU - PDU send_leave собирается по make_leave как у join:
// версия комнаты из ответа (v11), hashes, event_id - reference hash по
// правилам версии, только в URL; без event_id и origin в теле. Раньше leave
// уходил без hashes, с event_id и origin, по правилам v10.
func TestOutgoingLeavePDU(t *testing.T) {
	env := newFedEnv(t)
	token := env.localUserToken(t)
	roomID := "!v11:remote.test"

	remoteRoomWith(t, env, roomID, "@bob:test.local", "join")

	mock := newFedMock(t, env, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.Contains(r.URL.Path, "/make_leave/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"room_version": "11",
				"event": map[string]any{
					"type": "m.room.member", "room_id": roomID, "sender": "@bob:test.local",
					"state_key": "@bob:test.local", "origin": "remote.test", "event_id": "$fromtemplate",
					"content": map[string]any{"membership": "leave"}, "depth": 9, "origin_server_ts": 1,
					"prev_events": []string{"$ralice"}, "auth_events": []string{"$rcreate", "$rours"},
				},
			})
		case strings.Contains(r.URL.Path, "/v2/send_leave/"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()

	var (
		sentPath string
		sent     map[string]any
	)

	for path, body := range mock.bodies {
		if strings.Contains(path, "/v2/send_leave/") {
			sentPath = path
			_ = json.Unmarshal(body, &sent)
		}
	}

	if sent == nil {
		t.Fatalf("send_leave v2 не вызван: %v", mock.requests)
	}

	for _, field := range []string{"event_id", "origin"} {
		if _, ok := sent[field]; ok {
			t.Fatalf("%s в теле send_leave: %v", field, sent)
		}
	}

	if _, ok := sent["hashes"]; !ok {
		t.Fatalf("в send_leave нет hashes: %v", sent)
	}

	id, err := eventjson.EventID(sent, 11)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	if !strings.HasSuffix(sentPath, "/"+id) {
		t.Fatalf("event_id в URL %s, reference hash v11 %s", sentPath, id)
	}

	if err := eventjson.VerifyVersioned(sent, "test.local", 11, env.server.federationKeyLookup(context.Background())); err != nil {
		t.Fatalf("подпись leave по правилам v11: %v", err)
	}

	if got := membershipRow(t, env, roomID, "@bob:test.local"); got != "leave" {
		t.Fatalf("membership = %q, ожидали leave", got)
	}
}
