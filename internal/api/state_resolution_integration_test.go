//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestFederationStateResolutionApplied — при конфликте state-событий
// room_state материализуется по результату v2 (mainline-порядок), а не «кто последний».
func TestFederationStateResolutionApplied(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!resolved:test.local"
	env.seedRoom(t, roomID)

	// Начальное состояние комнаты: alice join + топик уже есть (current).
	ctx := context.Background()

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ('$alicejoin',$1,'m.room.member','@alice:test.local','@alice:test.local','{"membership":"join"}',3,30,'test.local',3)`, roomID); err != nil {
		t.Fatalf("insert alice join: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,'m.room.member','@alice:test.local','$alicejoin')
		ON CONFLICT (room_id, type, state_key) DO UPDATE SET event_id = EXCLUDED.event_id`, roomID); err != nil {
		t.Fatalf("insert alice state: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		VALUES ('$topic-old',$1,'m.room.topic','@alice:test.local','','{"topic":"old"}',4,40,'test.local',4)`, roomID); err != nil {
		t.Fatalf("insert old topic: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1,'m.room.topic','','$topic-old')`, roomID); err != nil {
		t.Fatalf("insert state: %v", err)
	}

	// Событие подписываем «чужим» ключом, но отправитель — наш участник (alice),
	// иначе auth rules отвергнут state-событие от не-участника.
	env.server.keyLookup = func(origin, keyID string) (string, int64, error) {
		if origin == "remote.test" && keyID == env.remoteKey.KeyID {
			return env.remoteKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		if origin == "test.local" && env.server.federationKey != nil && keyID == env.server.federationKey.KeyID {
			return env.server.federationKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		return "", 0, signing.ErrInvalidKey
	}

	// event_id считается по содержимому: явный в PDU не передается.
	object := map[string]any{
		"room_id": roomID, "type": "m.room.topic", "state_key": "", "sender": "@alice:test.local",
		"content": map[string]any{"topic": "new"}, "depth": 5, "origin_server_ts": 1_700_000_000_000,
		"prev_events": []string{"$pl:test.local"}, "auth_events": []string{"$create:test.local", "$pl:test.local", "$alicejoin"},
	}

	// Подпись — нашим ключом (отправитель наш), поэтому подписываем от test.local.
	signed, err := eventjson.Sign(*env.server.federationKey, "test.local", withContentHash(t, object))
	if err != nil {
		t.Fatalf("sign topic: %v", err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal topic: %v", err)
	}

	var topicNew map[string]any
	if err := json.Unmarshal(raw, &topicNew); err != nil {
		t.Fatalf("unmarshal topic: %v", err)
	}

	topicID, err := core.EventID(topicNew, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	// Событие alice пересылает ее же сервер: событие чужого сервера в /send
	// принимается только как join/invite (Synapse).
	request := func() *httptest.ResponseRecorder {
		return env.requestAs(t, "test.local", *env.server.federationKey, http.MethodPut,
			"/_matrix/federation/v1/send/txn-res", map[string]any{"origin": "test.local", "pdus": []any{topicNew}})
	}

	rec := request()

	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	var eventID string
	if err := env.server.db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id=$1 AND type='m.room.topic' AND state_key=''", roomID).Scan(&eventID); err != nil {
		t.Fatalf("room_state: %v", err)
	}

	if eventID != topicID {
		t.Fatalf("room_state topic = %q, ожидали %s", eventID, topicID)
	}

	// Повторный /sync того же события не меняет состояние (идемпотентность материализации).
	if rec := request(); rec.Code != http.StatusOK {
		t.Fatalf("повтор send: %d", rec.Code)
	}

	var again string
	if err := env.server.db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id=$1 AND type='m.room.topic' AND state_key=''", roomID).Scan(&again); err != nil {
		t.Fatalf("room_state after repeat: %v", err)
	}

	if again != topicID {
		t.Fatalf("после повтора room_state topic = %q", again)
	}
}
