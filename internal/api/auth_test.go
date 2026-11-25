package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
)

// fastHasher — предсказуемый хешер: HTTP-тесты не должны платить за argon2id.
type fastHasher struct{}

func (fastHasher) Hash(password string) (string, error) { return "plain:" + password, nil }

func (fastHasher) Verify(hash, password string) (bool, error) { return hash == "plain:"+password, nil }

// countingTokens выдаёт токены по счётчику.
type countingTokens struct{ n int }

func (c *countingTokens) NewToken() (plain string, hash []byte, err error) {
	c.n++

	plain = fmt.Sprintf("syt_test_%d", c.n)

	return plain, core.HashToken(plain), nil
}

// memStore — минимальное in-memory хранилище для HTTP-тестов.
type memStore struct {
	users    map[string]core.User
	sessions map[string]core.Session
}

func newMemStore() *memStore {
	return &memStore{
		users:    map[string]core.User{},
		sessions: map[string]core.Session{},
	}
}

// UpdatePasswordHash — заглушка для миграции bcrypt → argon2id.
func (m *memStore) UpdatePasswordHash(_ context.Context, userID, passwordHash string) error {
	for localpart, user := range m.users {
		if user.UserID == userID {
			user.PasswordHash = passwordHash
			m.users[localpart] = user

			return nil
		}
	}

	return core.ErrNotFound
}

// SessionByRefreshHash — упрощённо: первая сессия (тесты refresh не про хранилище).
func (m *memStore) SessionByRefreshHash(_ context.Context, _ []byte) (core.Session, error) {
	for _, session := range m.sessions {
		return session, nil
	}

	return core.Session{}, core.ErrNotFound
}

// RevokeRefresh — заглушка.
func (m *memStore) RevokeRefresh(_ context.Context, _ []byte) error { return nil }

func (m *memStore) CreateUser(_ context.Context, user core.User) (core.User, error) {
	if _, ok := m.users[user.Localpart]; ok {
		return core.User{}, core.ErrUserInUse
	}

	m.users[user.Localpart] = user

	return user, nil
}

func (m *memStore) UserByLocalpart(_ context.Context, localpart string) (core.User, error) {
	user, ok := m.users[localpart]
	if !ok {
		return core.User{}, fmt.Errorf("user: %w", core.ErrNotFound)
	}

	return user, nil
}

func (m *memStore) SearchUsers(_ context.Context, term string, limit int) ([]core.User, error) {
	var out []core.User

	for _, user := range m.users {
		if strings.Contains(strings.ToLower(user.Localpart), strings.ToLower(term)) ||
			strings.Contains(strings.ToLower(user.DisplayName), strings.ToLower(term)) {
			out = append(out, user)
		}
	}

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

func (m *memStore) UserByID(_ context.Context, userID string) (core.User, error) {
	for _, user := range m.users {
		if user.UserID == userID {
			return user, nil
		}
	}

	return core.User{}, fmt.Errorf("user: %w", core.ErrNotFound)
}

func (m *memStore) UpsertDevice(context.Context, core.Device) error { return nil }

func (m *memStore) CreateToken(_ context.Context, token core.TokenRecord) error {
	user := core.User{}
	for _, candidate := range m.users {
		if candidate.UserID == token.UserID {
			user = candidate
		}
	}

	m.sessions[hex.EncodeToString(token.TokenHash)] = core.Session{
		UserID:      token.UserID,
		Localpart:   user.Localpart,
		DisplayName: user.DisplayName,
		DeviceID:    token.DeviceID,
		Deactivated: user.Deactivated,
		ExpiresAt:   token.ExpiresAt,
	}

	return nil
}

func (m *memStore) SessionByTokenHash(_ context.Context, hash []byte) (core.Session, error) {
	session, ok := m.sessions[hex.EncodeToString(hash)]
	if !ok {
		return core.Session{}, fmt.Errorf("session: %w", core.ErrNotFound)
	}

	return session, nil
}

func (m *memStore) RevokeToken(_ context.Context, hash []byte) error {
	key := hex.EncodeToString(hash)
	if _, ok := m.sessions[key]; !ok {
		return core.ErrInvalidToken
	}

	delete(m.sessions, key)

	return nil
}

func (m *memStore) DeactivateUser(_ context.Context, userID string) error {
	for key, user := range m.users {
		if user.UserID != userID {
			continue
		}

		user.Deactivated = true
		m.users[key] = user

		return nil
	}

	return core.ErrNotFound
}

func (m *memStore) RevokeAllTokens(_ context.Context, userID string) (int64, error) {
	var count int64

	for key, session := range m.sessions {
		if session.UserID == userID {
			delete(m.sessions, key)

			count++
		}
	}

	return count, nil
}

// newAuthServer собирает сервер с in-memory аутентификацией (без PostgreSQL).
func newAuthServer(t *testing.T, opts core.AuthOptions, registrationOpen ...bool) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{} // лимиты проверяются отдельно (ratelimit_test.go)

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	opts.ServerName = "test.local"
	opts.RegistrationOpen = len(registrationOpen) == 0 || registrationOpen[0]

	if opts.MinPasswordLength == 0 {
		opts.MinPasswordLength = 8
	}

	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, opts)

	return server
}

// do выполняет запрос к роутеру и возвращает ответ.
func do(t *testing.T, server *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// decodeMatrixError разбирает тело ошибки Matrix.
func decodeMatrixError(t *testing.T, rec *httptest.ResponseRecorder) matrixError {
	t.Helper()

	var resp matrixError
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error body %q: %v", rec.Body.String(), err)
	}

	return resp
}

// uiaSession проходит первый шаг UIA и возвращает идентификатор сессии.
func uiaSession(t *testing.T, server *Server, body string) string {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register", body, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("шаг UIA: status %d, ожидали 401 (body %s)", rec.Code, rec.Body.String())
	}

	var challenge uiaChallenge
	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("unmarshal challenge: %v (body %s)", err, rec.Body.String())
	}

	if challenge.Session == "" || len(challenge.Flows) == 0 {
		t.Fatalf("challenge без сессии или flows: %s", rec.Body.String())
	}

	return challenge.Session
}

// registerBody собирает тело /register с завершённым шагом m.login.dummy.
func registerBody(localpart, password, session string) string {
	return fmt.Sprintf(
		`{"username":%q,"password":%q,"auth":{"type":"m.login.dummy","session":%q}}`,
		localpart, password, session,
	)
}

// registerUser регистрирует пользователя через UIA и возвращает выданную сессию.
func registerUser(t *testing.T, server *Server, localpart, password string) registerResponse {
	t.Helper()

	plain := fmt.Sprintf(`{"username":%q,"password":%q}`, localpart, password)
	session := uiaSession(t, server, plain)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		registerBody(localpart, password, session), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("register: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal register: %v", err)
	}

	return resp
}

func TestRegisterLoginWhoAmILogout(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	registered := registerUser(t, server, "alice", "correct horse")

	if registered.UserID != "@alice:test.local" || registered.AccessToken == "" || registered.DeviceID == "" {
		t.Fatalf("register response = %+v", registered)
	}

	// whoami по выданному токену.
	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", registered.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami: status %d, body %s", rec.Code, rec.Body.String())
	}

	var who whoAmIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &who); err != nil {
		t.Fatalf("unmarshal whoami: %v", err)
	}

	if who.UserID != registered.UserID || who.DeviceID != registered.DeviceID || who.IsGuest {
		t.Fatalf("whoami = %+v", who)
	}

	// Логин по identifier m.id.user с новым устройством.
	loginBody := `{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},` +
		`"password":"correct horse","device_id":"NEWDEVICE"}`
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/login", loginBody, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}

	var login loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}

	if login.DeviceID != "NEWDEVICE" || login.AccessToken == "" {
		t.Fatalf("login = %+v", login)
	}

	// logout отзывает текущий токен.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/logout", "", login.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", login.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("whoami после logout: status %d", rec.Code)
	}
}

func TestAuthMissingToken(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeMissingToken {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeMissingToken)
	}
}

func TestAuthUnknownToken(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", "syt_unknown")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeUnknownToken {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeUnknownToken)
	}
}

func TestAuthTokenFromQuery(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})
	registered := registerUser(t, server, "bob", "1234567890")

	rec := do(t, server, http.MethodGet,
		"/_matrix/client/v3/account/whoami?access_token="+registered.AccessToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterErrors(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{MinPasswordLength: 10})
	registerUser(t, server, "taken", "1234567890")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "занятый localpart", body: `{"username":"taken","password":"1234567890"}`, status: 400, code: ErrCodeUserInUse},
		{name: "плохой localpart", body: `{"username":"bad name","password":"1234567890"}`, status: 400, code: ErrCodeInvalidUsername},
		{name: "короткий пароль", body: `{"username":"fresh","password":"short"}`, status: 400, code: ErrCodeWeakPassword},
		{name: "битый JSON", body: `{"username":`, status: 400, code: ErrCodeBadJSON},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := tc.body

			// Пароль проверяется после шага UIA; битый JSON и имя (как в Synapse) -
			// до него.
			if tc.code == ErrCodeWeakPassword {
				session := uiaSession(t, server, tc.body)
				body = strings.Replace(tc.body, "}", fmt.Sprintf(`,"auth":{"type":"m.login.dummy","session":%q}}`, session), 1)
			}

			rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register", body, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, ожидали %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}

			if errResp := decodeMatrixError(t, rec); errResp.Code != tc.code {
				t.Fatalf("errcode = %q, ожидали %q", errResp.Code, tc.code)
			}
		})
	}
}

// TestRegisterUIA проверяет parity с Synapse: первый /register без auth — 401 с flows
// и session, повтор с auth m.login.dummy — 200.
func TestRegisterUIA(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		`{"username":"uia","password":"1234567890"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401", rec.Code)
	}

	var challenge uiaChallenge
	if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("unmarshal challenge: %v", err)
	}

	if len(challenge.Flows) != 1 || len(challenge.Flows[0].Stages) != 1 ||
		challenge.Flows[0].Stages[0] != "m.login.dummy" {
		t.Fatalf("flows = %+v, ожидали stages [m.login.dummy]", challenge.Flows)
	}

	if challenge.Session == "" {
		t.Fatal("в challenge нет session")
	}

	// Неизвестный тип аутентификации — снова 401 с новым session.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		`{"username":"uia","password":"1234567890","auth":{"type":"m.login.email.identity"}}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("неизвестный flow: status = %d, ожидали 401", rec.Code)
	}

	var unsupported uiaChallenge
	if err := json.Unmarshal(rec.Body.Bytes(), &unsupported); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if unsupported.Code != ErrCodeUnrecognized {
		t.Fatalf("errcode = %q, ожидали %q", unsupported.Code, ErrCodeUnrecognized)
	}

	// Мусорный session — 401 с новой сессией.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		registerBody("uia", "1234567890", "bogus"), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("неизвестный session: status = %d, ожидали 401", rec.Code)
	}

	// Правильное прохождение шага — 200 и сессия.
	registered := registerUser(t, server, "uia", "1234567890")
	if registered.UserID != "@uia:test.local" || registered.AccessToken == "" {
		t.Fatalf("register через UIA = %+v", registered)
	}
}

func TestLoginErrors(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})
	registerUser(t, server, "carol", "1234567890")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "неверный пароль", body: `{"type":"m.login.password","identifier":{"type":"m.id.user","user":"carol"},"password":"nope"}`, status: 403, code: ErrCodeForbidden},
		{name: "неизвестный пользователь", body: `{"type":"m.login.password","user":"ghost","password":"x"}`, status: 403, code: ErrCodeForbidden},
		{name: "неподдерживаемый тип", body: `{"type":"m.login.sso"}`, status: 400, code: ErrCodeUnknown},
		{name: "неподдерживаемый identifier", body: `{"type":"m.login.password","identifier":{"type":"m.id.phone","user":"carol"},"password":"1234567890"}`, status: 400, code: ErrCodeUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login", tc.body, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, ожидали %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}

			if errResp := decodeMatrixError(t, rec); errResp.Code != tc.code {
				t.Fatalf("errcode = %q, ожидали %q", errResp.Code, tc.code)
			}
		})
	}
}

func TestRegistrationDisabled(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{}, false)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		`{"username":"dave","password":"1234567890"}`, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, ожидали 403", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeForbidden {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeForbidden)
	}
}

func TestAuthFlows(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/login", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "m.login.password") {
		t.Fatalf("GET /login = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/register", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "m.login.dummy") {
		t.Fatalf("GET /register = %d %s", rec.Code, rec.Body.String())
	}
}

func TestLogoutAll(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})
	first := registerUser(t, server, "erin", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","user":"erin","password":"1234567890","device_id":"SECOND"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/logout/all", "", first.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout/all: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", first.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("whoami после logout/all: status %d", rec.Code)
	}
}

func TestAuthWithoutStorage(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","user":"alice","password":"x"}`, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, ожидали 503 без БД", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeUnknown {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeUnknown)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	server := newAuthServer(t, core.AuthOptions{
		AccessTokenTTL: time.Minute,
		Now:            func() time.Time { return now },
	})
	registered := registerUser(t, server, "frank", "1234567890")

	// Тот же сервер, но время ушло вперёд: сервис проверяет срок при чтении сессии.
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName:        "test.local",
		RegistrationOpen:  true,
		MinPasswordLength: 8,
		AccessTokenTTL:    time.Minute,
		Now:               func() time.Time { return now.Add(time.Hour) },
	})

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/whoami", "", registered.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401 для просроченного токена", rec.Code)
	}
}
