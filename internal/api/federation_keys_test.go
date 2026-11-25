package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// newFederationServerWith - сервер федерации без БД с правкой конфига до New.
func newFederationServerWith(t *testing.T, mutate func(*config.Config)) (*Server, signing.Key) {
	t.Helper()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.Federation.Enabled = true
	cfg.Federation.ValidUntilDays = 7
	cfg.RateLimit = config.RateLimit{}

	if mutate != nil {
		mutate(&cfg)
	}

	server := New(cfg, nil, nil)
	server.SetFederationKey(&key)

	return server, key
}

// TestKeyServerPublishesOldVerifyKeys - old_verify_keys публикуются из
// federation.old_signing_keys (раньше всегда {}), срок - не более 7 дней.
func TestKeyServerPublishesOldVerifyKeys(t *testing.T) {
	t.Parallel()

	oldKey, err := signing.Generate("old_1")
	if err != nil {
		t.Fatal(err)
	}

	server, _ := newFederationServerWith(t, func(cfg *config.Config) {
		cfg.Federation.ValidUntilDays = 30
		cfg.Federation.OldSigningKeys = map[string]config.OldSigningKey{
			oldKey.KeyID: {Key: oldKey.Public(), ExpiredTS: 1_600_000_000_000},
		}
	})

	for _, path := range []string{"/_matrix/key/v2/server", "/_matrix/key/v2/server/" + oldKey.KeyID} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}

		var body struct {
			ValidUntilTS  int64 `json:"valid_until_ts"`
			OldVerifyKeys map[string]struct {
				Key       string `json:"key"`
				ExpiredTS int64  `json:"expired_ts"`
			} `json:"old_verify_keys"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}

		old, ok := body.OldVerifyKeys[oldKey.KeyID]
		if !ok || old.Key != oldKey.Public() || old.ExpiredTS != 1_600_000_000_000 {
			t.Fatalf("%s: old_verify_keys = %+v", path, body.OldVerifyKeys)
		}

		if limit := time.Now().Add(7*24*time.Hour + time.Minute).UnixMilli(); body.ValidUntilTS > limit {
			t.Fatalf("valid_until_ts дальше 7 дней: %d", body.ValidUntilTS)
		}
	}
}

// signedFederationRequest - запрос, подписанный key от имени origin.
func signedFederationRequest(t *testing.T, key signing.Key, origin, remoteAddr string) *http.Request {
	t.Helper()

	const path = "/_matrix/federation/v1/version"

	header, err := signing.MatrixAuthHeader(key, origin, "test.local", http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", header)
	req.RemoteAddr = remoteAddr

	return req
}

// TestFederationAllowHostsIncoming - allow_hosts отвергает входящий
// запрос чужого сервера (403) до запроса его ключей.
func TestFederationAllowHostsIncoming(t *testing.T) {
	t.Parallel()

	server, key := newFederationServerWith(t, func(cfg *config.Config) {
		cfg.Federation.AllowHosts = []string{"good.test"}
		cfg.Federation.DenyHosts = []string{"bad.good.test"}
	})

	var lookups atomic.Int32

	server.keyLookup = func(string, string) (string, int64, error) {
		lookups.Add(1)

		return key.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
	}

	cases := map[string]int{
		"good.test":     http.StatusOK,
		"evil.test":     http.StatusForbidden,
		"bad.good.test": http.StatusForbidden,
	}

	for origin, want := range cases {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, signedFederationRequest(t, key, origin, "198.51.100.1:1"))

		if rec.Code != want {
			t.Fatalf("%s: %d %s, ожидали %d", origin, rec.Code, rec.Body.String(), want)
		}
	}

	if lookups.Load() != 1 {
		t.Fatalf("ключи запрошены %d раз, ожидали только для good.test", lookups.Load())
	}

	// origin не по грамматике server_name - 401 без запроса ключей.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, signedFederationRequest(t, key, "good.test/x", "198.51.100.1:1"))

	if rec.Code != http.StatusUnauthorized || lookups.Load() != 1 {
		t.Fatalf("невалидный origin: %d, lookups %d", rec.Code, lookups.Load())
	}
}

// TestFederationRateLimitBeforeSignatureByIP - до проверки подписи
// лимит - по IP: поток запросов с поддельным origin не выбивает 429 настоящему
// серверу; после подписи - лимит по origin.
func TestFederationRateLimitBeforeSignatureByIP(t *testing.T) {
	t.Parallel()

	server, key := newFederationServerWith(t, func(cfg *config.Config) {
		cfg.Federation.IncomingRatePerSecond = 0.001
		cfg.Federation.IncomingBurst = 2
	})

	forger, err := signing.Generate("a_1")
	if err != nil {
		t.Fatal(err)
	}

	server.keyLookup = func(string, string) (string, int64, error) {
		return key.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
	}

	// Атакующий шлет запросы от имени remote.test чужим ключом.
	for range 3 {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, signedFederationRequest(t, forger, "remote.test", "198.51.100.66:1"))

		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("поддельный запрос: %d", rec.Code)
		}
	}

	// Настоящий remote.test со своего IP проходит.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, signedFederationRequest(t, key, "remote.test", "203.0.113.5:1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("настоящий сервер: %d %s", rec.Code, rec.Body.String())
	}

	// Лимит по проверенному origin: с разных IP, но тем же origin.
	codes := make([]int, 0, 2)

	for i := range 2 {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, signedFederationRequest(t, key, "remote.test", "203.0.113."+strconv.Itoa(6+i)+":1"))
		codes = append(codes, rec.Code)
	}

	if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
		t.Fatalf("лимит по origin после подписи: %v", codes)
	}
}
