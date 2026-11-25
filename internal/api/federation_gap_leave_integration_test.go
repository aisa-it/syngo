//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// resign подписывает объект заново после изменения полей.
func (e *fedEnv) resign(t *testing.T, object map[string]any) map[string]any {
	t.Helper()

	signed, err := eventjson.Sign(e.remoteKey, "remote.test", withContentHash(t, object))
	if err != nil {
		t.Fatalf("resign: %v", err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal resigned: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal resigned: %v", err)
	}

	return generic
}

// withContentHash ставит hashes.sha256 события по его текущему содержимому
// (входящий PDU без content hash не принимается).
func withContentHash(t *testing.T, object map[string]any) map[string]any {
	t.Helper()

	delete(object, "hashes")

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatalf("content hash: %v", err)
	}

	object["hashes"] = map[string]any{"sha256": hash}

	return object
}

// setTransport направляет исходящий транспорт на httptest-сервер.
func (e *fedEnv) setTransport(serverURL string) {
	host := strings.TrimPrefix(serverURL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: host, Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &e.remoteKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	})

	e.server.SetFederationTransport(client)
}

// TestIncomingLeave — S2S leave: чужой пользователь выходит из нашей комнаты.
func TestIncomingLeave(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!leave:test.local"
	env.seedRoom(t, roomID)

	// Сначала join, чтобы leave был авторизован.
	joinPDU, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/txn-join", map[string]any{
		"origin": "remote.test", "pdus": []any{joinPDU},
	}); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	// event_id считается по содержимому, явный в PDU не передается.
	leavePDU, leaveID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@bob:remote.test"),
		map[string]any{"membership": "leave"}, []string{"$create:test.local", "$pl:test.local", joinID},
		[]string{joinID}, 5)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send_leave/"+roomID+"/"+leaveID, leavePDU)
	if rec.Code != http.StatusOK {
		t.Fatalf("send_leave: %d %s", rec.Code, rec.Body.String())
	}

	var membership string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		"SELECT membership FROM room_memberships WHERE room_id=$1 AND user_id='@bob:remote.test'", roomID).Scan(&membership); err != nil {
		t.Fatalf("membership: %v", err)
	}

	if membership != "leave" {
		t.Fatalf("membership = %q, ожидали leave", membership)
	}
}

// TestOutgoingLeave — наш пользователь выходит из чужой комнаты через make_leave/send_leave.
func TestOutgoingLeave(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!remoteleave:remote.test"

	ctx := context.Background()

	// Локально уже есть комната и наш membership join (как после исходящего join).
	if _, err := env.server.db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1,'@bob:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ('$joinours:test.local',$1,'m.room.member','@bob:test.local','@bob:test.local','{"membership":"join"}',1,1,'test.local',1)`, roomID); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		VALUES ('$joinours:test.local',$1,'@bob:test.local','@bob:test.local','join')`, roomID); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	var sent atomic.Int32

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "make_leave"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"room_version": "10",
				"event": map[string]any{
					"event_id": "$leaveout:test.local", "type": "m.room.member",
					"content": map[string]any{"membership": "leave"}, "depth": float64(2),
					"prev_events": []string{"$joinours:test.local"}, "auth_events": []string{},
				},
			})
		case strings.Contains(r.URL.Path, "send_leave"):
			sent.Add(1)
			_ = json.NewEncoder(w).Encode([]any{200, map[string]any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer remote.Close()

	env.setTransport(remote.URL)

	// Joiner поверх того же транспорта (в проде его собирает main).
	host := strings.TrimPrefix(remote.URL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: host, Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: env.server.federationKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	})
	env.server.SetRemoteJoiner(join.New(client, env.server.db, env.server.federationKey, "test.local"))

	// Клиент нашего пользователя выходит из чужой комнаты.
	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", "", env.localUserToken(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	if sent.Load() != 1 {
		t.Fatalf("send_leave вызван %d раз, ожидали 1", sent.Load())
	}

	var membership string
	if err := env.server.db.Pool().QueryRow(ctx,
		"SELECT membership FROM room_memberships WHERE room_id=$1 AND user_id='@bob:test.local'", roomID).Scan(&membership); err != nil {
		t.Fatalf("membership: %v", err)
	}

	if membership != "leave" {
		t.Fatalf("membership = %q, ожидали leave", membership)
	}

	_ = time.Second
}

// localUserToken регистрирует нашего пользователя и возвращает токен.
func (e *fedEnv) localUserToken(t *testing.T) string {
	t.Helper()

	return registerHTTP(t, e.server, "bob", "BOB")
}
