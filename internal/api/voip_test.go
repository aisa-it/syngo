package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
)

// TestTurnServer — креды по схеме coturn отдаются только с токеном.
// Раньше роут стоял вне requireAuth, сессии не было никогда, и при
// настроенном TURN клиент получал пустой объект.
func TestTurnServer(t *testing.T) {
	t.Parallel()

	srv := newAuthServer(t, core.AuthOptions{})
	srv.cfg.VoIP = config.VoIP{
		URIs:   []string{"turn:turn.test:3478"},
		Secret: "turn-secret",
		TTL:    config.Duration(time.Hour),
	}

	alice := registerUser(t, srv, "alice", "1234567890")

	// Без токена — 401.
	rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/voip/turnServer", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d %s, ожидали 401", rec.Code, rec.Body.String())
	}

	// С токеном — креды.
	rec = do(t, srv, http.MethodGet, "/_matrix/client/v3/voip/turnServer", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("с токеном: %d %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		URIs     []string `json:"uris"`
		Username string   `json:"username"`
		Password string   `json:"password"`
		TTL      int      `json:"ttl"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(payload.URIs) != 1 || payload.TTL != 3600 {
		t.Fatalf("ответ = %+v", payload)
	}

	if payload.Username == "" || payload.Password == "" {
		t.Fatalf("пустые креденшелы: %+v", payload)
	}

	// Без настроенного TURN — 200 и пустой объект (клиент это переживает).
	empty := newAuthServer(t, core.AuthOptions{})
	bob := registerUser(t, empty, "bob", "1234567890")

	rec = do(t, empty, http.MethodGet, "/_matrix/client/v3/voip/turnServer", "", bob.AccessToken)
	if rec.Code != http.StatusOK || rec.Body.String() != "{}\n" {
		t.Fatalf("без конфигурации: %d %s", rec.Code, rec.Body.String())
	}
}
