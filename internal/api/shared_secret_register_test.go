package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

const testSharedSecret = "0123456789abcdef0123456789abcdef"

// sharedSecretBody собирает тело POST /_synapse/admin/v1/register с подписью.
func sharedSecretBody(secret, nonce, user, password string, admin bool) string {
	mac := hex.EncodeToString(sharedSecretMAC(secret, nonce, user, password, admin))

	return fmt.Sprintf(`{"nonce":%q,"username":%q,"password":%q,"admin":%t,"mac":%q}`,
		nonce, user, password, admin, mac)
}

func getNonce(t *testing.T, server *Server) string {
	t.Helper()

	rec := do(t, server, http.MethodGet, "/_synapse/admin/v1/register", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET nonce: %d %s", rec.Code, rec.Body)
	}

	var resp struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Nonce == "" {
		t.Fatalf("nonce: %v %s", err, rec.Body)
	}

	return resp.Nonce
}

// TestSharedSecretRegistration - при закрытой регистрации (по умолчанию)
// оператор создает пользователя и администратора через общий секрет, обычный
// /register отвечает 403.
func TestSharedSecretRegistration(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{}, false)
	server.cfg.Auth.RegistrationSharedSecret = testSharedSecret

	store := newMemStore()
	server.auth = core.NewAuth(store, fastHasher{}, &countingTokens{},
		core.AuthOptions{ServerName: "test.local", RegistrationOpen: false, MinPasswordLength: 8})

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		`{"username":"bob","password":"bob-password-1","auth":{"type":"m.login.dummy"}}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("/register при закрытой регистрации: %d, ожидали 403", rec.Code)
	}

	nonce := getNonce(t, server)

	rec := do(t, server, http.MethodPost, "/_synapse/admin/v1/register",
		sharedSecretBody(testSharedSecret, nonce, "admin", "admin-password-1", true), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("shared secret register: %d %s", rec.Code, rec.Body)
	}

	var reg registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if reg.UserID != "@admin:test.local" || reg.AccessToken == "" {
		t.Fatalf("ответ регистрации: %+v", reg)
	}

	if !store.users["admin"].IsAdmin {
		t.Fatal("пользователь создан без роли администратора")
	}

	// Повтор nonce запрещен.
	if rec := do(t, server, http.MethodPost, "/_synapse/admin/v1/register",
		sharedSecretBody(testSharedSecret, nonce, "other", "other-password-1", false), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("повтор nonce: %d, ожидали 400", rec.Code)
	}

	// Неверный секрет - 403, nonce сгорает.
	nonce = getNonce(t, server)
	if rec := do(t, server, http.MethodPost, "/_synapse/admin/v1/register",
		sharedSecretBody("wrong-secret", nonce, "evil", "evil-password-1", true), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("неверный mac: %d, ожидали 403", rec.Code)
	}

	if _, exists := store.users["evil"]; exists {
		t.Fatal("пользователь создан с неверной подписью")
	}

	// Вход созданным пользователем.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","user":"admin","password":"admin-password-1"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
}

// TestSharedSecretRegistrationDisabled - без секрета эндпоинт выключен.
func TestSharedSecretRegistrationDisabled(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{}, false)

	if rec := do(t, server, http.MethodGet, "/_synapse/admin/v1/register", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("GET без секрета: %d, ожидали 400", rec.Code)
	}

	if rec := do(t, server, http.MethodPost, "/_synapse/admin/v1/register",
		sharedSecretBody("", "n", "u", "password-123", false), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST без секрета: %d, ожидали 400", rec.Code)
	}
}

func TestNonceStoreExpires(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	n := newNonceStore()
	n.now = func() time.Time { return now }

	nonce, err := n.issue()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(sharedSecretNonceTTL + time.Second)

	if n.consume(nonce) {
		t.Fatal("просроченный nonce принят")
	}
}
