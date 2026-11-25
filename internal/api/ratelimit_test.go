package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
)

// newServerWithRateLimit собирает сервер с заданными лимитами и без БД.
func newServerWithRateLimit(t *testing.T, rl config.RateLimit) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.RateLimit = rl

	return New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// strictLoginLimit — лимит «один запрос навсегда» для проверки отказа.
func strictLoginLimit() config.RateLimit {
	return config.RateLimit{
		LoginIP: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 1},
	}
}

func postLogin(server *Server, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", http.NoBody)
	req.RemoteAddr = remoteAddr

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

func TestRateLimitLoginByIP(t *testing.T) {
	t.Parallel()

	server := newServerWithRateLimit(t, strictLoginLimit())

	first := postLogin(server, "203.0.113.10:5555")
	if first.Code == http.StatusTooManyRequests {
		t.Fatalf("первый логин не должен ограничиваться, получили %d", first.Code)
	}

	second := postLogin(server, "203.0.113.10:6666")
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("второй логин с того же IP должен дать 429, получили %d", second.Code)
	}

	var body limitErrorBody
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}

	if body.Code != ErrCodeLimitExceeded {
		t.Fatalf("errcode = %q, ожидали %q", body.Code, ErrCodeLimitExceeded)
	}

	if body.RetryAfterMS <= 0 {
		t.Fatalf("retry_after_ms должен быть > 0, получили %d", body.RetryAfterMS)
	}

	if got := second.Header().Get("Retry-After"); got == "" {
		t.Fatal("заголовок Retry-After обязателен при 429")
	}
}

func TestRateLimitDifferentIPsIndependent(t *testing.T) {
	t.Parallel()

	server := newServerWithRateLimit(t, strictLoginLimit())

	if rec := postLogin(server, "203.0.113.10:1000"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("первый IP не должен быть ограничен")
	}

	// Другой IP — своё ведро.
	if rec := postLogin(server, "198.51.100.20:1000"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("второй IP не должен зависеть от лимита первого")
	}

	// Повтор с первого IP — уже отказ.
	if rec := postLogin(server, "203.0.113.10:1001"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("повтор с первого IP должен дать 429, получили %d", rec.Code)
	}
}

func TestRateLimitDisabledAllowsAll(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.RateLimit = config.RateLimit{} // все лимиты выключены

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for range 20 {
		if rec := postLogin(server, "203.0.113.10:1000"); rec.Code == http.StatusTooManyRequests {
			t.Fatal("при выключенных лимитах 429 быть не должно")
		}
	}
}

// sendLikeRoute - имитация маршрута /send: проверяется сам middleware с ключом «пользователь или IP».
func sendLikeRoute(server *Server) http.Handler {
	return server.rateLimit(limitSend, userOrIPKey)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestRateLimitSendLikeRouteByUser(t *testing.T) {
	t.Parallel()

	rl := config.RateLimit{Send: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 2}}
	server := newServerWithRateLimit(t, rl)
	handler := sendLikeRoute(server)

	call := func(addr string) int {
		req := httptest.NewRequest(http.MethodPut, "/_matrix/client/v3/rooms/!r:test/send/m.room.message/t1", http.NoBody)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec.Code
	}

	if code := call("203.0.113.10:1000"); code != http.StatusOK {
		t.Fatalf("1-й send должен пройти, получили %d", code)
	}

	if code := call("203.0.113.10:1001"); code != http.StatusOK {
		t.Fatalf("2-й send должен пройти (burst=2), получили %d", code)
	}

	if code := call("203.0.113.10:1002"); code != http.StatusTooManyRequests {
		t.Fatalf("3-й send должен дать 429, получили %d", code)
	}

	// Другой пользователь/IP — своё ведро.
	if code := call("198.51.100.20:1000"); code != http.StatusOK {
		t.Fatalf("другой ключ должен проходить независимо, получили %d", code)
	}
}

func TestRateLimitMediaUploadRoute(t *testing.T) {
	t.Parallel()

	rl := config.RateLimit{MediaUpload: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 1}}
	server := newServerWithRateLimit(t, rl)
	handler := server.rateLimit(limitMediaUpload, userOrIPKey)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v1/media/upload", http.NoBody)
	req.RemoteAddr = "203.0.113.10:1000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("первый upload должен пройти, получили %d", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/_matrix/client/v1/media/upload", http.NoBody)
	req2.RemoteAddr = "203.0.113.10:1001"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("второй upload должен дать 429, получили %d", rec2.Code)
	}
}

// TestRateLimitLoginByAccount — parity с rc_login.account: неудачные попытки по
// одному аккаунту с разных IP приближают блокировку, а успешные — нет.
func TestRateLimitLoginByAccount(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{
		LoginAccount: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 3},
	}

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName:        "test.local",
		RegistrationOpen:  true,
		MinPasswordLength: 8,
	})

	registerUser(t, server, "victim", "correct horse battery")

	failedLogin := func(addr, password string) int {
		body := fmt.Sprintf(`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"victim"},"password":%q}`, password)
		req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = addr

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		return rec.Code
	}

	// Три неверных пароля с разных IP: ведро аккаунта исчерпано.
	for i := range 3 {
		if code := failedLogin(fmt.Sprintf("203.0.113.%d:1000", 10+i), "wrong"); code != http.StatusForbidden {
			t.Fatalf("неудачная попытка %d: status %d, ожидали 403", i+1, code)
		}
	}

	// Четвёртая попытка с нового IP — 429 уже на входе, пароль даже не проверяется.
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login",
		strings.NewReader(`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"victim"},"password":"correct horse battery"}`))
	req.RemoteAddr = "198.51.100.99:1000"

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("после 3 неудач аккаунт должен быть ограничен: status %d", rec.Code)
	}

	var body limitErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode 429: %v", err)
	}

	if body.Code != ErrCodeLimitExceeded || body.RetryAfterMS <= 0 {
		t.Fatalf("тело 429 = %+v", body)
	}

	// Другой аккаунт не затронут.
	registerUser(t, server, "other", "correct horse battery")
}
