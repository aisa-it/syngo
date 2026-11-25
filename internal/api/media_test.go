package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/media"
)

// memMediaRepo - метаданные медиа в памяти для тестов API. Мьютекс нужен: download
// с timeout_ms опрашивает репозиторий параллельно с PUT.
type memMediaRepo struct {
	mu    sync.Mutex
	items map[string]media.Meta
}

func (m *memMediaRepo) Insert(_ context.Context, meta media.Meta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items[meta.ContentURI] = meta

	return nil
}

func (m *memMediaRepo) ByID(_ context.Context, serverName, mediaID string) (media.Meta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.items {
		meta := m.items[i]
		if meta.ServerName == serverName && meta.MediaID == mediaID {
			return meta, nil
		}
	}

	return media.Meta{}, media.ErrNotFound
}

func (m *memMediaRepo) TouchAccess(context.Context, string) error { return nil }

func (m *memMediaRepo) InsertPending(_ context.Context, meta media.Meta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items[meta.ContentURI] = meta

	return nil
}

// UpdateUploaded повторяет условный UPDATE хранилища: заполнить можно только
// незагруженный placeholder.
func (m *memMediaRepo) UpdateUploaded(_ context.Context, meta media.Meta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.items[meta.ContentURI]
	if !ok {
		return media.ErrNotFound
	}

	if current.Uploaded {
		return media.ErrAlreadyUploaded
	}

	m.items[meta.ContentURI] = meta

	return nil
}

func (m *memMediaRepo) PendingStats(_ context.Context, uploader string, since time.Time) (int, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var (
		count  int
		oldest time.Time
	)

	for uri := range m.items {
		meta := m.items[uri]
		if meta.Uploader != uploader || meta.Uploaded || meta.CreatedAt.Before(since) {
			continue
		}

		count++

		if oldest.IsZero() || meta.CreatedAt.Before(oldest) {
			oldest = meta.CreatedAt
		}
	}

	return count, oldest, nil
}

func (m *memMediaRepo) DeleteExpiredPending(_ context.Context, uploader string, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for uri := range m.items {
		if meta := m.items[uri]; meta.Uploader == uploader && !meta.Uploaded && meta.CreatedAt.Before(before) {
			delete(m.items, uri)
		}
	}

	return nil
}

// setCreatedAt сдвигает время создания записи (проверка срока placeholder и freeze).
func (m *memMediaRepo) setCreatedAt(contentURI string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	meta := m.items[contentURI]
	meta.CreatedAt = at
	m.items[contentURI] = meta
}

// newMediaServer — сервер с рабочим медиа-сервисом (локальное хранилище во временном каталоге).
// Лимиты по умолчанию выключены: их включает newMediaServerLimited.
func newMediaServer(t *testing.T, maxBytes int64) *Server {
	t.Helper()

	return newMediaServerWithLimits(t, maxBytes, config.RateLimit{})
}

// newMediaServerWithLimits — то же, но с заданными лимитами (они читаются при сборке роутера).
func newMediaServerWithLimits(t *testing.T, maxBytes int64, rl config.RateLimit) *Server {
	t.Helper()

	server, _ := newMediaServerRepo(t, maxBytes, rl)

	return server
}

// newMediaServerRepo - то же, но отдает и репозиторий метаданных (тесты
// сдвигают created_at и смотрят на placeholder).
func newMediaServerRepo(t *testing.T, maxBytes int64, rl config.RateLimit) (*Server, *memMediaRepo) {
	t.Helper()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = rl

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	objects, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"))
	if err != nil {
		t.Fatalf("local store: %v", err)
	}

	repo := &memMediaRepo{items: map[string]media.Meta{}}
	server.media = media.NewService(repo, objects, "test.local", maxBytes)

	// Аутентификация без БД — как в auth_test.go.
	opts := core.AuthOptions{ServerName: "test.local", RegistrationOpen: true, MinPasswordLength: 8}
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, opts)

	return server, repo
}

func upload(t *testing.T, server *Server, token, path, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// TestMediaUploadDownloadTwoSessions - A загрузил, B скачал.
func TestMediaUploadDownloadTwoSessions(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)

	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	payload := "media round-trip"

	rec := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload?filename=note.txt", "text/plain", strings.NewReader(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status %d, body %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	if !strings.HasPrefix(uploaded.ContentURI, "mxc://test.local/") {
		t.Fatalf("content_uri = %q", uploaded.ContentURI)
	}

	mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	// Второй пользователь скачивает своим токеном через v1 (MSC3916).
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/media/download/test.local/"+mediaID, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+bob.AccessToken)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("download: status %d, body %s", rec.Code, rec.Body.String())
	}

	if got := rec.Body.String(); got != payload {
		t.Fatalf("скачали %q, ожидали %q", got, payload)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("content-type = %q", ct)
	}

	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "note.txt") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestMediaUploadTooLarge(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 4)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "text/plain",
		strings.NewReader("definitely longer than four bytes"))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, ожидали 413 (body %s)", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), ErrCodeTooLarge) {
		t.Fatalf("тело должно содержать %s: %s", ErrCodeTooLarge, rec.Body.String())
	}
}

func TestMediaUploadRequiresAuth(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)

	rec := upload(t, server, "", "/_matrix/media/v3/upload", "text/plain", strings.NewReader("x"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401", rec.Code)
	}
}

func TestMediaDownloadUnknown(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)

	req := httptest.NewRequest(http.MethodGet, "/_matrix/media/v3/download/test.local/unknown", http.NoBody)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, ожидали 404", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), ErrCodeNotFound) {
		t.Fatalf("тело должно содержать %s: %s", ErrCodeNotFound, rec.Body.String())
	}
}

func TestMediaConfigAndLegacyPath(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1234)
	alice := registerUser(t, server, "alice", "1234567890")

	// config требует токен на обоих префиксах.
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/media/config", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+alice.AccessToken)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("config: status %d", rec.Code)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	if cfg["m.upload.size"] != float64(1234) {
		t.Fatalf("m.upload.size = %v, ожидали 1234", cfg["m.upload.size"])
	}

	// Легаси-алиас /_matrix/media/v3/config должен отвечать так же.
	req = httptest.NewRequest(http.MethodGet, "/_matrix/media/v3/config", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+alice.AccessToken)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("legacy config: status %d", rec.Code)
	}

	// v1-путь загрузки работает наравне с v3.
	rec = upload(t, server, alice.AccessToken, "/_matrix/client/v1/media/upload", "text/plain", strings.NewReader("v1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 upload: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestMediaRateLimit — rc_media_create реально режет upload по пользователю.
func TestMediaRateLimit(t *testing.T) {
	t.Parallel()

	server := newMediaServerWithLimits(t, 1<<20, config.RateLimit{
		MediaUpload: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 1},
	})

	alice := registerUser(t, server, "alice", "1234567890")

	first := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "text/plain", strings.NewReader("1"))
	if first.Code != http.StatusOK {
		t.Fatalf("первый upload: status %d, body %s", first.Code, first.Body.String())
	}

	second := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "text/plain", strings.NewReader("2"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("второй upload должен дать 429, получили %d", second.Code)
	}
}
