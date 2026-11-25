package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/sso"
)

// Вход через OIDC/SSO, совместимо с Synapse:
//
//	GET /_matrix/client/v3/login/sso/redirect[/{idpId}]?redirectUrl=... - в браузере
//	    клиента: cookie сессии и редирект на authorize провайдера;
//	GET /_synapse/client/oidc/callback - возврат от провайдера (адрес как у Synapse:
//	    IdP при переезде не перенастраивается): проверка state по cookie, обмен code,
//	    проверка id_token, учетка по user_external_ids, loginToken клиенту;
//	POST /_matrix/client/v3/login {"type":"m.login.token"} - обмен loginToken на сессию;
//	GET /_matrix/client/v3/auth/m.login.sso/fallback/web?session=... - стадия UIA
//	    m.login.sso для учеток без пароля.

// ssoStage - стадия UIA и тип входа SSO.
const ssoStage = "m.login.sso"

// ssoServer - провайдеры и состояние входа через SSO.
type ssoServer struct {
	providers []*sso.Provider
	byID      map[string]*sso.Provider
	codec     *sso.SessionCodec
	// accounts - учетки и loginToken; nil без БД (вход недоступен, 503).
	accounts      *core.SSO
	whitelist     []string
	updateProfile bool
	secureCookie  bool
}

// newSSO собирает провайдеры из auth.oidc_providers. Провайдер с ошибкой конфига
// (нечитаемый client_secret_path) пропускается с записью в журнал, как LDAP;
// discovery выполняется при первом входе. Ключ cookie сессии - случайный на процесс:
// незавершенные входы после перезапуска начинаются заново.
func (s *Server) newSSO(cfg config.Config, links core.SSOStore) *ssoServer {
	enabled := cfg.Auth.EnabledOIDCProviders()
	if len(enabled) == 0 {
		return nil
	}

	base := cfg.Server.PublicBaseURL()
	callback := strings.TrimSuffix(base, "/") + sso.CallbackPath

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		s.log.Error("sso: session key", "error", err)

		return nil
	}

	codec, err := sso.NewSessionCodec(key, nil)
	if err != nil {
		s.log.Error("sso: session codec", "error", err)

		return nil
	}

	out := &ssoServer{
		byID: map[string]*sso.Provider{}, codec: codec,
		whitelist:     cfg.Auth.SSO.ClientWhitelist,
		updateProfile: cfg.Auth.SSO.UpdateProfileInformation,
		secureCookie:  strings.HasPrefix(base, "https://"),
	}

	for i := range enabled {
		providerCfg := &enabled[i]

		provider, err := sso.NewProvider(*providerCfg, sso.Options{RedirectURI: callback})
		if err != nil {
			s.log.Error("sso: OIDC provider is not configured", "idp_id", providerCfg.AuthProviderID(), "error", err)

			continue
		}

		s.log.Info("sso: OIDC provider enabled", "idp_id", provider.ID, "issuer", providerCfg.Issuer)
		out.providers = append(out.providers, provider)
		out.byID[provider.ID] = provider
	}

	if len(out.providers) == 0 {
		return nil
	}

	if links != nil && s.auth != nil {
		out.accounts = core.NewSSO(s.auth, links)
	}

	return out
}

// identityProvider - элемент identity_providers в GET /login.
type identityProvider struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Icon  string `json:"icon,omitempty"`
	Brand string `json:"brand,omitempty"`
}

// ssoLoginFlows - m.login.sso (кнопки «Continue with ...» в Element) и m.login.token.
func (s *Server) ssoLoginFlows() []loginFlow {
	if s.sso == nil {
		return nil
	}

	providers := make([]identityProvider, 0, len(s.sso.providers))
	for _, p := range s.sso.providers {
		providers = append(providers, identityProvider{ID: p.ID, Name: p.Name, Icon: p.Icon, Brand: p.Brand})
	}

	return []loginFlow{{Type: ssoStage, IdentityProviders: providers}, {Type: "m.login.token"}}
}

// handleSSORedirect - GET /login/sso/redirect[/{idpId}]?redirectUrl=...
func (s *Server) handleSSORedirect(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Homeserver not configured for SSO")

		return
	}

	redirectURL := r.URL.Query().Get("redirectUrl")
	if redirectURL == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingParam, "Missing redirectUrl")

		return
	}

	if _, err := sso.CheckRedirectURL(redirectURL); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid redirectUrl")

		return
	}

	idpID := pathParamValue(r, "idpId")
	if idpID == "" {
		if len(s.sso.providers) > 1 {
			s.writeIDPPicker(w, redirectURL)

			return
		}

		idpID = s.sso.providers[0].ID
	}

	provider := s.sso.byID[idpID]
	if provider == nil {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Unknown identity provider")

		return
	}

	s.startSSO(w, r, provider, redirectURL, "")
}

// startSSO ставит cookie сессии и уводит браузер на authorize провайдера.
func (s *Server) startSSO(w http.ResponseWriter, r *http.Request, provider *sso.Provider, redirectURL, uiaSession string) {
	session, err := s.sso.codec.NewSession(provider.ID, redirectURL, uiaSession)
	if err != nil {
		s.log.ErrorContext(r.Context(), "sso: new session", "error", err)
		writeSSOPage(w, http.StatusInternalServerError, pageError, ssoPageData{Message: msgInternal})

		return
	}

	authURL, err := provider.AuthorizationURL(r.Context(), &session)
	if err != nil {
		s.log.ErrorContext(r.Context(), "sso: provider unavailable", "idp_id", provider.ID, "error", err)
		writeSSOPage(w, http.StatusBadGateway, pageError, ssoPageData{Message: msgProviderUnavailable})

		return
	}

	value, err := s.sso.codec.Encode(session)
	if err != nil {
		s.log.ErrorContext(r.Context(), "sso: encode session", "error", err)
		writeSSOPage(w, http.StatusInternalServerError, pageError, ssoPageData{Message: msgInternal})

		return
	}

	http.SetCookie(w, s.ssoCookie(value, int(sso.SessionTTL.Seconds())))
	w.Header().Set("Cache-Control", "no-store")
	//nolint:gosec // адрес authorize собран из конфига провайдера, не из запроса
	http.Redirect(w, r, authURL, http.StatusFound)
}

// ssoCookie - cookie сессии входа. SameSite=Lax: возврат от провайдера - переход
// верхнего уровня GET, cookie уходит; из чужих фоновых запросов - нет.
func (s *Server) ssoCookie(value string, maxAge int) *http.Cookie {
	//nolint:gosec // Secure - при https public_baseurl; http допустим только в dev/test (валидация конфига)
	return &http.Cookie{
		Name: sso.CookieName, Value: value, Path: sso.CookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: s.sso.secureCookie, SameSite: http.SameSiteLaxMode,
	}
}

// handleOIDCCallback - GET /_synapse/client/oidc/callback.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeSSOPage(w, http.StatusNotFound, pageError, ssoPageData{Message: msgNotConfigured})

		return
	}

	session, ok := s.callbackSession(w, r)
	if !ok {
		return
	}

	provider := s.sso.byID[session.IDP]
	if provider == nil {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionInvalid})

		return
	}

	if s.sso.accounts == nil {
		writeSSOPage(w, http.StatusServiceUnavailable, pageError, ssoPageData{Message: msgInternal})

		return
	}

	identity, err := provider.Exchange(r.Context(), r.URL.Query().Get("code"), session)
	if err != nil {
		s.writeSSOExchangeError(w, r, provider.ID, err)

		return
	}

	if session.UIASession != "" {
		s.completeSSOUIA(w, r, provider, identity, session.UIASession)

		return
	}

	user, created, err := s.sso.accounts.ResolveUser(r.Context(), core.SSOIdentity{
		Provider: provider.ID, Subject: identity.Subject,
		Localpart: provider.Localpart(identity.Claims), DisplayName: provider.DisplayName(identity.Claims),
		AllowExistingUsers: provider.AllowExistingUsers(), UpdateProfile: s.sso.updateProfile,
	})
	if err != nil {
		s.writeSSOAccountError(w, r, provider.ID, err)

		return
	}

	token, err := s.sso.accounts.IssueLoginToken(r.Context(), user.UserID, provider.ID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "sso: login token", "user_id", user.UserID, "error", err)
		writeSSOPage(w, http.StatusInternalServerError, pageError, ssoPageData{Message: msgInternal})

		return
	}

	s.log.InfoContext(r.Context(), "sso: login", "idp_id", provider.ID, "user_id", user.UserID, "created", created)

	target, err := sso.CheckRedirectURL(session.RedirectURL)
	if err != nil {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionInvalid})

		return
	}

	destination := sso.AddLoginToken(target, token)
	w.Header().Set("Cache-Control", "no-store")

	// Адрес вне sso.client_whitelist - страница подтверждения, как в Synapse: иначе
	// чужой сайт, подсунувший свой redirectUrl, молча получил бы loginToken.
	if !sso.Whitelisted(session.RedirectURL, s.sso.whitelist) {
		writeSSOPage(w, http.StatusOK, pageConfirmRedirect, ssoPageData{
			Target: displayURL(target), Link: template.URL(destination), //nolint:gosec // схема проверена CheckRedirectURL
		})

		return
	}

	http.Redirect(w, r, destination, http.StatusFound)
}

// callbackSession проверяет cookie и state; cookie удаляется в любом случае
// (сессия одноразовая).
func (s *Server) callbackSession(w http.ResponseWriter, r *http.Request) (sso.Session, bool) {
	cookie, err := r.Cookie(sso.CookieName)
	if err != nil {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionMissing})

		return sso.Session{}, false
	}

	http.SetCookie(w, s.ssoCookie("", -1))

	session, err := s.sso.codec.Decode(cookie.Value)
	if err != nil {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionMissing})

		return sso.Session{}, false
	}

	query := r.URL.Query()

	if state := query.Get("state"); state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(session.State)) != 1 {
		s.log.WarnContext(r.Context(), "sso: state mismatch", "idp_id", session.IDP)
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionInvalid})

		return sso.Session{}, false
	}

	if idpError := query.Get("error"); idpError != "" {
		s.log.WarnContext(r.Context(), "sso: provider returned error", "idp_id", session.IDP,
			"error", idpError, "description", query.Get("error_description"))
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgProviderRefused, Detail: idpError})

		return sso.Session{}, false
	}

	if query.Get("code") == "" {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgSessionInvalid})

		return sso.Session{}, false
	}

	return session, true
}

// writeSSOExchangeError - отказ обмена code или проверки id_token.
func (s *Server) writeSSOExchangeError(w http.ResponseWriter, r *http.Request, idpID string, err error) {
	s.log.WarnContext(r.Context(), "sso: exchange failed", "idp_id", idpID, "error", err)

	switch {
	case errors.Is(err, sso.ErrInvalidIDToken):
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgInvalidResponse})
	case errors.Is(err, sso.ErrNotAllowed):
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgNotAllowed})
	case errors.Is(err, sso.ErrMapping):
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgMapping})
	default:
		writeSSOPage(w, http.StatusBadGateway, pageError, ssoPageData{Message: msgProviderUnavailable})
	}
}

// writeSSOAccountError - учетку не удалось найти или создать.
func (s *Server) writeSSOAccountError(w http.ResponseWriter, r *http.Request, idpID string, err error) {
	switch {
	case errors.Is(err, core.ErrUserDeactivated):
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgDeactivated})
	case errors.Is(err, core.ErrSSOMapping):
		s.log.WarnContext(r.Context(), "sso: mapping failed", "idp_id", idpID, "error", err)
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgMapping})
	default:
		s.log.ErrorContext(r.Context(), "sso: resolve user", "idp_id", idpID, "error", err)
		writeSSOPage(w, http.StatusInternalServerError, pageError, ssoPageData{Message: msgInternal})
	}
}

// displayURL - адрес для страницы подтверждения без query и фрагмента.
func displayURL(target *url.URL) string {
	out := *target
	out.RawQuery, out.Fragment, out.RawFragment, out.User = "", "", "", nil

	return out.String()
}

// writeIDPPicker - выбор провайдера, если их несколько, а клиент не указал idpId
// (pick_idp Synapse).
func (s *Server) writeIDPPicker(w http.ResponseWriter, redirectURL string) {
	links := make([]ssoLink, 0, len(s.sso.providers))

	for _, p := range s.sso.providers {
		links = append(links, ssoLink{
			Name: p.Name,
			URL: "/_matrix/client/v3/login/sso/redirect/" + url.PathEscape(p.ID) +
				"?redirectUrl=" + url.QueryEscape(redirectURL),
		})
	}

	writeSSOPage(w, http.StatusOK, pagePicker, ssoPageData{Links: links})
}

// handleTokenLogin - POST /login {"type":"m.login.token","token":...}.
func (s *Server) handleTokenLogin(w http.ResponseWriter, r *http.Request, req *loginRequest) {
	if s.sso == nil || s.sso.accounts == nil {
		s.writeAuthError(w, core.ErrUnsupportedLogin)

		return
	}

	result, err := s.sso.accounts.LoginWithToken(r.Context(), core.TokenLoginRequest{
		Token: req.Token, DeviceID: req.DeviceID, DeviceDisplayName: req.InitialDM, RequestRefreshToken: req.Refresh,
	})
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		UserID: result.UserID, AccessToken: result.AccessToken, DeviceID: result.DeviceID,
		HomeServer: s.cfg.Matrix.ServerName, RefreshToken: result.RefreshToken, ExpiresInMS: result.ExpiresInMS,
	})
}

// --- UIA m.login.sso ---

// uiaBinding - к чему привязана сессия UIA: пользователь и действие (метод и путь).
// SSO подтверждает только этого пользователя, и сессия не годится для другого запроса.
type uiaBinding struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
}

func requestAction(r *http.Request) string {
	return r.Method + " " + r.URL.Path
}

// uiaBindingOf читает привязку сессии UIA.
func (s *Server) uiaBindingOf(sessionID string) (core.UIASession, uiaBinding, bool) {
	if sessionID == "" {
		return core.UIASession{}, uiaBinding{}, false
	}

	session, ok := s.uia.Get(sessionID)
	if !ok {
		return core.UIASession{}, uiaBinding{}, false
	}

	var binding uiaBinding
	if err := json.Unmarshal(session.ClientData, &binding); err != nil || binding.UserID == "" {
		return core.UIASession{}, uiaBinding{}, false
	}

	return session, binding, true
}

// uiaSessionFor - сессия UIA пользователя для запроса: переданная клиентом, если
// она его и для того же действия, иначе новая.
func (s *Server) uiaSessionFor(r *http.Request, userID, requested string) string {
	if _, binding, ok := s.uiaBindingOf(requested); ok && binding.UserID == userID && binding.Action == requestAction(r) {
		return requested
	}

	data, _ := json.Marshal(uiaBinding{UserID: userID, Action: requestAction(r)})

	session, err := s.uia.Create(data)
	if err != nil {
		return ""
	}

	return session.ID
}

// ssoUIACompleted - сессия прошла m.login.sso для этого пользователя и действия.
func (s *Server) ssoUIACompleted(r *http.Request, userID, sessionID string) bool {
	session, binding, ok := s.uiaBindingOf(sessionID)

	return ok && binding.UserID == userID && binding.Action == requestAction(r) &&
		slices.Contains(session.Completed, ssoStage)
}

// ssoProvidersFor - настроенные провайдеры, к которым привязана учетка.
func (s *Server) ssoProvidersFor(ctx context.Context, userID string) []*sso.Provider {
	if s.sso == nil || s.sso.accounts == nil {
		return nil
	}

	linked, err := s.sso.accounts.Providers(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "sso: providers for user", "user_id", userID, "error", err)

		return nil
	}

	var out []*sso.Provider

	for _, p := range s.sso.providers {
		if slices.Contains(linked, p.ID) {
			out = append(out, p)
		}
	}

	return out
}

// uiaStages - стадии UIA пользователя: пароль, если он есть (локальный или LDAP),
// и m.login.sso, если учетка привязана к провайдеру SSO. Без того и другого -
// m.login.password, как раньше (стадию не пройти, но клиент видит понятный отказ).
func (s *Server) uiaStages(ctx context.Context, userID string) []string {
	password := true

	if s.auth != nil {
		usable, err := s.auth.PasswordUsable(ctx, userID)
		if err == nil {
			password = usable
		}
	}

	var stages []string
	if password {
		stages = append(stages, "m.login.password")
	}

	if len(s.ssoProvidersFor(ctx, userID)) > 0 {
		stages = append(stages, ssoStage)
	}

	if len(stages) == 0 {
		stages = []string{"m.login.password"}
	}

	return stages
}

// handleSSOAuthFallback - GET /auth/m.login.sso/fallback/web?session=...[&idp_id=...]:
// страница подтверждения, затем вход у провайдера с привязкой к сессии UIA.
func (s *Server) handleSSOAuthFallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil || s.sso.accounts == nil {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgNotConfigured})

		return
	}

	sessionID := r.URL.Query().Get("session")

	_, binding, ok := s.uiaBindingOf(sessionID)
	if !ok {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgUIASessionUnknown})

		return
	}

	providers := s.ssoProvidersFor(r.Context(), binding.UserID)
	if len(providers) == 0 {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgNoLinkedProvider})

		return
	}

	if idpID := r.URL.Query().Get("idp_id"); idpID != "" {
		index := slices.IndexFunc(providers, func(p *sso.Provider) bool { return p.ID == idpID })
		if index < 0 {
			writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgNoLinkedProvider})

			return
		}

		s.startSSO(w, r, providers[index], "", sessionID)

		return
	}

	links := make([]ssoLink, 0, len(providers))
	for _, p := range providers {
		links = append(links, ssoLink{
			Name: p.Name,
			URL: "/_matrix/client/v3/auth/m.login.sso/fallback/web?session=" + url.QueryEscape(sessionID) +
				"&idp_id=" + url.QueryEscape(p.ID),
		})
	}

	writeSSOPage(w, http.StatusOK, pageUIAConfirm, ssoPageData{Links: links, Target: binding.Action})
}

// completeSSOUIA отмечает стадию m.login.sso, если провайдер подтвердил владельца
// сессии UIA (тот же user_id по user_external_ids).
func (s *Server) completeSSOUIA(
	w http.ResponseWriter, r *http.Request, provider *sso.Provider, identity sso.Identity, sessionID string,
) {
	_, binding, ok := s.uiaBindingOf(sessionID)
	if !ok {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgUIASessionUnknown})

		return
	}

	userID, err := s.sso.accounts.UserIDForIdentity(r.Context(), provider.ID, identity.Subject)
	if err != nil {
		s.log.ErrorContext(r.Context(), "sso: uia identity", "error", err)
		writeSSOPage(w, http.StatusInternalServerError, pageError, ssoPageData{Message: msgInternal})

		return
	}

	if userID == "" || userID != binding.UserID {
		s.log.WarnContext(r.Context(), "sso: uia identity mismatch", "idp_id", provider.ID, "user_id", binding.UserID)
		writeSSOPage(w, http.StatusForbidden, pageError, ssoPageData{Message: msgIdentityMismatch})

		return
	}

	if !s.uia.Complete(sessionID, ssoStage) {
		writeSSOPage(w, http.StatusBadRequest, pageError, ssoPageData{Message: msgUIASessionUnknown})

		return
	}

	writeSSOPage(w, http.StatusOK, pageUIADone, ssoPageData{})
}
