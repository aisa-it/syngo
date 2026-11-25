package api

import (
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/auth"
	"github.com/aisa-it/syngo/internal/core"
)

// TestLoginEmptyHashForbidden - пользователь без локального пароля (перенесенный из
// Synapse с password_hash IS NULL: LDAP, SSO, appservice) получает 403 M_FORBIDDEN, как
// при неверном пароле. Раньше /login отвечал 500 и этим раскрывал, что учетка есть.
func TestLoginEmptyHashForbidden(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})
	mem := newMemStore()
	mem.users["ldapuser"] = core.User{UserID: "@ldapuser:test.local", Localpart: "ldapuser"}
	server.auth = core.NewAuth(mem, auth.NewMultiHasher(), &countingTokens{}, core.AuthOptions{ServerName: "test.local"})

	empty := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"ldapuser"},"password":"guess"}`, "")
	unknown := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"ghost"},"password":"guess"}`, "")

	if empty.Code != http.StatusForbidden || decodeMatrixError(t, empty).Code != ErrCodeForbidden {
		t.Fatalf("пустой хеш: %d %s", empty.Code, empty.Body.String())
	}

	if empty.Body.String() != unknown.Body.String() {
		t.Fatalf("ответ отличается от неизвестного пользователя: %s vs %s", empty.Body.String(), unknown.Body.String())
	}
}
