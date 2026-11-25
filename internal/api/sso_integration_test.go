//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/sso/ssotest"
)

// TestSSOLoginIntegration - путь Element на PostgreSQL: GET /login с
// m.login.sso, редирект на подставной IdP (discovery, authorize, token, JWKS),
// callback, loginToken, POST /login m.login.token, /sync; привязка в
// user_external_ids, loginToken погашен; удаление устройства через UIA m.login.sso.
func TestSSOLoginIntegration(t *testing.T) {
	idp := ssotest.New(t, "matrix", "client-secret")

	server := newIntegrationServerWithConfig(t, nil, func(c *config.Config) {
		c.Server.PublicBaseurl = ssoPublicBase
		c.Auth.SSO.ClientWhitelist = []string{"https://element.test/"}
		c.Auth.OIDCProviders = []config.OIDCProvider{ssoOIDCConfig(idp)}
	})

	h := &ssoHarness{t: t, server: server, idp: idp}
	ctx := context.Background()

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/login", "", ""); !strings.Contains(rec.Body.String(),
		`{"type":"m.login.sso","identity_providers":[{"id":"oidc-test","name":"Test IdP","brand":"org.matrix.test"}]}`) {
		t.Fatalf("GET /login: %s", rec.Body.String())
	}

	idp.Login(map[string]any{"sub": "kc-42", "preferred_username": "Olga", "name": "Ольга"})

	start := "/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=" + url.QueryEscape(ssoClientURL)
	token := loginTokenFrom(t, h.loginVia(start))

	code, body := tokenLogin(t, server, token)
	if code != http.StatusOK || body["user_id"] != "@olga:test.local" {
		t.Fatalf("m.login.token: %d %v", code, body)
	}

	access, _ := body["access_token"].(string)

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", access); rec.Code != http.StatusOK {
		t.Fatalf("/sync: %d %s", rec.Code, rec.Body.String())
	}

	profile := decodeJSON(t, do(t, server, http.MethodGet, "/_matrix/client/v3/profile/@olga:test.local", "", access).Body.Bytes())
	if profile["displayname"] != "Ольга" {
		t.Fatalf("профиль: %v", profile)
	}

	var linked string
	if err := server.db.Pool().QueryRow(ctx, `SELECT user_id FROM user_external_ids
		WHERE auth_provider = 'oidc-test' AND external_id = 'kc-42'`).Scan(&linked); err != nil || linked != "@olga:test.local" {
		t.Fatalf("user_external_ids: %q %v", linked, err)
	}

	var used int
	if err := server.db.Pool().QueryRow(ctx, `SELECT count(*) FROM login_tokens WHERE used_at IS NOT NULL`).Scan(&used); err != nil || used != 1 {
		t.Fatalf("login_tokens: %d %v", used, err)
	}

	if code, body := tokenLogin(t, server, token); code != http.StatusForbidden || body["errcode"] != ErrCodeForbidden {
		t.Fatalf("повтор loginToken: %d %v", code, body)
	}

	// Второе устройство и его удаление через UIA m.login.sso (пароля у учетки нет).
	second := loginTokenFrom(t, h.loginVia(start))

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login", `{"type":"m.login.token","token":"`+second+`","device_id":"OTHER"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("второй вход: %d %s", rec.Code, rec.Body.String())
	}

	const deleteBody = `{"devices":["OTHER"]`

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices", deleteBody+`}`, access)

	var challenge struct {
		Flows []struct {
			Stages []string `json:"stages"`
		} `json:"flows"`
		Session string `json:"session"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil || rec.Code != http.StatusUnauthorized ||
		len(challenge.Flows) != 1 || challenge.Flows[0].Stages[0] != "m.login.sso" {
		t.Fatalf("UIA: %d %s", rec.Code, rec.Body.String())
	}

	done := h.loginVia("/_matrix/client/v3/auth/m.login.sso/fallback/web?session=" + url.QueryEscape(challenge.Session) + "&idp_id=oidc-test")
	if done.Code != http.StatusOK || !strings.Contains(done.Body.String(), "authDone") {
		t.Fatalf("fallback: %d %s", done.Code, done.Body.String())
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/delete_devices", deleteBody+`,"auth":{"session":"`+challenge.Session+`"}}`, access)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete_devices после SSO: %d %s", rec.Code, rec.Body.String())
	}

	devices := do(t, server, http.MethodGet, "/_matrix/client/v3/devices", "", access).Body.String()
	if strings.Contains(devices, "OTHER") || !strings.Contains(devices, "SSODEV") {
		t.Fatalf("устройства: %s", devices)
	}
}
