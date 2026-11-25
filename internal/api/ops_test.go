package api

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

func serveCode(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, http.NoBody))

	return rec.Code
}

// TestServiceEndpointsSeparated - /metrics и pprof только на
// служебном листенере; на публичном /metrics нет (вне dev/test).
func TestServiceEndpointsSeparated(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.RateLimit = config.RateLimit{}
	cfg.Server.OpsAddr = "127.0.0.1:9108"
	srv := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if code := serveCode(srv.Handler(), http.MethodGet, "/metrics"); code != http.StatusNotFound {
		t.Fatalf("публичный /metrics: %d, ожидали 404", code)
	}

	if code := serveCode(srv.Handler(), http.MethodGet, "/debug/pprof/"); code != http.StatusNotFound {
		t.Fatalf("публичный pprof: %d", code)
	}

	if code := serveCode(srv.OpsHandler(), http.MethodGet, "/metrics"); code != http.StatusOK {
		t.Fatalf("служебный /metrics: %d", code)
	}

	if code := serveCode(srv.OpsHandler(), http.MethodGet, "/debug/pprof/"); code != http.StatusNotFound {
		t.Fatalf("pprof без server.pprof: %d", code)
	}

	// prod без ops_addr: метрик на публичном листенере тоже нет.
	prod := newTestServer(t)
	if code := serveCode(prod.Handler(), http.MethodGet, "/metrics"); code != http.StatusNotFound {
		t.Fatalf("prod без ops_addr: /metrics %d", code)
	}

	cfg.Server.Pprof = true
	withPprof := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if code := serveCode(withPprof.OpsHandler(), http.MethodGet, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("pprof на служебном: %d", code)
	}

	if code := serveCode(withPprof.OpsHandler(), http.MethodGet, "/debug/pprof/goroutine?debug=1"); code != http.StatusOK {
		t.Fatalf("pprof goroutine: %d", code)
	}

	// dev без ops_addr - /metrics на основном листенере, как раньше.
	dev := config.Default()
	dev.Env = "dev"
	dev.RateLimit = config.RateLimit{}
	devSrv := New(dev, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if code := serveCode(devSrv.Handler(), http.MethodGet, "/metrics"); code != http.StatusOK {
		t.Fatalf("dev /metrics: %d", code)
	}
}

// syncBuffer - потокобезопасный буфер для логов.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// TestLogsWithoutSecrets - в логах запросов и ошибок нет
// Authorization, access_token из query, тел запросов, user id из пути. Логгер
// без маскирования: секреты не должны попадать в лог в самом коде.
func TestLogsWithoutSecrets(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{}
	srv := New(cfg, nil, logger)
	srv.SetFederationKey(&key)
	srv.keyLookup = func(string, string) (string, int64, error) { return "", 0, errors.New("no key") }

	send := func(method, path, body string, header map[string]string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}

		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	send(http.MethodGet, "/_matrix/client/v3/sync?access_token=QUERYSECRET1", "", nil)
	send(http.MethodGet, "/_matrix/client/v3/profile/@pathsecretuser:test.local", "",
		map[string]string{"Authorization": "Bearer HEADERSECRET2"})
	send(http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","user":"u","password":"BODYSECRET3"}`, nil)
	send(http.MethodPut, "/_matrix/federation/v1/send/txn1", `{"pdus":[{"content":{"body":"PDUSECRET5"}}]}`,
		map[string]string{
			"Authorization": `X-Matrix origin="remote.test",destination="test.local",key="ed25519:a",sig="SIGSECRET4"`,
			"Content-Type":  "application/json",
		})
	send(http.MethodGet, "/no/such/path/NOTFOUNDSECRET6", "", nil)

	out := logs.String()
	if !strings.Contains(out, "federation auth failed") || !strings.Contains(out, "http request") {
		t.Fatalf("ожидаемые записи не залогированы:\n%s", out)
	}

	for _, secret := range []string{
		"QUERYSECRET1", "HEADERSECRET2", "BODYSECRET3", "SIGSECRET4", "PDUSECRET5", "pathsecretuser", "NOTFOUNDSECRET6",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("в логе %q:\n%s", secret, out)
		}
	}

	if !strings.Contains(out, `"route":"/_matrix/client/v3/profile/{userId}"`) {
		t.Errorf("в логе нет шаблона маршрута:\n%s", out)
	}
}

// TestRedactLogAttr - маскирование секретов в атрибутах логгера процесса.
func TestRedactLogAttr(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{ReplaceAttr: RedactLogAttr}))

	logger.Info("probe",
		"authorization", "Bearer S1",
		"password", "S2",
		"uri", "/_matrix/client/v3/sync?since=x&access_token=S3",
		"error", errors.New(`header Authorization: Bearer S4; X-Matrix sig="S5"`),
		"note", "postgres://syngo:S6@db:5432/syngo",
		"user_id", "@alice:test.local",
	)

	out := logs.String()

	for _, secret := range []string{"S1", "S2", "S3", "S4", "S5", "S6"} {
		if strings.Contains(out, `"`+secret) || strings.Contains(out, secret+`"`) || strings.Contains(out, "="+secret) ||
			strings.Contains(out, ":"+secret) || strings.Contains(out, " "+secret) {
			t.Errorf("секрет %s в логе: %s", secret, out)
		}
	}

	if !strings.Contains(out, "@alice:test.local") || !strings.Contains(out, "since=x") {
		t.Fatalf("маскирование задело обычные поля: %s", out)
	}
}
