//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/sso"
	"github.com/aisa-it/syngo/internal/sso/ssotest"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// TestImportSynapseExternalIDs - user_external_ids Synapse
// переносится export.sql и syngo-import, и после переезда вход через того же
// провайдера (idp_id keycloak -> auth_provider oidc-keycloak) с тем же subject
// попадает в перенесенную учетку без пароля, а не создает новую.
func TestImportSynapseExternalIDs(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	ctx := context.Background()

	db, err := store.Open(ctx, dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	loadUsersFixture(t, db.Pool())

	// Привязки как у Synapse с oidc_providers: idp_id keycloak хранится с префиксом.
	// Привязка удаленного пользователя (нет в users) не выгружается.
	if _, err := db.Pool().Exec(ctx, `INSERT INTO synapse.user_external_ids (auth_provider, external_id, user_id) VALUES
		('oidc-keycloak', 'kc-ldapuser', '@ldapuser:fixture.local'),
		('oidc-keycloak', 'kc-ghost', '@ghost:fixture.local')`); err != nil {
		t.Fatal(err)
	}

	dump := exportSynapse(t, db.Pool())

	for run := 1; run <= 2; run++ {
		im := newImporter(db, fixtureServer, 1000)
		if err := im.importFile(ctx, dump, 0); err != nil || len(im.errors) > 0 {
			t.Fatalf("импорт %d: %v %+v", run, err, im.errors)
		}

		if im.counts.ExternalIDs != 1 {
			t.Fatalf("импорт %d: external_ids = %d", run, im.counts.ExternalIDs)
		}

		report, err := verifyUsers(ctx, db, im.userStats)
		if err != nil || len(report.Mismatches) > 0 || report.Checked["external_ids"] != 1 {
			t.Fatalf("сверка %d: %+v %v", run, report, err)
		}
	}

	idp := ssotest.New(t, "matrix", "client-secret")

	cfg := config.Default()
	cfg.Matrix.ServerName = fixtureServer
	cfg.RateLimit = config.RateLimit{}
	cfg.Sync.TokenSecret = "integration-secret"
	cfg.Media.Directory = t.TempDir()
	cfg.Server.PublicBaseurl = "https://matrix.fixture.local"
	cfg.Auth.SSO.ClientWhitelist = []string{"https://element.fixture.local/"}
	cfg.Auth.OIDCProviders = []config.OIDCProvider{{
		IDPID: "keycloak", IDPName: "Keycloak", Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		UserMappingProvider: config.OIDCUserMapping{Config: config.OIDCUserMappingConfig{
			LocalpartTemplate: "{{ user.preferred_username }}",
		}},
	}}

	handler := api.New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()

	// Имя в провайдере другое: учетка находится по subject.
	idp.Login(map[string]any{"sub": "kc-ldapuser", "preferred_username": "renamed-in-keycloak"})

	token := ssoLoginToken(t, handler, idp, "/_matrix/client/v3/login/sso/redirect/oidc-keycloak?redirectUrl="+
		url.QueryEscape("https://element.fixture.local/"))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login",
		strings.NewReader(`{"type":"m.login.token","token":"`+token+`"}`)))

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	if rec.Code != http.StatusOK || body["user_id"] != "@ldapuser:fixture.local" {
		t.Fatalf("вход SSO после переезда: %d %v", rec.Code, body)
	}

	var users int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM users WHERE localpart LIKE 'renamed%'`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("создана новая учетка: %d %v", users, err)
	}
}

// ssoLoginToken проходит редирект на IdP, authorize и callback; loginToken из
// редиректа на клиента.
func ssoLoginToken(t *testing.T, handler http.Handler, idp *ssotest.Provider, start string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, start, http.NoBody))

	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), idp.Issuer) {
		t.Fatalf("редирект на IdP: %d %s", rec.Code, rec.Body.String())
	}

	var cookie *http.Cookie

	for _, c := range rec.Result().Cookies() {
		if c.Name == sso.CookieName {
			cookie = c
		}
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, rec.Header().Get("Location"), http.NoBody)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	resp.Body.Close()

	callback, _ := url.Parse(resp.Header.Get("Location"))

	cbReq := httptest.NewRequest(http.MethodGet, callback.RequestURI(), http.NoBody)
	cbReq.AddCookie(cookie)

	cb := httptest.NewRecorder()
	handler.ServeHTTP(cb, cbReq)

	location, _ := url.Parse(cb.Header().Get("Location"))
	if cb.Code != http.StatusFound || location == nil || location.Query().Get("loginToken") == "" {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}

	return location.Query().Get("loginToken")
}
