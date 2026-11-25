package transport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// signedServerKeys - ответ /_matrix/key/v2/server, подписанный signers.
func signedServerKeys(t *testing.T, serverName string, current signing.Key, validUntil int64, old map[string]any, signers ...signing.Key) map[string]any {
	t.Helper()

	object := map[string]any{
		"server_name":     serverName,
		"verify_keys":     map[string]any{current.KeyID: map[string]any{"key": current.Public()}},
		"old_verify_keys": old,
	}

	if validUntil != 0 {
		object["valid_until_ts"] = validUntil
	}

	if len(signers) == 0 {
		signers = []signing.Key{current}
	}

	return signAs(t, object, serverName, signers...)
}

// signAs добавляет подписи signer к объекту (не затирая чужие).
func signAs(t *testing.T, object map[string]any, signer string, keys ...signing.Key) map[string]any {
	t.Helper()

	existing, _ := object["signatures"].(map[string]any)
	delete(object, "signatures")

	canonical, err := signing.CanonicalJSON(object)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	sigs := map[string]any{}
	for _, key := range keys {
		sigs[key.KeyID] = key.Sign(canonical)
	}

	if existing == nil {
		existing = map[string]any{}
	}

	existing[signer] = sigs
	object["signatures"] = existing

	return object
}

func mustKey(t *testing.T, version string) signing.Key {
	t.Helper()

	key, err := signing.Generate(version)
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	return key
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json: %v", err)
	}

	return raw
}

// TestParseServerKeys - old_verify_keys разбираются со своим
// expired_ts, valid_until_ts ограничен 7 днями и обязателен, подписи сервера
// ключами verify_keys проверяются все.
func TestParseServerKeys(t *testing.T) {
	t.Parallel()

	now := time.UnixMilli(1_700_000_000_000)
	current := mustKey(t, "new")
	oldKey := mustKey(t, "old")
	far := now.Add(365 * 24 * time.Hour).UnixMilli()

	raw := mustJSON(t, signedServerKeys(t, "remote.test", current, far, map[string]any{
		oldKey.KeyID: map[string]any{"key": oldKey.Public(), "expired_ts": int64(1_600_000_000_000)},
	}))

	keys, err := transport.ParseServerKeys(raw, "remote.test", now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if want := now.Add(transport.MaxKeyValidity).UnixMilli(); keys.ValidUntilTS != want {
		t.Fatalf("valid_until_ts = %d, ожидали не более 7 дней (%d)", keys.ValidUntilTS, want)
	}

	if key, until, ok := keys.Lookup(oldKey.KeyID); !ok || key != oldKey.Public() || until != 1_600_000_000_000 {
		t.Fatalf("old_verify_keys: %q %d %v", key, until, ok)
	}

	if key, until, ok := keys.Lookup(current.KeyID); !ok || key != current.Public() || until != keys.ValidUntilTS {
		t.Fatalf("verify_keys: %q %d %v", key, until, ok)
	}

	// Без valid_until_ts (раньше 0 означал «вечно») - отказ.
	if _, err := transport.ParseServerKeys(mustJSON(t, signedServerKeys(t, "remote.test", current, 0, nil)), "remote.test", now); !errors.Is(err, transport.ErrBadKeyResponse) {
		t.Fatalf("без valid_until_ts: %v", err)
	}

	// Чужой server_name - отказ.
	if _, err := transport.ParseServerKeys(raw, "other.test", now); !errors.Is(err, transport.ErrBadKeyResponse) {
		t.Fatalf("server_name: %v", err)
	}

	// Одна из подписей ключом из verify_keys неверна - отказ, даже если другая верна.
	second := mustKey(t, "second")
	object := map[string]any{
		"server_name":    "remote.test",
		"valid_until_ts": far,
		"verify_keys": map[string]any{
			current.KeyID: map[string]any{"key": current.Public()},
			second.KeyID:  map[string]any{"key": second.Public()},
		},
	}
	object = signAs(t, object, "remote.test", current, second)
	object["signatures"].(map[string]any)["remote.test"].(map[string]any)[second.KeyID] = current.Sign([]byte("x"))

	if _, err := transport.ParseServerKeys(mustJSON(t, object), "remote.test", now); err == nil {
		t.Fatal("неверная подпись одним из verify_keys должна отвергаться")
	}

	// Подпись только ключом, которого нет в verify_keys, - отказ.
	stranger := mustKey(t, "x")
	if _, err := transport.ParseServerKeys(mustJSON(t, signedServerKeys(t, "remote.test", current, far, nil, stranger)), "remote.test", now); err == nil {
		t.Fatal("подпись не из verify_keys должна отвергаться")
	}
}

// keyResolver - резолвер, у которого все server_name ведут на httptest.
func keyResolver(host string) *transport.Resolver {
	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(_ context.Context, name string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: name, Host: host, Scheme: "http"}}, nil
	}

	return resolver
}

// TestKeyFetcherDelegatedWellKnown - ключи сервера с делегированием
// через well-known берутся у delegated-адреса (раньше - напрямую origin:8448).
func TestKeyFetcherDelegatedWellKnown(t *testing.T) {
	t.Parallel()

	remoteKey := mustKey(t, "a_1")

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/key/v2/server" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_ = json.NewEncoder(w).Encode(signedServerKeys(t, "deleg.test", remoteKey, time.Now().Add(time.Hour).UnixMilli(), nil))
	}))
	defer keyServer.Close()

	wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"m.server": strings.TrimPrefix(keyServer.URL, "http://")})
	}))
	defer wk.Close()

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.LookupSRV = noSRV
	resolver.WellKnownURL = func(_, hostname string) string {
		if hostname != "deleg.test" {
			return "http://127.0.0.1:1/"
		}

		return wk.URL + "/.well-known/matrix/server"
	}

	fetcher := &transport.KeyFetcher{Client: transport.NewClient(transport.Options{
		Resolver: resolver, Scheme: "http", AddressPolicy: transport.AllowAllAddresses(),
	})}

	keys, err := fetcher.Fetch(context.Background(), "deleg.test", remoteKey.KeyID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if key, _, ok := keys.Lookup(remoteKey.KeyID); !ok || key != remoteKey.Public() {
		t.Fatalf("ключ не получен: %+v", keys)
	}
}

// TestKeyFetcherSingleflightAndNegativeCache - одновременные запросы
// ключей одного сервера - один исходящий запрос; неудача запоминается.
func TestKeyFetcherSingleflightAndNegativeCache(t *testing.T) {
	t.Parallel()

	remoteKey := mustKey(t, "a_1")

	var (
		hits    atomic.Int32
		release = make(chan struct{})
	)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if strings.HasPrefix(r.Host, "down.test") {
			w.WriteHeader(http.StatusBadGateway)

			return
		}

		<-release
		_ = json.NewEncoder(w).Encode(signedServerKeys(t, "remote.test", remoteKey, time.Now().Add(time.Hour).UnixMilli(), nil))
	}))
	defer remote.Close()

	resolver := keyResolver(strings.TrimPrefix(remote.URL, "http://"))
	resolver.TargetsOverride = func(_ context.Context, name string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: name, Host: strings.TrimPrefix(remote.URL, "http://"), HostHeader: name, Scheme: "http"}}, nil
	}

	now := time.Now()
	fetcher := &transport.KeyFetcher{
		Client: transport.NewClient(transport.Options{Resolver: resolver, AddressPolicy: transport.AllowAllAddresses()}),
		Now:    func() time.Time { return now },
	}

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := fetcher.Fetch(context.Background(), "remote.test", remoteKey.KeyID); err != nil {
				t.Errorf("fetch: %v", err)
			}
		}()
	}

	// Даем всем горутинам встать в ожидание одного запроса.
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if hits.Load() != 1 {
		t.Fatalf("исходящих запросов %d, ожидали 1 (singleflight)", hits.Load())
	}

	// Неизвестный key_id сразу после успешного ответа - без нового запроса.
	if keys, err := fetcher.Fetch(context.Background(), "remote.test", "ed25519:unknown"); err != nil || len(keys.VerifyKeys) == 0 {
		t.Fatalf("повтор: %v", err)
	}

	if hits.Load() != 1 {
		t.Fatalf("перебор key_id размножил запросы: %d", hits.Load())
	}

	// Недоступный сервер: вторая попытка в пределах NegativeTTL в сеть не идет.
	if _, err := fetcher.Fetch(context.Background(), "down.test", "ed25519:a"); err == nil {
		t.Fatal("ожидали ошибку")
	}

	if _, err := fetcher.Fetch(context.Background(), "down.test", "ed25519:a"); err == nil {
		t.Fatal("ожидали ошибку из negative cache")
	}

	if hits.Load() != 2 {
		t.Fatalf("negative cache не сработал: запросов %d", hits.Load())
	}

	// После NegativeTTL - снова в сеть.
	now = now.Add(time.Minute)

	_, _ = fetcher.Fetch(context.Background(), "down.test", "ed25519:a")

	if hits.Load() != 3 {
		t.Fatalf("после NegativeTTL запросов %d, ожидали 3", hits.Load())
	}
}

// TestKeyFetcherNotary - trusted_key_servers - ключи берутся у нотария
// (/_matrix/key/v2/query), ответ подписан сервером и нотарием.
func TestKeyFetcherNotary(t *testing.T) {
	t.Parallel()

	remoteKey := mustKey(t, "a_1")
	notaryKey := mustKey(t, "n_1")
	forged := mustKey(t, "n_1")

	var (
		directHits atomic.Int32
		signWith   atomic.Value
	)

	signWith.Store(notaryKey)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/key/v2/query" && r.Method == http.MethodPost:
			var query struct {
				ServerKeys map[string]map[string]any `json:"server_keys"`
			}

			if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query.ServerKeys["remote.test"] == nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			object := signedServerKeys(t, "remote.test", remoteKey, time.Now().Add(time.Hour).UnixMilli(), nil)
			key, _ := signWith.Load().(signing.Key)
			object = signAs(t, object, "notary.test", key)

			_ = json.NewEncoder(w).Encode(map[string]any{"server_keys": []any{object}})
		case r.URL.Path == "/_matrix/key/v2/server":
			directHits.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	newFetcher := func() *transport.KeyFetcher {
		return &transport.KeyFetcher{
			Client: transport.NewClient(transport.Options{
				Resolver: keyResolver(strings.TrimPrefix(server.URL, "http://")), AddressPolicy: transport.AllowAllAddresses(),
			}),
			TrustedKeyServers: []transport.TrustedKeyServer{{
				ServerName: "notary.test", VerifyKeys: map[string]string{notaryKey.KeyID: notaryKey.Public()},
			}},
		}
	}

	keys, err := newFetcher().Fetch(context.Background(), "remote.test", remoteKey.KeyID)
	if err != nil {
		t.Fatalf("fetch через нотария: %v", err)
	}

	if key, _, ok := keys.Lookup(remoteKey.KeyID); !ok || key != remoteKey.Public() || directHits.Load() != 0 {
		t.Fatalf("ключ от нотария: %+v, прямых запросов %d", keys, directHits.Load())
	}

	// Подпись нотария не его ключом - ответ отвергается, идем напрямую (там 503).
	signWith.Store(forged)

	if _, err := newFetcher().Fetch(context.Background(), "remote.test", remoteKey.KeyID); err == nil {
		t.Fatal("ответ с поддельной подписью нотария принят")
	}

	if directHits.Load() != 1 {
		t.Fatalf("после отказа нотария прямых запросов %d, ожидали 1", directHits.Load())
	}
}
