package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Коды ошибок Matrix для аутентификации.
const (
	ErrCodeMissingToken    = "M_MISSING_TOKEN"
	ErrCodeUnknownToken    = "M_UNKNOWN_TOKEN"
	ErrCodeForbidden       = "M_FORBIDDEN"
	ErrCodeUserInUse       = "M_USER_IN_USE"
	ErrCodeInvalidUsername = "M_INVALID_USERNAME"
	ErrCodeWeakPassword    = "M_WEAK_PASSWORD"
	ErrCodeUserDeactivated = "M_USER_DEACTIVATED"
)

// maxAuthBodySize ограничивает тело запросов аутентификации.
const maxAuthBodySize = 64 << 10

// sessionCtxKey — ключ контекста для проверенной сессии.
type sessionCtxKey struct{}

// bearerToken достаёт токен из заголовка Authorization или параметра access_token.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header != "" {
		scheme, token, found := strings.Cut(header, " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}

		return ""
	}

	return r.URL.Query().Get("access_token")
}

// authReady сообщает, сконфигурировано ли хранилище аутентификации.
// Без БД сервис отвечает 503, а не паникует и не притворяется, что логин работает.
func (s *Server) authReady(w http.ResponseWriter) bool {
	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Auth storage is not configured")

		return false
	}

	return true
}

// requireAuth — middleware: проверяет токен и кладёт сессию в контекст.
// Отсутствие токена — 401 M_MISSING_TOKEN, неизвестный/отозванный — 401 M_UNKNOWN_TOKEN.
// adminScopeRejected сообщает, относится ли токен к admin-контуру (ADR-0008):
// такие токены не действуют в CS-API.
func adminScopeRejected(scope string) bool {
	return scope == "admin" || scope == "admin:read"
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := sessionFrom(r.Context())

		// Токены admin-контура (scope admin/admin:read) не действуют в CS-API:
		// контуры разделены (ADR-0008 п.2), иначе admin-токен ходит как обычный.
		if adminScopeRejected(session.Scope) {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Admin token is not valid for the client API")

			return
		}

		next.ServeHTTP(w, r)
	}))
}

// optionalAuth - аутентификация по желанию: без токена запрос идет анонимно (сессии
// в контексте нет), с токеном - как requireAuth, и невалидный токен дает 401. Для
// публичных эндпоинтов, где сессия лишь уточняет ответ (GET профиля).
func (s *Server) optionalAuth(next http.Handler) http.Handler {
	authed := s.requireAuth(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) == "" {
			next.ServeHTTP(w, r)

			return
		}

		authed.ServeHTTP(w, r)
	})
}

// authenticate проверяет токен и кладёт сессию в контекст без ограничений по scope:
// общая часть для Client-Server API (requireAuth) и admin-контура (requireAdmin).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authReady(w) {
			return
		}

		token := bearerToken(r)
		if token == "" {
			writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

			return
		}

		session, err := s.auth.WhoAmI(r.Context(), token)
		if err != nil {
			s.writeAuthError(w, err)

			return
		}

		ctx := context.WithValue(r.Context(), sessionCtxKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionFrom достаёт сессию, положенную requireAuth.
func sessionFrom(ctx context.Context) (core.Session, bool) {
	session, ok := ctx.Value(sessionCtxKey{}).(core.Session)

	return session, ok
}

// writeAuthError переводит доменные ошибки аутентификации в коды Matrix.
func (s *Server) writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrRegistrationDisabled):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Registration has been disabled")
	case errors.Is(err, core.ErrInvalidLocalpart):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidUsername, "Invalid username")
	case errors.Is(err, core.ErrWeakPassword):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeWeakPassword, "Password is too short")
	case errors.Is(err, core.ErrUserInUse):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUserInUse, "User ID already taken")
	case errors.Is(err, core.ErrInvalidCredentials):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Invalid username or password")
	case errors.Is(err, core.ErrInvalidLoginToken):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Invalid login token")
	case errors.Is(err, core.ErrUnsupportedLogin):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Unsupported login type")
	case errors.Is(err, core.ErrUserDeactivated):
		writeMatrixError(w, http.StatusForbidden, ErrCodeUserDeactivated, "User has been deactivated")
	case errors.Is(err, core.ErrInvalidToken):
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnknownToken, "Invalid access token")
	case errors.Is(err, core.ErrNotFound):
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Not found")
	default:
		s.log.Error("api: auth failure", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
	}
}

// decodeBody читает JSON-тело запроса и различает M_NOT_JSON и M_BAD_JSON.
func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeBodyLimit(w, r, target, maxAuthBodySize)
}

// decodeBodyLimit - decodeBody с другим пределом размера тела.
func decodeBodyLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not read request body")

		return false
	}

	if len(body) == 0 {
		return true // пустое тело допустимо: у /register и /login все поля опциональны
	}

	// encoding/json молча заменяет невалидный UTF-8 на U+FFFD; Synapse такое тело не
	// принимает (400 M_NOT_JSON, Complement TestRequestEncodingFails).
	if !utf8.Valid(body) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Request body is not valid UTF-8")

		return false
	}

	if err := json.Unmarshal(body, target); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON")

		return false
	}

	return true
}

// registerAuth — шаг UIA в теле /register (у нас поддерживается только m.login.dummy).
type registerAuth struct {
	Type    string `json:"type"`
	Session string `json:"session"`
}

// registerRequest — тело POST /_matrix/client/v3/register.
type registerRequest struct {
	Username                 string        `json:"username"`
	Password                 string        `json:"password"`
	DeviceID                 string        `json:"device_id"`
	InitialDeviceDisplayName string        `json:"initial_device_display_name"`
	InhibitLogin             bool          `json:"inhibit_login"`
	RefreshToken             bool          `json:"refresh_token"`
	Auth                     *registerAuth `json:"auth"`
}

// uiaFlow — один доступный сценарий интерактивной аутентификации.
type uiaFlow struct {
	Stages []string `json:"stages"`
}

// uiaChallenge — ответ 401 на незавершённый /register (формат спеки Matrix).
type uiaChallenge struct {
	Flows   []uiaFlow      `json:"flows"`
	Params  map[string]any `json:"params"`
	Session string         `json:"session"`
	Code    string         `json:"errcode,omitempty"`
	Message string         `json:"error,omitempty"`
}

// registrationFlows — сценарии UIA, которые поддерживает сервер.
func registrationFlows() []uiaFlow {
	return []uiaFlow{{Stages: []string{"m.login.dummy"}}}
}

// registerResponse — тело успешного POST /register.
type registerResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	HomeServer  string `json:"home_server,omitempty"`
	ExpiresInMS *int64 `json:"expires_in_ms,omitempty"`
}

// loginRequest — тело POST /_matrix/client/v3/login.
type loginRequest struct {
	Type string `json:"type"`
	// Token - loginToken для m.login.token.
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
	DeviceID   string `json:"device_id"`
	Refresh    bool   `json:"refresh_token"`
	InitialDM  string `json:"initial_device_display_name"`
	Identifier *struct {
		Type string `json:"type"`
		User string `json:"user"`
	} `json:"identifier"`
}

// loginResponse — тело успешного POST /login.
type loginResponse struct {
	UserID       string `json:"user_id"`
	AccessToken  string `json:"access_token"`
	DeviceID     string `json:"device_id"`
	HomeServer   string `json:"home_server,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresInMS  *int64 `json:"expires_in_ms,omitempty"`
}

// whoAmIResponse — тело GET /account/whoami.
type whoAmIResponse struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id,omitempty"`
	IsGuest  bool   `json:"is_guest"`
}

// loginFlowsResponse — тело GET /_matrix/client/v3/login.
type loginFlowsResponse struct {
	Flows []loginFlow `json:"flows"`
}

type loginFlow struct {
	Type string `json:"type"`
	// IdentityProviders - провайдеры m.login.sso (кнопки «Continue with ...»).
	IdentityProviders []identityProvider `json:"identity_providers,omitempty"`
}

// registerUIA проверяет шаг UIA. Возвращает идентификатор сессии для удаления после
// успешной регистрации либо готовый challenge (401), если шаг не завершён.
func (s *Server) registerUIA(auth *registerAuth) (string, *uiaChallenge) {
	challenge := func(code, message string) *uiaChallenge {
		session, err := s.uia.Create(nil)
		if err != nil {
			return &uiaChallenge{
				Flows: registrationFlows(), Params: map[string]any{},
				Code: ErrCodeUnknown, Message: "Could not create UIA session",
			}
		}

		return &uiaChallenge{
			Flows: registrationFlows(), Params: map[string]any{}, Session: session.ID,
			Code: code, Message: message,
		}
	}

	if auth == nil || auth.Type == "" {
		return "", challenge("", "")
	}

	if auth.Type != "m.login.dummy" {
		return "", challenge(ErrCodeUnrecognized, "Unsupported authentication type")
	}

	if auth.Session == "" {
		return "", challenge("", "")
	}

	if _, ok := s.uia.Get(auth.Session); !ok {
		return "", challenge("", "Unknown UIA session")
	}

	return auth.Session, nil
}

// handleRegister обрабатывает POST /register и GET /register (список flow).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"flows": registrationFlows()})

		return
	}

	if !s.authReady(w) {
		return
	}

	var req registerRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if !s.auth.RegistrationAllowed() {
		s.writeAuthError(w, core.ErrRegistrationDisabled)

		return
	}

	// Тело проверяется до UIA, как в Synapse (check_username): недопустимое или
	// занятое имя - сразу 400, без лишнего шага auth (Complement
	// TestRegistration).
	if req.Username != "" {
		available, err := s.auth.Available(r.Context(), req.Username)
		if err != nil {
			s.writeAuthError(w, err)

			return
		}

		if !available {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUserInUse, "User ID already taken")

			return
		}
	}

	// UIA как в Synapse: первый запрос без auth получает 401 с flows и session,
	// повторный запрос с auth: {type: m.login.dummy, session} завершает регистрацию.
	session, challenge := s.registerUIA(req.Auth)
	if challenge != nil {
		writeJSON(w, http.StatusUnauthorized, *challenge)

		return
	}

	result, err := s.auth.Register(r.Context(), core.RegisterRequest{
		Localpart:           req.Username,
		Password:            req.Password,
		DeviceID:            req.DeviceID,
		DeviceDisplayName:   req.InitialDeviceDisplayName,
		InhibitLogin:        req.InhibitLogin,
		RequestRefreshToken: req.RefreshToken,
	})
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	s.uia.Delete(session)

	resp := registerResponse{
		UserID:      result.UserID,
		AccessToken: result.AccessToken,
		DeviceID:    result.DeviceID,
		ExpiresInMS: result.ExpiresInMS,
	}
	if result.AccessToken != "" {
		resp.HomeServer = s.cfg.Matrix.ServerName
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleLoginFlows обрабатывает GET /login. Порядок как в Synapse: m.login.sso и
// m.login.token (если настроен OIDC), затем m.login.password (если пароль
// где-то проверяется: локальная база или LDAP).
func (s *Server) handleLoginFlows(w http.ResponseWriter, _ *http.Request) {
	flows := s.ssoLoginFlows()

	if s.cfg.Auth.LocalPasswordsEnabled() || len(s.cfg.Auth.LDAP) > 0 || len(flows) == 0 {
		flows = append(flows, loginFlow{Type: "m.login.password"})
	}

	writeJSON(w, http.StatusOK, loginFlowsResponse{Flows: flows})
}

// handleLogin обрабатывает POST /login (m.login.password).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authReady(w) {
		return
	}

	var req loginRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Type == "" {
		req.Type = "m.login.password"
	}

	if req.Type == "m.login.token" {
		s.handleTokenLogin(w, r, &req)

		return
	}

	if req.Type != "m.login.password" {
		s.writeAuthError(w, core.ErrUnsupportedLogin)

		return
	}

	identifier := req.User
	if req.Identifier != nil {
		// Поддерживаем только m.id.user: остальные типы (3pid, phone) вне скоупа.
		if req.Identifier.Type != "" && req.Identifier.Type != "m.id.user" {
			s.writeAuthError(w, core.ErrUnsupportedLogin)

			return
		}

		identifier = req.Identifier.User
	}

	// rc_login.account в Synapse: ведро расходуется только неудачными попытками,
	// поэтому сначала смотрим состояние, а учитываем отказ ниже.
	accountKey := s.auth.LoginLimitKey(identifier)
	if s.loginAccountLimited(w, r, accountKey) {
		return
	}

	result, err := s.auth.Login(r.Context(), core.LoginRequest{
		Identifier:          identifier,
		Password:            req.Password,
		DeviceID:            req.DeviceID,
		DeviceDisplayName:   req.InitialDM,
		RequestRefreshToken: req.Refresh,
	})
	if err != nil {
		// Неудача по существующему аккаунту приближает блокировку.
		if errors.Is(err, core.ErrInvalidCredentials) && s.recordLoginFailure(w, r, accountKey) {
			return
		}

		s.writeAuthError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		UserID:       result.UserID,
		AccessToken:  result.AccessToken,
		DeviceID:     result.DeviceID,
		HomeServer:   s.cfg.Matrix.ServerName,
		RefreshToken: result.RefreshToken,
		ExpiresInMS:  result.ExpiresInMS,
	})
}

// loginAccountLimited проверяет лимит неудачных входов по учетке
// (rate_limit.login_account), не расходуя его; true - ответ 429 уже отдан.
// Ключ - Auth.LoginLimitKey: формы одного логина делят ведро.
func (s *Server) loginAccountLimited(w http.ResponseWriter, r *http.Request, key string) bool {
	limiter := s.limiters[limitLoginAccount]
	if limiter == nil || key == "" {
		return false
	}

	decision := limiter.Peek(key)
	if decision.Allowed {
		return false
	}

	s.writeLimitExceeded(w, r, limitLoginAccount, decision.RetryAfter)

	return true
}

// recordLoginFailure учитывает неудачный вход по учетке; true - лимит исчерпан
// и ответ 429 уже отдан.
func (s *Server) recordLoginFailure(w http.ResponseWriter, r *http.Request, key string) bool {
	limiter := s.limiters[limitLoginAccount]
	if limiter == nil || key == "" {
		return false
	}

	decision := limiter.Record(key)
	if decision.Allowed {
		return false
	}

	s.writeLimitExceeded(w, r, limitLoginAccount, decision.RetryAfter)

	return true
}

// handleWhoAmI обрабатывает GET /account/whoami.
func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	writeJSON(w, http.StatusOK, whoAmIResponse{
		UserID:   session.UserID,
		DeviceID: session.DeviceID,
		IsGuest:  false,
	})
}

// handleLogout обрабатывает POST /logout: отзывает текущий токен.
//
// Как в Synapse, выход удаляет и устройство сессии (с ключами E2EE, to-device и
// pushers): иначе оно оставалось в GET /devices (Complement TestLogout).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), bearerToken(r)); err != nil {
		s.writeAuthError(w, err)

		return
	}

	if session, ok := sessionFrom(r.Context()); ok {
		s.deleteLoggedOutDevices(r.Context(), session.UserID, session.DeviceID)
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// deleteLoggedOutDevices удаляет устройства после выхода: одно (deviceID) или все
// устройства пользователя (deviceID пустой). Сбой логируется: токены уже отозваны.
func (s *Server) deleteLoggedOutDevices(ctx context.Context, userID, deviceID string) {
	if s.profiles == nil {
		return
	}

	deviceIDs := []string{deviceID}

	if deviceID == "" {
		devices, err := s.profiles.Devices(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "api: list devices after logout failed", "user_id", userID, "error", err)

			return
		}

		deviceIDs = deviceIDs[:0]
		for i := range devices {
			deviceIDs = append(deviceIDs, devices[i].DeviceID)
		}
	}

	for _, id := range deviceIDs {
		if id == "" {
			continue
		}

		if err := s.profiles.DeleteDevice(ctx, userID, id); err != nil && !errors.Is(err, core.ErrNotFound) {
			s.log.WarnContext(ctx, "api: delete device after logout failed",
				"user_id", userID, "device_id", id, "error", err)

			continue
		}

		s.sendDeviceListUpdate(ctx, userID, []string{id}, true)
	}
}

// handleLogoutAll обрабатывает POST /logout/all: отзывает все токены пользователя.
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if _, err := s.auth.LogoutAll(r.Context(), session.UserID); err != nil {
		s.writeAuthError(w, err)

		return
	}

	s.deleteLoggedOutDevices(r.Context(), session.UserID, "")

	writeJSON(w, http.StatusOK, map[string]any{})
}

// registerAuthRoutes подключает эндпоинты аутентификации.
func (s *Server) registerAuthRoutes(r chi.Router) {
	// Лимиты по IP — parity с Synapse (rc_registration, rc_login.address).
	r.With(s.rateLimit(limitRegisterIP, ipKey)).Post("/register", s.handleRegister)
	r.Get("/register", s.handleRegister)
	r.Get("/login", s.handleLoginFlows)
	r.Get("/register/available", s.handleRegisterAvailable)
	r.Post("/refresh", s.handleRefresh)
	r.With(s.rateLimit(limitLoginIP, ipKey)).Post("/login", s.handleLogin)
	// SSO: редирект на провайдера и стадия UIA m.login.sso открываются
	// в браузере, без access-токена.
	r.Get("/login/sso/redirect", s.handleSSORedirect)
	r.Get("/login/sso/redirect/{idpId}", s.handleSSORedirect)
	r.Get("/auth/m.login.sso/fallback/web", s.handleSSOAuthFallback)

	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/logout", s.handleLogout)
		r.Post("/logout/all", s.handleLogoutAll)
		r.Get("/account/whoami", s.handleWhoAmI)
		r.Post("/account/password", s.handleChangePassword)
		r.Post("/account/deactivate", s.handleDeactivate)
		r.Post("/delete_devices", s.handleDeleteDevices)
	})
}

// handleRegisterAvailable — GET /register/available?username=...
func (s *Server) handleRegisterAvailable(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Auth storage is not configured")

		return
	}

	username := strings.TrimSpace(r.URL.Query().Get("username"))
	if username == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "username is required")

		return
	}

	available, err := s.auth.Available(r.Context(), username)
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	// Спека v1.11: занятое имя — 400 M_USER_IN_USE, а не 200 {"available":false}.
	if !available {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUserInUse, "User ID already taken")

		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"available": true})
}

// refreshRequest — тело POST /refresh.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// handleRefresh — POST /refresh: обмен refresh-token на новую пару токенов.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Auth storage is not configured")

		return
	}

	var req refreshRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	result, err := s.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		UserID:       result.UserID,
		AccessToken:  result.AccessToken,
		DeviceID:     result.DeviceID,
		RefreshToken: result.RefreshToken,
		ExpiresInMS:  result.ExpiresInMS,
	})
}

// deleteDevicesRequest — тело POST /delete_devices (UIA: пароль текущего пользователя).
type deleteDevicesRequest struct {
	Devices []string     `json:"devices"`
	Auth    *accountAuth `json:"auth"`
}

// handleDeleteDevices — POST /delete_devices: удаляет устройства и их токены пачкой.
func (s *Server) handleDeleteDevices(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.auth == nil || s.profiles == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var req deleteDevicesRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	if len(req.Devices) == 0 {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "devices is required")

		return
	}

	if !s.passwordUIA(w, r, session.UserID, req.Auth) {
		return
	}

	for _, deviceID := range req.Devices {
		if strings.TrimSpace(deviceID) == "" {
			continue
		}

		if err := s.profiles.DeleteDevice(r.Context(), session.UserID, deviceID); err != nil {
			// Уже удалённое устройство не считается ошибкой: спека разрешает идемпотентность.
			if errors.Is(err, core.ErrNotFound) {
				continue
			}

			s.log.ErrorContext(r.Context(), "api: delete device failed", "device_id", deviceID, "error", err)
			writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to delete device")

			return
		}

		s.sendDeviceListUpdate(r.Context(), session.UserID, []string{deviceID}, true)
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}
