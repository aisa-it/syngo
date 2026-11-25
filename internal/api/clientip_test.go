package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
)

// rate limit за TLS-прокси - доверенные прокси, ключи лимитов, лимит /sync.

// proxyAddr - адрес TLS-прокси перед сервисом в тестах.
const proxyAddr = "10.0.0.5:44321"

// newProxiedServer - сервер со строгим лимитом логина по IP и доверенной сетью прокси.
func newProxiedServer(t *testing.T, rl config.RateLimit, trusted ...string) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = rl
	cfg.Server.TrustedProxies = trusted

	return New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// postLoginVia - POST /login с адресом соединения и заголовками прокси.
func postLoginVia(server *Server, remoteAddr string, headers map[string]string) int {
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/login", http.NoBody)
	req.RemoteAddr = remoteAddr

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code
}

// TestRateLimitBehindTrustedProxyPerClient - два клиента за доверенным прокси
// получают разные ведра: один не исчерпывает лимит логина всем остальным.
func TestRateLimitBehindTrustedProxyPerClient(t *testing.T) {
	t.Parallel()

	server := newProxiedServer(t, strictLoginLimit(), "10.0.0.0/8")

	alice := map[string]string{"X-Forwarded-For": "203.0.113.10"}
	bob := map[string]string{"X-Forwarded-For": "198.51.100.20"}

	if code := postLoginVia(server, proxyAddr, alice); code == http.StatusTooManyRequests {
		t.Fatal("первый логин первого клиента не должен ограничиваться")
	}

	if code := postLoginVia(server, proxyAddr, bob); code == http.StatusTooManyRequests {
		t.Fatal("второй клиент за тем же прокси получил 429: ведро общее на всех")
	}

	if code := postLoginVia(server, proxyAddr, alice); code != http.StatusTooManyRequests {
		t.Fatalf("повтор первого клиента: %d, ожидали 429", code)
	}

	// X-Real-IP (nginx proxy_set_header X-Real-IP) - тоже отдельное ведро.
	if code := postLoginVia(server, proxyAddr, map[string]string{"X-Real-IP": "192.0.2.77"}); code == http.StatusTooManyRequests {
		t.Fatal("клиент из X-Real-IP получил 429 чужого ведра")
	}
}

// TestRateLimitSpoofedForwardedForIgnored - X-Forwarded-For от недоверенного
// адреса не меняет IP: подделкой заголовка лимит не обходится.
func TestRateLimitSpoofedForwardedForIgnored(t *testing.T) {
	t.Parallel()

	for name, trusted := range map[string][]string{
		"прокси не настроены":      nil,
		"клиент не из сети прокси": {"10.0.0.0/8"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := newProxiedServer(t, strictLoginLimit(), trusted...)

			if code := postLoginVia(server, "203.0.113.10:1000",
				map[string]string{"X-Forwarded-For": "198.51.100.1"}); code == http.StatusTooManyRequests {
				t.Fatal("первый логин не должен ограничиваться")
			}

			// Тот же адрес соединения с другим X-Forwarded-For и X-Real-IP - то же ведро.
			if code := postLoginVia(server, "203.0.113.10:1001", map[string]string{
				"X-Forwarded-For": "198.51.100.2",
				"X-Real-IP":       "198.51.100.3",
			}); code != http.StatusTooManyRequests {
				t.Fatalf("подделка заголовков обошла лимит: %d, ожидали 429", code)
			}
		})
	}
}

// TestRateLimitForwardedForChainSpoof - клиент за доверенным прокси дописывает
// в X-Forwarded-For чужие адреса слева: ключом остается его адрес, который
// дописал прокси (крайний правый недоверенный).
func TestRateLimitForwardedForChainSpoof(t *testing.T) {
	t.Parallel()

	server := newProxiedServer(t, strictLoginLimit(), "10.0.0.0/8")

	if code := postLoginVia(server, proxyAddr,
		map[string]string{"X-Forwarded-For": "1.1.1.1, 203.0.113.10"}); code == http.StatusTooManyRequests {
		t.Fatal("первый логин не должен ограничиваться")
	}

	if code := postLoginVia(server, proxyAddr,
		map[string]string{"X-Forwarded-For": "2.2.2.2, 203.0.113.10"}); code != http.StatusTooManyRequests {
		t.Fatalf("подмена левой части цепочки обошла лимит: %d, ожидали 429", code)
	}
}

// TestRequestLogClientIP - в логе запроса remote_addr - IP клиента за
// доверенным прокси, адрес прокси - отдельным полем proxy_addr.
func TestRequestLogClientIP(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	server := New(cfg, nil, slog.New(slog.NewJSONHandler(&buf, nil)))

	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/versions", http.NoBody)
	req.RemoteAddr = proxyAddr
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)

	var entry struct {
		Msg        string `json:"msg"`
		RemoteAddr string `json:"remote_addr"`
		ProxyAddr  string `json:"proxy_addr"`
	}

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Msg == "http request" {
			break
		}
	}

	if entry.Msg != "http request" {
		t.Fatalf("нет записи http request в логе: %s", buf.String())
	}

	if entry.RemoteAddr != "203.0.113.10" || entry.ProxyAddr != "10.0.0.5" {
		t.Fatalf("remote_addr=%q proxy_addr=%q, ожидали клиента 203.0.113.10 за прокси 10.0.0.5",
			entry.RemoteAddr, entry.ProxyAddr)
	}
}

// TestLoginAccountLimiterKeyNormalized - "alice", "Alice", " alice" и
// "@alice:test.local" - одна учетка и одно ведро rate_limit.login_account.
func TestLoginAccountLimiterKeyNormalized(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{LoginAccount: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 3}}
	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName: "test.local", RegistrationOpen: true, MinPasswordLength: 8,
	})

	registerUser(t, server, "alice", "correct horse battery")

	login := func(id, password string) int {
		body := fmt.Sprintf(`{"type":"m.login.password","identifier":{"type":"m.id.user","user":%q},"password":%q}`, id, password)

		return do(t, server, http.MethodPost, "/_matrix/client/v3/login", body, "").Code
	}

	for _, id := range []string{"alice", "Alice", " ALICE "} {
		if code := login(id, "wrong-password"); code != http.StatusForbidden {
			t.Fatalf("неверный пароль для %q: %d, ожидали 403", id, code)
		}
	}

	// Ведро учетки исчерпано тремя формами логина: полная форма user_id тоже под лимитом.
	if code := login("@alice:test.local", "correct horse battery"); code != http.StatusTooManyRequests {
		t.Fatalf("@alice:test.local после 3 неудач другими формами: %d, ожидали 429", code)
	}

	// Другая учетка не затронута.
	registerUser(t, server, "bob", "correct horse battery")

	if code := login("bob", "correct horse battery"); code != http.StatusOK {
		t.Fatalf("bob: %d, ожидали 200", code)
	}
}

// syncLimitServer - сервер без БД с in-memory auth и лимитом /sync.
func syncLimitServer(t *testing.T, sync config.RateLimitAction) *Server {
	t.Helper()

	server := newProxiedServer(t, config.RateLimit{Sync: sync})
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName: "test.local", RegistrationOpen: true, MinPasswordLength: 8,
	})

	return server
}

func getSync(server *Server, token, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/sync"+query, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// TestSyncRateLimitPolling - частый /sync без timeout получает 429
// M_LIMIT_EXCEEDED с retry_after_ms и Retry-After; лимит - по устройству.
func TestSyncRateLimitPolling(t *testing.T) {
	t.Parallel()

	server := syncLimitServer(t, config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 2})
	alice := registerUser(t, server, "alice", "correct horse battery")

	for i := range 2 {
		if rec := getSync(server, alice.AccessToken, ""); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("/sync %d в пределах burst получил 429", i+1)
		}
	}

	rec := getSync(server, alice.AccessToken, "?timeout=0")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("третий /sync без ожидания: %d, ожидали 429", rec.Code)
	}

	var body limitErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429: %v", err)
	}

	if body.Code != ErrCodeLimitExceeded || body.RetryAfterMS <= 0 {
		t.Fatalf("тело 429 = %+v", body)
	}

	if retry, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || retry < 1 {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}

	// Другое устройство того же пользователя - свое ведро.
	second := do(t, server, http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},"password":"correct horse battery"}`, "")
	if second.Code != http.StatusOK {
		t.Fatalf("login второго устройства: %d %s", second.Code, second.Body.String())
	}

	var login loginResponse
	if err := json.Unmarshal(second.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}

	if rec := getSync(server, login.AccessToken, ""); rec.Code == http.StatusTooManyRequests {
		t.Fatal("второе устройство получило 429 по ведру первого")
	}
}

// TestSyncRateLimitLongPollWaits - long-poll сверх лимита не отклоняется, а
// выдерживается до освобождения ведра (Element будит long-poll часто); если
// ждать дольше timeout - 429.
func TestSyncRateLimitLongPollWaits(t *testing.T) {
	t.Parallel()

	// Ведро на один запрос, освобождается за 50 мс.
	server := syncLimitServer(t, config.RateLimitAction{Enabled: true, PerSecond: 20, Burst: 1})
	alice := registerUser(t, server, "alice", "correct horse battery")

	if rec := getSync(server, alice.AccessToken, "?timeout=30000"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("первый long-poll получил 429")
	}

	started := time.Now()
	if rec := getSync(server, alice.AccessToken, "?timeout=30000"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("long-poll сверх лимита получил 429 вместо ожидания")
	}

	if waited := time.Since(started); waited < 20*time.Millisecond {
		t.Fatalf("long-poll сверх лимита не выдержан: %v", waited)
	}

	// Опрос без ожидания сразу после - 429.
	if rec := getSync(server, alice.AccessToken, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("/sync без timeout сверх лимита: %d, ожидали 429", rec.Code)
	}

	// Ждать дольше timeout - 429, а не зависание.
	slow := syncLimitServer(t, config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 1})
	bob := registerUser(t, slow, "bob", "correct horse battery")

	_ = getSync(slow, bob.AccessToken, "?timeout=100")

	if rec := getSync(slow, bob.AccessToken, "?timeout=100"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("long-poll с ожиданием дольше timeout: %d, ожидали 429", rec.Code)
	}
}
