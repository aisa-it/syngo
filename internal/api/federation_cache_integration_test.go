//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// testKeyFetcher - получение ключей по http с разрешенным loopback (httptest);
// prepare настраивает резолвер (well-known, SRV).
func testKeyFetcher(prepare func(*transport.Resolver)) *transport.KeyFetcher {
	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.LookupSRV = func(context.Context, string, string, string) ([]*net.SRV, error) {
		return nil, net.UnknownNetworkError("no srv")
	}

	if prepare != nil {
		prepare(resolver)
	}

	return &transport.KeyFetcher{Client: transport.NewClient(transport.Options{
		Resolver: resolver, Scheme: "http", AddressPolicy: transport.AllowAllAddresses(),
	})}
}

// signedKeysFor - ответ /_matrix/key/v2/server сервера serverName.
func signedKeysFor(t *testing.T, serverName string, key signing.Key, old map[string]any) map[string]any {
	t.Helper()

	object := map[string]any{
		"server_name":     serverName,
		"valid_until_ts":  time.Now().Add(time.Hour).UnixMilli(),
		"verify_keys":     map[string]any{key.KeyID: map[string]any{"key": key.Public()}},
		"old_verify_keys": old,
	}

	signed, err := signing.SignJSON(key, serverName, object)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return signed
}

// TestServerKeysDelegatedWellKnown - входящий запрос сервера с
// делегированием через well-known проходит проверку подписи - ключи берутся у
// delegated-адреса через резолвер (раньше - origin:8448 напрямую, 401).
func TestServerKeysDelegatedWellKnown(t *testing.T) {
	server := newIntegrationServer(t)

	remoteKey, err := signing.Generate("deleg_1")
	if err != nil {
		t.Fatal(err)
	}

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/key/v2/server" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_ = json.NewEncoder(w).Encode(signedKeysFor(t, "deleg.test", remoteKey, nil))
	}))
	defer keyServer.Close()

	wellKnown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"m.server": strings.TrimPrefix(keyServer.URL, "http://")})
	}))
	defer wellKnown.Close()

	server.keyFetcher = testKeyFetcher(func(resolver *transport.Resolver) {
		resolver.WellKnownURL = func(_, hostname string) string {
			if hostname != "deleg.test" {
				return "http://127.0.0.1:1/"
			}

			return wellKnown.URL + "/.well-known/matrix/server"
		}
	})

	const path = "/_matrix/federation/v1/version"

	header, err := signing.MatrixAuthHeader(remoteKey, "deleg.test", server.cfg.Matrix.ServerName, http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", header)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("запрос делегированного сервера: %d %s", rec.Code, rec.Body.String())
	}
}

// TestServerKeysOldVerifyKeys - ключ из old_verify_keys находится и
// действует до expired_ts: событие, подписанное до него, проверяется.
func TestServerKeysOldVerifyKeys(t *testing.T) {
	server := newIntegrationServer(t)

	current, err := signing.Generate("new_1")
	if err != nil {
		t.Fatal(err)
	}

	oldKey, err := signing.Generate("old_1")
	if err != nil {
		t.Fatal(err)
	}

	expired := time.Now().Add(-time.Hour).UnixMilli()

	var remote *httptest.Server

	remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origin := strings.TrimPrefix(remote.URL, "http://")

		_ = json.NewEncoder(w).Encode(signedKeysFor(t, origin, current, map[string]any{
			oldKey.KeyID: map[string]any{"key": oldKey.Public(), "expired_ts": expired},
		}))
	}))
	defer remote.Close()

	origin := strings.TrimPrefix(remote.URL, "http://")
	server.keyFetcher = testKeyFetcher(nil)

	publicKey, validUntil, err := server.lookupServerKeyCtx(context.Background(), origin, oldKey.KeyID)
	if err != nil || publicKey != oldKey.Public() || validUntil != expired {
		t.Fatalf("old key: %q %d %v", publicKey, validUntil, err)
	}

	sign := func(ts int64) map[string]any {
		signed, err := signing.SignJSON(oldKey, origin, map[string]any{"type": "m.room.message", "origin_server_ts": ts})
		if err != nil {
			t.Fatal(err)
		}

		raw, _ := json.Marshal(signed)

		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		return decoded
	}

	lookup := server.federationKeyLookup(context.Background())

	if err := signing.VerifyJSON(sign(expired-1000), origin, lookup); err != nil {
		t.Fatalf("событие до expired_ts: %v", err)
	}

	if err := signing.VerifyJSON(sign(time.Now().UnixMilli()), origin, lookup); err == nil {
		t.Fatal("событие после expired_ts старым ключом должно отвергаться")
	}
}

// TestServerKeysCache — ключи кэшируются, повторный запрос не ходит в сеть
// до истечения TTL; self-подпись чужого ответа проверяется.
func TestServerKeysCache(t *testing.T) {
	server := newIntegrationServer(t)

	remoteKey, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("generate remote key: %v", err)
	}

	var hits atomic.Int32

	var remote *httptest.Server

	remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/key/v2/server" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		hits.Add(1)

		// В федерации origin = server_name, поэтому отдаём имя тестового хоста.
		origin := strings.TrimPrefix(remote.URL, "http://")

		object := map[string]any{
			"server_name":    origin,
			"valid_until_ts": int64(4_000_000_000_000), // далеко в будущем
			"verify_keys":    map[string]any{remoteKey.KeyID: map[string]any{"key": remoteKey.Public()}},
		}

		signed, err := signing.SignJSON(remoteKey, origin, object)
		if err != nil {
			t.Errorf("sign: %v", err)

			return
		}

		_ = json.NewEncoder(w).Encode(signed)
	}))
	defer remote.Close()

	origin := strings.TrimPrefix(remote.URL, "http://")
	server.keyFetcher = testKeyFetcher(nil)

	// Первый запрос — идём в сеть и кэшируем.
	publicKey, validUntil, err := server.lookupServerKeyCtx(context.Background(), origin, remoteKey.KeyID)
	if err != nil {
		t.Fatalf("lookup 1: %v", err)
	}

	if publicKey != remoteKey.Public() {
		t.Fatalf("public key не совпал")
	}

	if validUntil == 0 {
		t.Fatal("valid_until_ts не сохранён")
	}

	if hits.Load() != 1 {
		t.Fatalf("сетевых запросов %d, ожидали 1", hits.Load())
	}

	// Второй — из кэша, без сети.
	if _, _, err := server.lookupServerKeyCtx(context.Background(), origin, remoteKey.KeyID); err != nil {
		t.Fatalf("lookup 2: %v", err)
	}

	if hits.Load() != 1 {
		t.Fatalf("повторный lookup сходил в сеть (hits=%d)", hits.Load())
	}

	// Подделанная подпись удалённого сервера — отказ.
	var badRemote *httptest.Server

	badRemote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badOrigin := strings.TrimPrefix(badRemote.URL, "http://")

		object := map[string]any{
			"server_name":    badOrigin,
			"valid_until_ts": int64(4_000_000_000_000),
			"verify_keys":    map[string]any{"ed25519:a_x": map[string]any{"key": remoteKey.Public()}},
			"signatures":     map[string]any{badOrigin: map[string]any{"ed25519:a_x": "AAAA"}},
		}

		_ = json.NewEncoder(w).Encode(object)
	}))
	defer badRemote.Close()

	badOrigin := strings.TrimPrefix(badRemote.URL, "http://")

	if _, _, err := server.lookupServerKeyCtx(context.Background(), badOrigin, "ed25519:a_x"); err == nil {
		t.Fatal("поддельная подпись ответа сервера должна отвергаться")
	}
}
