package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOpenIDToken — выдача одноразового токена и userinfo.
func TestOpenIDToken(t *testing.T) {
	t.Parallel()

	store := newOpenIDStore()

	token := store.issue("@alice:test.local")
	if token == "" {
		t.Fatal("пустой токен")
	}

	userID, ok := store.lookup(token)
	if !ok || userID != "@alice:test.local" {
		t.Fatalf("lookup = %q ok=%v", userID, ok)
	}

	// Одноразовый: второй lookup не находит.
	if _, ok := store.lookup(token); ok {
		t.Fatal("токен переиспользован")
	}

	// Протухший токен не находится.
	store.now = func() time.Time { return time.Now().Add(2 * openIDTTL) }
	expired := "expired-token"

	store.mu.Lock()
	store.tokens[expired] = openIDToken{userID: "@bob:test.local", expiresAt: time.Now()}
	store.mu.Unlock()

	if _, ok := store.lookup(expired); ok {
		t.Fatal("протухший токен принят")
	}

	// userinfo без токена — 400.
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/openid/userinfo", http.NoBody)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("без access_token: %d %s", rec.Code, rec.Body.String())
	}

	// userinfo с неизвестным токеном — 401.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/openid/userinfo?access_token=unknown", http.NoBody)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("неизвестный токен: %d %s", rec.Code, rec.Body.String())
	}
}
