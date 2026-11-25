//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestOwnEventsResigned — наши события уходят с подписью, посчитанной
// уже с hashes, даже если в БД лежит устаревшая подпись (без hashes).
func TestOwnEventsResigned(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!resign:test.local"
	env.seedRoom(t, roomID)
	// /event отдается только серверу с участником комнаты.
	setRemoteMember(t, env.server, roomID, "@bob:remote.test", "join")

	ctx := t.Context()

	// Портим сохранённую подпись create-события: имитируем старую подпись без hashes.
	if _, err := env.server.db.Pool().Exec(ctx,
		`UPDATE events SET signatures = '{"test.local":{"ed25519:a_1":"stale"}}' WHERE event_id=$1`,
		"$create:test.local"); err != nil {
		t.Fatalf("update signatures: %v", err)
	}

	rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/event/$create:test.local", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("event: %d %s", rec.Code, rec.Body.String())
	}

	// Формат ответа: объект {origin, pdus, auth_chain} (обёртку [200, …] убрали).
	var payload struct {
		PDUs []map[string]any `json:"pdus"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if len(payload.PDUs) != 1 {
		t.Fatalf("pdus = %d", len(payload.PDUs))
	}

	event := payload.PDUs[0]

	if _, ok := event["hashes"]; !ok {
		t.Fatal("в PDU нет hashes")
	}

	sigs, _ := event["signatures"].(map[string]any)
	ours, _ := sigs["test.local"].(map[string]any)

	if len(ours) == 0 {
		t.Fatalf("нет подписи нашего сервера: %v", sigs)
	}

	if ours["ed25519:a_1"] == "stale" {
		t.Fatal("отдали устаревшую подпись вместо пересчёта")
	}

	if err := signing.VerifyJSON(event, "test.local", func(origin, keyID string) (string, int64, error) {
		if origin == "test.local" && env.server.federationKey != nil {
			return env.server.federationKey.Public(), 0, nil
		}

		return "", 0, signing.ErrInvalidKey
	}); err != nil {
		t.Fatalf("подпись не сходится: %v", err)
	}
}
