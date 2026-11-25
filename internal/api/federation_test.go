package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// newFederationServer — сервер с ключом федерации и без БД.
func newFederationServer(t *testing.T) (*Server, signing.Key) {
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

	server := New(cfg, nil, nil)
	server.SetFederationKey(&key)

	return server, key
}

// TestKeyServerSignedResponse - ответ /_matrix/key/v2/server валиден
// и подпись проверяется внешним кодом (без наших хелперов проверки запросов).
func TestKeyServerSignedResponse(t *testing.T) {
	t.Parallel()

	server, key := newFederationServer(t)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/key/v2/server", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var object map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if object["server_name"] != "test.local" {
		t.Fatalf("server_name = %v", object["server_name"])
	}

	keys, ok := object["verify_keys"].(map[string]any)
	if !ok || keys[key.KeyID] == nil {
		t.Fatalf("verify_keys: %v", object["verify_keys"])
	}

	pub := keys[key.KeyID].(map[string]any)["key"].(string)
	if pub != key.Public() {
		t.Fatalf("public key не совпал")
	}

	signatures := object["signatures"].(map[string]any)["test.local"].(map[string]any)
	sig := signatures[key.KeyID].(string)

	delete(object, "signatures")

	canonical, err := signing.CanonicalJSON(object)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	if err := signing.VerifyEd25519(pub, string(canonical), sig); err != nil {
		t.Fatalf("внешняя проверка подписи ответа: %v", err)
	}

	// Неизвестный keyId — 404.
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/key/v2/server/ed25519:zzz", http.NoBody))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестный keyId: %d, ожидали 404", rec.Code)
	}
}

// TestFederationMiddleware — корректный X-Matrix принимается, поддельный отклоняется.
func TestFederationMiddleware(t *testing.T) {
	t.Parallel()

	server, key := newFederationServer(t)

	server.keyLookup = func(origin, keyID string) (string, int64, error) {
		if origin != "origin.test" || keyID != key.KeyID {
			return "", 0, signing.ErrInvalidKey
		}

		return key.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
	}

	handler := server.requireFederation(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := `{"pdus":[]}`

	// Корректная подпись.
	header, err := signing.MatrixAuthHeader(key, "origin.test", "test.local", http.MethodPut, "/_matrix/federation/v1/send/1", []byte(body))
	if err != nil {
		t.Fatalf("header: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/_matrix/federation/v1/send/1", strings.NewReader(body))
	req.Header.Set("Authorization", header)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("корректный X-Matrix: %d %s", rec.Code, rec.Body.String())
	}

	// Подделанная подпись.
	bad, err := signing.MatrixAuthHeader(key, "origin.test", "test.local", http.MethodPut, "/_matrix/federation/v1/send/2", []byte(body))
	if err != nil {
		t.Fatalf("header: %v", err)
	}

	req = httptest.NewRequest(http.MethodPut, "/_matrix/federation/v1/send/1", strings.NewReader(body)) // другой URI
	req.Header.Set("Authorization", bad)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("поддельная подпись: %d, ожидали 401", rec.Code)
	}

	var errResp matrixError
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)

	if errResp.Code != ErrCodeUnauthorized {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeUnauthorized)
	}

	// Без заголовка — 401.
	req = httptest.NewRequest(http.MethodPut, "/_matrix/federation/v1/send/1", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без заголовка: %d, ожидали 401", rec.Code)
	}
}
