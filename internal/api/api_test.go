package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

// newTestServer собирает сервер без БД и с логгером в /dev/null.
//
// Rate limiting в общих тестах выключен: иначе один и тот же тестовый IP упирается
// в лимит (burst=3) на серии запросов. Тесты лимитов включают их явно —
// newServerWithRateLimit в ratelimit_test.go.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.RateLimit = config.RateLimit{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return New(cfg, nil, log)
}

func TestHealth(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body.Status != "ok" {
		t.Fatalf("status field = %q, want ok", body.Status)
	}

	// Версия и коммит - только на служебном листенере.
	if body.Version != "" || body.Commit != "" {
		t.Fatalf("публичный /health отдает версию: %+v", body)
	}

	rec = httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", http.NoBody))

	body = healthResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Version == "" {
		t.Fatalf("служебный /health без версии: %v %s", err, rec.Body.String())
	}
}

func TestReadyWithoutDatabase(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", http.NoBody))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body.Status != "unavailable" {
		t.Fatalf("status field = %q, want unavailable", body.Status)
	}

	// Публичный readiness без текста ошибки, служебный - с ним.
	if body.Checks["database"] != "fail" {
		t.Fatalf("database check = %q, want fail", body.Checks["database"])
	}

	rec = httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", http.NoBody))

	body = readyResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	if rec.Code != http.StatusServiceUnavailable || body.Checks["database"] == "" || body.Checks["database"] == "fail" {
		t.Fatalf("служебный readiness: %d %s", rec.Code, rec.Body.String())
	}
}

func TestVersions(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/client/versions", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body versionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if len(body.Versions) == 0 {
		t.Fatal("versions list is empty")
	}

	last := body.Versions[len(body.Versions)-1]
	if last != "v1.11" {
		t.Fatalf("last version = %q, want v1.11", last)
	}

	if body.UnstableFeatures == nil {
		t.Fatal("unstable_features must be an object, not null")
	}
}

func TestMatrixNotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/nope", http.NoBody))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body matrixError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body.Code != ErrCodeUnrecognized {
		t.Fatalf("errcode = %q, want %q", body.Code, ErrCodeUnrecognized)
	}
}

func TestNonMatrixNotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", http.NoBody))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want plain text for non-matrix paths", ct)
	}
}

func TestMatrixMethodNotAllowed(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_matrix/client/versions", http.NoBody))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestJSONContentType - ответы Matrix API отдают Content-Type ровно "application/json":
// Complement (TestLogin, TestRegistration) сравнивает заголовок целиком.
func TestJSONContentType(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/_matrix/client/versions"},
		{http.MethodGet, "/_matrix/client/v3/no-such-endpoint"},
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, http.NoBody))

		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want application/json", tc.method, tc.path, got)
		}
	}
}
