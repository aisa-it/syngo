package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/sso"
	"github.com/aisa-it/syngo/internal/sso/ssotest"
)

const (
	ssoPublicBase = "https://matrix.test/"
	ssoClientURL  = "https://element.test/#/login"
)

// ssoOIDCConfig - провайдер oidc-test на подставном IdP.
func ssoOIDCConfig(idp *ssotest.Provider) config.OIDCProvider {
	return config.OIDCProvider{
		IDPID: "test", IDPName: "Test IdP", IDPBrand: "org.matrix.test", Issuer: idp.Issuer,
		ClientID: idp.ClientID, ClientSecret: idp.ClientSecret, Scopes: []string{"openid", "profile"},
		UserMappingProvider: config.OIDCUserMapping{Config: config.OIDCUserMappingConfig{
			LocalpartTemplate:   "{{ user.preferred_username }}",
			DisplayNameTemplate: "{{ user.name }}",
		}},
	}
}

// ssoHarness - сервер с SSO на in-memory хранилищах и подставным IdP.
type ssoHarness struct {
	t      *testing.T
	server *Server
	idp    *ssotest.Provider
	store  *memStore
	links  *coretest.MemSSOStore
}

func newSSOHarness(t *testing.T, mutate func(*config.Config)) *ssoHarness {
	t.Helper()

	idp := ssotest.New(t, "matrix", "client-secret")

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{}
	cfg.Server.PublicBaseurl = ssoPublicBase
	cfg.Auth.SSO.ClientWhitelist = []string{"https://element.test/"}
	cfg.Auth.OIDCProviders = []config.OIDCProvider{ssoOIDCConfig(idp)}

	if mutate != nil {
		mutate(&cfg)
	}

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mem := newMemStore()
	links := coretest.NewMemSSOStore()

	server.auth = core.NewAuth(mem, fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName: "test.local", RegistrationOpen: true, MinPasswordLength: 8,
		OnExternalLogin: server.syncExternalProfile,
	})

	if server.sso != nil {
		server.sso.accounts = core.NewSSO(server.auth, links)
	}

	return &ssoHarness{t: t, server: server, idp: idp, store: mem, links: links}
}

// get - GET к серверу с cookie сессии SSO (nil - без cookie).
func (h *ssoHarness) get(target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	if cookie != nil {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	return rec
}

// start - редирект на провайдера: cookie сессии и адрес authorize.
func (h *ssoHarness) start(target string) (cookie *http.Cookie, authURL string) {
	h.t.Helper()

	rec := h.get(target, nil)
	if rec.Code != http.StatusFound {
		h.t.Fatalf("redirect %s: %d %s", target, rec.Code, rec.Body.String())
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == sso.CookieName {
			cookie = c
		}
	}

	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.Path != sso.CookiePath || cookie.SameSite != http.SameSiteLaxMode {
		h.t.Fatalf("cookie сессии: %+v", cookie)
	}

	authURL = rec.Header().Get("Location")
	if !strings.HasPrefix(authURL, h.idp.Issuer+"/authorize?") {
		h.t.Fatalf("редирект не на IdP: %s", authURL)
	}

	return cookie, authURL
}

// authorize - вход у IdP; возвращает путь callback с code и state.
func (h *ssoHarness) authorize(authURL string) string {
	h.t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, authURL, http.NoBody)

	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatalf("authorize: %v", err)
	}

	defer resp.Body.Close()

	location, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || location.Scheme+"://"+location.Host+location.Path != ssoPublicBase+"_synapse/client/oidc/callback" {
		h.t.Fatalf("authorize: %d %s", resp.StatusCode, location)
	}

	return location.RequestURI()
}

// loginVia - полный вход: редирект, IdP, callback; возвращает ответ callback.
func (h *ssoHarness) loginVia(target string) *httptest.ResponseRecorder {
	h.t.Helper()

	cookie, authURL := h.start(target)

	return h.get(h.authorize(authURL), cookie)
}

// loginTokenFrom - loginToken из редиректа callback на клиента.
func loginTokenFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	if rec.Code != http.StatusFound {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}

	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), "https://element.test/?loginToken=") || location.Fragment != "/login" {
		t.Fatalf("редирект на клиента: %s", rec.Header().Get("Location"))
	}

	return location.Query().Get("loginToken")
}

func tokenLogin(t *testing.T, server *Server, token string) (status int, body map[string]any) {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.token","token":"`+token+`","device_id":"SSODEV","initial_device_display_name":"Element"}`, "")

	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	return rec.Code, body
}

// TestSSOLoginFlows - GET /login отдает m.login.sso с identity_providers и
// m.login.token: Element показывает кнопку «Continue with ...».
func TestSSOLoginFlows(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)

	rec := do(t, h.server, http.MethodGet, "/_matrix/client/v3/login", "", "")

	var body struct {
		Flows []struct {
			Type              string             `json:"type"`
			IdentityProviders []identityProvider `json:"identity_providers"`
		} `json:"flows"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /login: %d %s", rec.Code, rec.Body.String())
	}

	if len(body.Flows) != 3 || body.Flows[0].Type != "m.login.sso" || body.Flows[1].Type != "m.login.token" ||
		body.Flows[2].Type != "m.login.password" {
		t.Fatalf("flows: %s", rec.Body.String())
	}

	idps := body.Flows[0].IdentityProviders
	if len(idps) != 1 || idps[0].ID != "oidc-test" || idps[0].Name != "Test IdP" || idps[0].Brand != "org.matrix.test" {
		t.Fatalf("identity_providers: %+v", idps)
	}

	// Только SSO: без локальных паролей и LDAP пароль не предлагается.
	disabled := false
	ssoOnly := newSSOHarness(t, func(c *config.Config) { c.Auth.PasswordLocalDB = &disabled })

	rec = do(t, ssoOnly.server, http.MethodGet, "/_matrix/client/v3/login", "", "")
	if strings.Contains(rec.Body.String(), "m.login.password") {
		t.Fatalf("пароль при SSO-only: %s", rec.Body.String())
	}

	// Без SSO - как раньше, только пароль.
	plain := newAuthServer(t, core.AuthOptions{})

	rec = do(t, plain, http.MethodGet, "/_matrix/client/v3/login", "", "")
	if rec.Body.String() != `{"flows":[{"type":"m.login.password"}]}`+"\n" {
		t.Fatalf("flows без SSO: %s", rec.Body.String())
	}
}

// TestSSOLoginFullFlow - полный путь Element: редирект на IdP, callback, loginToken
// на разрешенный адрес клиента, обмен в POST /login; повторный вход - та же учетка
// по subject, повтор loginToken - отказ.
func TestSSOLoginFullFlow(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)
	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "Ivan.Petrov", "name": "Иван Петров"})

	token := loginTokenFrom(t, h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl="+url.QueryEscape(ssoClientURL)))

	authorize := h.idp.Authorizations[0]
	if authorize.Get("code_challenge_method") != "S256" || authorize.Get("nonce") == "" || authorize.Get("state") == "" {
		t.Fatalf("authorize без PKCE/nonce/state: %v", authorize)
	}

	code, body := tokenLogin(t, h.server, token)
	if code != http.StatusOK || body["user_id"] != "@ivan.petrov:test.local" || body["device_id"] != "SSODEV" ||
		body["home_server"] != "test.local" {
		t.Fatalf("m.login.token: %d %v", code, body)
	}

	access, _ := body["access_token"].(string)
	if rec := do(t, h.server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", access); rec.Code != http.StatusOK {
		t.Fatalf("whoami: %d", rec.Code)
	}

	user := h.store.users["ivan.petrov"]
	if user.DisplayName != "Иван Петров" || user.PasswordHash != "" {
		t.Fatalf("учетка SSO: %+v", user)
	}

	// Повторное использование loginToken.
	if code, body := tokenLogin(t, h.server, token); code != http.StatusForbidden || body["errcode"] != ErrCodeForbidden {
		t.Fatalf("повтор loginToken: %d %v", code, body)
	}

	// Повторный вход с другим именем в claims - та же учетка (по subject). Без
	// idpId при одном провайдере - он и используется.
	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "renamed"})

	again := loginTokenFrom(t, h.loginVia("/_matrix/client/v3/login/sso/redirect?redirectUrl="+url.QueryEscape(ssoClientURL)))
	if code, body := tokenLogin(t, h.server, again); code != http.StatusOK || body["user_id"] != "@ivan.petrov:test.local" {
		t.Fatalf("повторный вход: %d %v", code, body)
	}

	// Другой subject с тем же именем - новая учетка с суффиксом.
	h.idp.Login(map[string]any{"sub": "kc-2", "preferred_username": "ivan.petrov"})

	other := loginTokenFrom(t, h.loginVia("/_matrix/client/r0/login/sso/redirect/oidc-test?redirectUrl="+url.QueryEscape(ssoClientURL)))
	if code, body := tokenLogin(t, h.server, other); code != http.StatusOK || body["user_id"] != "@ivan.petrov1:test.local" {
		t.Fatalf("другой subject: %d %v", code, body)
	}

	// Неизвестный токен.
	if code, _ := tokenLogin(t, h.server, "syl_forged"); code != http.StatusForbidden {
		t.Fatalf("чужой loginToken: %d", code)
	}
}

// TestSSOCallbackRejectsForgedState - подделанный state, отсутствующая или чужая
// cookie: callback отказывает, не обращаясь к провайдеру за токеном.
func TestSSOCallbackRejectsForgedState(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)
	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "ivan"})

	target := "/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=" + url.QueryEscape(ssoClientURL)

	cookie, authURL := h.start(target)
	callback := h.authorize(authURL)

	parsed, _ := url.Parse(callback)
	query := parsed.Query()
	query.Set("state", "forged-state")
	forged := parsed.Path + "?" + query.Encode()

	// Сессия другого браузера (атакующий подсовывает жертве свой code).
	otherCookie, _ := h.start(target)

	cases := map[string]struct {
		path   string
		cookie *http.Cookie
	}{
		"подделанный state": {forged, cookie},
		"без cookie":        {callback, nil},
		"чужая cookie":      {callback, otherCookie},
		"битая cookie":      {callback, &http.Cookie{Name: sso.CookieName, Value: "garbage"}},
	}

	for name, tc := range cases {
		rec := h.get(tc.path, tc.cookie)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" ||
			!strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Header().Get("Location"))
		}
	}

	if h.idp.TokenRequests != 0 {
		t.Fatalf("обмен code при подделанной сессии: %d", h.idp.TokenRequests)
	}

	// Ошибка от провайдера (пользователь отказал) и пустой code.
	errorQuery := parsed.Query()
	errorQuery.Del("code")
	errorQuery.Set("error", "access_denied")

	if rec := h.get(parsed.Path+"?"+errorQuery.Encode(), cookie); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "access_denied") {
		t.Fatalf("error от IdP: %d %s", rec.Code, rec.Body.String())
	}

	errorQuery.Del("error")

	if rec := h.get(parsed.Path+"?"+errorQuery.Encode(), cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("без code: %d", rec.Code)
	}

	// Исходная сессия с верным state все еще проходит (cookie одноразовая у браузера,
	// а не на сервере; code одноразовый у провайдера).
	if token := loginTokenFrom(t, h.get(callback, cookie)); token == "" {
		t.Fatal("нет loginToken")
	}
}

// TestSSOCallbackRejectsBadIDToken - id_token с подписью чужим ключом, чужим
// nonce или истекший: учетка не создается, loginToken не выдается.
func TestSSOCallbackRejectsBadIDToken(t *testing.T) {
	t.Parallel()

	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	otherNonce := "replayed-nonce"

	for name, attack := range map[string]func(*ssotest.Provider){
		"чужой ключ":  func(i *ssotest.Provider) { i.SignWith = foreign },
		"чужой nonce": func(i *ssotest.Provider) { i.Nonce = &otherNonce },
		"истекший":    func(i *ssotest.Provider) { i.TTL = -time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newSSOHarness(t, nil)
			h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "mallory"})
			h.idp.Configure(attack)

			rec := h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=" + url.QueryEscape(ssoClientURL))
			if rec.Code != http.StatusForbidden || rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), "loginToken") {
				t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
			}

			if len(h.store.users) != 0 {
				t.Fatalf("учетка создана: %v", h.store.users)
			}
		})
	}
}

// TestSSORedirectValidation - адрес возврата: обязателен, без опасных схем; вне
// sso.client_whitelist - страница подтверждения вместо молчаливого редиректа.
func TestSSORedirectValidation(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)

	for name, tc := range map[string]struct {
		path    string
		status  int
		errcode string
	}{
		"нет redirectUrl": {"/_matrix/client/v3/login/sso/redirect/oidc-test", http.StatusBadRequest, ErrCodeMissingParam},
		"javascript:":     {"/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=javascript:alert(1)", http.StatusBadRequest, ErrCodeInvalidParam},
		"относительный":   {"/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=/x", http.StatusBadRequest, ErrCodeInvalidParam},
		"чужой провайдер": {"/_matrix/client/v3/login/sso/redirect/oidc-evil?redirectUrl=https://element.test/", http.StatusNotFound, ErrCodeNotFound},
	} {
		rec := h.get(tc.path, nil)
		if rec.Code != tc.status || decodeMatrixError(t, rec).Code != tc.errcode {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "ivan"})

	// Адрес вне whitelist (в том числе с похожим префиксом).
	for _, target := range []string{"https://evil.example/steal", "https://element.test.evil.example/"} {
		rec := h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=" + url.QueryEscape(target))
		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: ожидали страницу подтверждения, %d %s", target, rec.Code, rec.Header().Get("Location"))
		}

		page := rec.Body.String()
		if !strings.Contains(page, "Продолжить вход?") || !strings.Contains(page, "loginToken=syl_") ||
			rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("страница подтверждения: %s", page)
		}
	}

	// Схема приложения (Element Desktop) - ссылка не вырезается шаблоном.
	rec := h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=" + url.QueryEscape("element://vector/webapp/"))
	if !strings.Contains(rec.Body.String(), `href="element://vector/webapp/?loginToken=syl_`) {
		t.Fatalf("element://: %s", rec.Body.String())
	}

	// SSO не настроен.
	plain := newAuthServer(t, core.AuthOptions{})
	if rec := do(t, plain, http.MethodGet, "/_matrix/client/v3/login/sso/redirect?redirectUrl=https://x/", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("без SSO: %d", rec.Code)
	}

	if rec := do(t, plain, http.MethodGet, "/_synapse/client/oidc/callback?code=x&state=y", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("callback без SSO: %d", rec.Code)
	}

	if code, body := tokenLogin(t, plain, "syl_x"); code != http.StatusBadRequest || body["errcode"] != ErrCodeUnknown {
		t.Fatalf("m.login.token без SSO: %d %v", code, body)
	}
}

// TestSSOProviderPickerAndErrors - выбор провайдера при нескольких; недоступный
// провайдер и деактивированная учетка - страница ошибки.
func TestSSOProviderPickerAndErrors(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, func(c *config.Config) {
		second := c.Auth.OIDCProviders[0]
		second.IDPID, second.IDPName = "second", "Second IdP"
		c.Auth.OIDCProviders = append(c.Auth.OIDCProviders, second)
	})

	rec := h.get("/_matrix/client/v3/login/sso/redirect?redirectUrl="+url.QueryEscape(ssoClientURL), nil)
	page := rec.Body.String()

	if rec.Code != http.StatusOK || !strings.Contains(page, "/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl=https%3A%2F%2Felement.test%2F%23%2Flogin") ||
		!strings.Contains(page, "Second IdP") {
		t.Fatalf("выбор провайдера: %d %s", rec.Code, page)
	}

	// Деактивированная учетка SSO.
	h.idp.Login(map[string]any{"sub": "kc-9", "preferred_username": "gone"})
	loginTokenFrom(t, h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-second?redirectUrl="+url.QueryEscape(ssoClientURL)))

	if err := h.server.auth.DeactivateAccount(context.Background(), "@gone:test.local"); err != nil {
		t.Fatal(err)
	}

	if rec := h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-second?redirectUrl=" + url.QueryEscape(ssoClientURL)); rec.Code != http.StatusForbidden {
		t.Fatalf("деактивированный: %d", rec.Code)
	}

	// Недоступный провайдер.
	h.idp.Server.Close()

	other := newSSOHarness(t, nil)
	other.idp.Server.Close()

	if rec := other.get("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl="+url.QueryEscape(ssoClientURL), nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("IdP недоступен: %d", rec.Code)
	}
}

// TestSSOUserInteractiveAuth - стадия UIA m.login.sso для учетки без пароля:
// 401 с m.login.sso, fallback, вход у провайдера той же учеткой, повтор запроса с
// session; чужая учетка провайдера стадию не проходит.
func TestSSOUserInteractiveAuth(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)
	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "anna"})

	token := loginTokenFrom(t, h.loginVia("/_matrix/client/v3/login/sso/redirect/oidc-test?redirectUrl="+url.QueryEscape(ssoClientURL)))

	_, body := tokenLogin(t, h.server, token)
	access, _ := body["access_token"].(string)

	const action = "/_matrix/client/v3/account/deactivate"

	rec := do(t, h.server, http.MethodPost, action, `{}`, access)

	var challenge struct {
		Flows []struct {
			Stages []string `json:"stages"`
		} `json:"flows"`
		Session string `json:"session"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil || rec.Code != http.StatusUnauthorized ||
		len(challenge.Flows) != 1 || challenge.Flows[0].Stages[0] != "m.login.sso" || challenge.Session == "" {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}

	// Без пройденной стадии session не пропускает.
	if rec := do(t, h.server, http.MethodPost, action, `{"auth":{"session":"`+challenge.Session+`"}}`, access); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), challenge.Session) {
		t.Fatalf("session без SSO: %d %s", rec.Code, rec.Body.String())
	}

	fallback := "/_matrix/client/v3/auth/m.login.sso/fallback/web?session=" + url.QueryEscape(challenge.Session)

	page := h.get(fallback, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "idp_id=oidc-test") ||
		!strings.Contains(page.Body.String(), action) {
		t.Fatalf("fallback: %d %s", page.Code, page.Body.String())
	}

	// Другая учетка провайдера не подтверждает anna.
	h.idp.Login(map[string]any{"sub": "kc-other", "preferred_username": "mallory"})

	if rec := h.loginVia(fallback + "&idp_id=oidc-test"); rec.Code != http.StatusForbidden {
		t.Fatalf("чужая учетка IdP: %d %s", rec.Code, rec.Body.String())
	}

	h.idp.Login(map[string]any{"sub": "kc-1", "preferred_username": "anna"})

	done := h.loginVia(fallback + "&idp_id=oidc-test")
	if done.Code != http.StatusOK || !strings.Contains(done.Body.String(), "authDone") {
		t.Fatalf("UIA SSO: %d %s", done.Code, done.Body.String())
	}

	// Пройденная сессия не годится для другого действия.
	if rec := do(t, h.server, http.MethodPost, "/_matrix/client/v3/account/password",
		`{"new_password":"new-password-1","auth":{"session":"`+challenge.Session+`"}}`, access); rec.Code != http.StatusUnauthorized {
		t.Fatalf("сессия для другого действия: %d", rec.Code)
	}

	if rec := do(t, h.server, http.MethodPost, action, `{"auth":{"type":"m.login.sso","session":"`+challenge.Session+`"}}`, access); rec.Code != http.StatusOK {
		t.Fatalf("повтор с session: %d %s", rec.Code, rec.Body.String())
	}

	// Ошибки fallback: неизвестная сессия, провайдер не привязан.
	if rec := h.get("/_matrix/client/v3/auth/m.login.sso/fallback/web?session=unknown", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестная сессия: %d", rec.Code)
	}

	if rec := h.get(fallback+"&idp_id=oidc-evil", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("сессия после использования: %d", rec.Code)
	}
}

// TestSSOUIAPasswordUser - пользователь с паролем без привязки SSO получает только
// m.login.password, как раньше; fallback SSO для него недоступен.
func TestSSOUIAPasswordUser(t *testing.T) {
	t.Parallel()

	h := newSSOHarness(t, nil)
	alice := registerUser(t, h.server, "alice", "alice-password")

	rec := do(t, h.server, http.MethodPost, "/_matrix/client/v3/account/deactivate", `{}`, alice.AccessToken)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"flows":[{"stages":["m.login.password"]}]`) {
		t.Fatalf("challenge: %d %s", rec.Code, rec.Body.String())
	}

	var challenge struct {
		Session string `json:"session"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &challenge)

	if rec := h.get("/_matrix/client/v3/auth/m.login.sso/fallback/web?session="+challenge.Session, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("fallback без привязки: %d", rec.Code)
	}

	// Пароль работает как раньше.
	if rec := do(t, h.server, http.MethodPost, "/_matrix/client/v3/account/deactivate",
		`{"auth":{"type":"m.login.password","password":"alice-password","session":"`+challenge.Session+`"}}`, alice.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("пароль: %d %s", rec.Code, rec.Body.String())
	}
}
