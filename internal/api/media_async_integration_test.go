//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAsyncMediaUpload - MSC2246 на живой БД: /create выдает
// серверный id, до загрузки download - 504, PUT байтов - 200 {}, повторная
// загрузка - 409 M_CANNOT_OVERWRITE_MEDIA, чужой пользователь - 403.
func TestAsyncMediaUpload(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "asyncmedia", "async-media-pass")
	other := registerPusherProbe(t, srv, "asyncother", "async-other-pass")

	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")

		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(http.MethodPost, "/_matrix/media/v1/create", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		ContentURI      string `json:"content_uri"`
		UnusedExpiresAt int64  `json:"unused_expires_at"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if !strings.HasPrefix(created.ContentURI, "mxc://test.local/") || created.UnusedExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("create: %s", rec.Body.String())
	}

	mediaID := strings.TrimPrefix(created.ContentURI, "mxc://test.local/")
	base := "/_matrix/media/v3/upload/test.local/" + mediaID
	download := "/_matrix/client/v1/media/download/test.local/" + mediaID

	// Скачивание до загрузки — 504 M_NOT_YET_UPLOADED.
	rec = do(http.MethodGet, download+"?timeout_ms=0", "", token)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), "M_NOT_YET_UPLOADED") {
		t.Fatalf("download до загрузки: %d %s", rec.Code, rec.Body.String())
	}

	if rec = do(http.MethodPut, base, "evil", other); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT чужим пользователем: %d %s", rec.Code, rec.Body.String())
	}

	// Загрузка байтов.
	if rec = do(http.MethodPut, base+"?filename=test.bin", "payload-bytes", token); rec.Code != http.StatusOK {
		t.Fatalf("upload to: %d %s", rec.Code, rec.Body.String())
	}

	// Теперь скачивается.
	rec = do(http.MethodGet, download, "", token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "payload-bytes") {
		t.Fatalf("download после загрузки: %d %s", rec.Code, rec.Body.String())
	}

	// Повторная загрузка — 409.
	if rec = do(http.MethodPut, base, "again", token); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "M_CANNOT_OVERWRITE_MEDIA") {
		t.Fatalf("повторная загрузка: %d %s", rec.Code, rec.Body.String())
	}

	// Новые медиа через неаутентифицированный legacy-путь не отдаются (freeze).
	if rec = do(http.MethodGet, "/_matrix/media/v3/download/test.local/"+mediaID, "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("legacy download нового медиа: %d %s", rec.Code, rec.Body.String())
	}
}
