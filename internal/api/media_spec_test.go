package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
)

// Тесты медиа по спецификации CS API v1.11: async upload (MSC2246),
// аутентифицированные пути /_matrix/client/v1/media (MSC3916), лимиты.

// mediaDo выполняет запрос к медиа-репозиторию с произвольным телом и Content-Type.
func mediaDo(t *testing.T, server *Server, method, path, token, contentType string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	if body == nil {
		body = http.NoBody
	}

	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// createMedia вызывает POST /_matrix/media/v1/create и возвращает media_id.
func createMedia(t *testing.T, server *Server, token string) (mediaID string, unusedExpiresAt int64) {
	t.Helper()

	rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v1/create", token, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ContentURI      string `json:"content_uri"`
		UnusedExpiresAt int64  `json:"unused_expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create: %v (%s)", err, rec.Body.String())
	}

	const prefix = "mxc://test.local/"
	if !strings.HasPrefix(resp.ContentURI, prefix) {
		t.Fatalf("content_uri = %q", resp.ContentURI)
	}

	return strings.TrimPrefix(resp.ContentURI, prefix), resp.UnusedExpiresAt
}

// matrixErrCode достает errcode из тела ответа.
func matrixErrCode(rec *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"errcode"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	return body.Code
}

// TestMediaCreateAsyncUpload - POST /_matrix/media/v1/create выдает серверный
// media_id и unused_expires_at; PUT заполняет его ровно один раз и только автором.
func TestMediaCreateAsyncUpload(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")
	bob := registerUser(t, server, "bob", "1234567890")

	before := time.Now().UnixMilli()
	mediaID, expiresAt := createMedia(t, server, alice.AccessToken)

	if !regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`).MatchString(mediaID) {
		t.Fatalf("media_id должен генерироваться сервером, получили %q", mediaID)
	}

	// Synapse: unused_expiration_time по умолчанию 24 часа.
	if expiresAt < before+23*time.Hour.Milliseconds() || expiresAt > before+25*time.Hour.Milliseconds() {
		t.Fatalf("unused_expires_at = %d, ожидали около now+24h (now=%d)", expiresAt, before)
	}

	// Без токена create недоступен.
	if rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v1/create", "", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("create без токена: %d", rec.Code)
	}

	download := "/_matrix/client/v1/media/download/test.local/" + mediaID

	// До загрузки - 504 M_NOT_YET_UPLOADED (timeout_ms=0: не ждем).
	rec := mediaDo(t, server, http.MethodGet, download+"?timeout_ms=0", alice.AccessToken, "", nil)
	if rec.Code != http.StatusGatewayTimeout || matrixErrCode(rec) != "M_NOT_YET_UPLOADED" {
		t.Fatalf("download до загрузки: %d %s", rec.Code, rec.Body.String())
	}

	put := "/_matrix/media/v3/upload/test.local/" + mediaID

	// Чужой пользователь - 403 M_FORBIDDEN.
	rec = mediaDo(t, server, http.MethodPut, put, bob.AccessToken, "text/plain", strings.NewReader("evil"))
	if rec.Code != http.StatusForbidden || matrixErrCode(rec) != ErrCodeForbidden {
		t.Fatalf("PUT чужим пользователем: %d %s", rec.Code, rec.Body.String())
	}

	rec = mediaDo(t, server, http.MethodPut, put+"?filename=hello.txt", alice.AccessToken, "text/plain", strings.NewReader("payload"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}

	if body := strings.TrimSpace(rec.Body.String()); body != "{}" {
		t.Fatalf("PUT по спеке отвечает {}, получили %s", body)
	}

	// Повторная загрузка - 409 M_CANNOT_OVERWRITE_MEDIA.
	rec = mediaDo(t, server, http.MethodPut, put, alice.AccessToken, "text/plain", strings.NewReader("again"))
	if rec.Code != http.StatusConflict || matrixErrCode(rec) != "M_CANNOT_OVERWRITE_MEDIA" {
		t.Fatalf("повторный PUT: %d %s", rec.Code, rec.Body.String())
	}

	rec = mediaDo(t, server, http.MethodGet, download, bob.AccessToken, "", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "payload" {
		t.Fatalf("download после загрузки: %d %q", rec.Code, rec.Body.String())
	}

	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "hello.txt") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

// TestMediaClientChosenIDRejected - клиент не выбирает media_id: выдуманный
// POST /upload/{server}/{id} убран, PUT в несуществующий id - 404.
func TestMediaClientChosenIDRejected(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	if rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v3/upload/test.local/myid", alice.AccessToken, "", nil); rec.Code == http.StatusOK {
		t.Fatalf("POST с media_id клиента не должен создавать placeholder: %d %s", rec.Code, rec.Body.String())
	}

	rec := mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/myid", alice.AccessToken, "text/plain", strings.NewReader("x"))
	if rec.Code != http.StatusNotFound || matrixErrCode(rec) != ErrCodeNotFound {
		t.Fatalf("PUT в несозданный id: %d %s", rec.Code, rec.Body.String())
	}
}

// TestReviewMediaIDDot - media_id "." и подобные
// создавали файл на месте каталога сервера, после чего любая загрузка давала 500.
// Теперь такой id - 400, обычная загрузка после этого работает.
func TestReviewMediaIDDot(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	for _, id := range []string{".", "..", "%2e%2e", "a%2Fb", "a.b", "%20"} {
		rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v3/upload/test.local/"+id, alice.AccessToken, "", nil)
		if rec.Code == http.StatusOK {
			t.Errorf("id=%q: POST create с id клиента: %d", id, rec.Code)
		}

		rec = mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/"+id, alice.AccessToken, "text/plain", strings.NewReader("x"))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("id=%q: PUT ожидали 400/404, получили %d %s", id, rec.Code, rec.Body.String())
		}
	}

	rec := mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/.", alice.AccessToken, "text/plain", strings.NewReader("x"))
	if rec.Code != http.StatusBadRequest || matrixErrCode(rec) != ErrCodeInvalidParam {
		t.Fatalf("PUT id=\".\": ожидали 400 M_INVALID_PARAM, получили %d %s", rec.Code, rec.Body.String())
	}

	rec = upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "text/plain", strings.NewReader("normal"))
	if rec.Code != http.StatusOK {
		t.Fatalf("обычный upload после кривых id: %d %s", rec.Code, rec.Body.String())
	}
}

// TestReviewAsyncUploadTooLarge - PUT больше
// лимита давал 500 вместо 413, /_matrix/media/v1/create отсутствовал (404),
// preview_url отвечал выдуманным полем.
func TestReviewAsyncUploadTooLarge(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 4)
	alice := registerUser(t, server, "alice", "1234567890")

	mediaID, _ := createMedia(t, server, alice.AccessToken)
	put := "/_matrix/media/v3/upload/test.local/" + mediaID

	rec := mediaDo(t, server, http.MethodPut, put, alice.AccessToken, "text/plain", strings.NewReader("definitely longer than four bytes"))
	if rec.Code != http.StatusRequestEntityTooLarge || matrixErrCode(rec) != ErrCodeTooLarge {
		t.Fatalf("PUT сверх лимита: ожидали 413 M_TOO_LARGE, получили %d %s", rec.Code, rec.Body.String())
	}

	// Тело без Content-Length (chunked) тоже режется по факту прочитанного.
	req := httptest.NewRequest(http.MethodPut, put, io.MultiReader(strings.NewReader("12345"), strings.NewReader("6789")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+alice.AccessToken)
	req.Header.Set("Content-Type", "text/plain")
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)

	if out.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked PUT сверх лимита: %d %s", out.Code, out.Body.String())
	}

	// После отказа placeholder остается пустым и принимает файл в пределах лимита.
	if rec = mediaDo(t, server, http.MethodPut, put, alice.AccessToken, "text/plain", strings.NewReader("ok")); rec.Code != http.StatusOK {
		t.Fatalf("PUT в пределах лимита после 413: %d %s", rec.Code, rec.Body.String())
	}

	// preview_url: превью не генерируем, но отвечаем пустым OpenGraph-объектом.
	rec = mediaDo(t, server, http.MethodGet, "/_matrix/client/v1/media/preview_url?url=http://169.254.169.254/", alice.AccessToken, "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("preview_url: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMediaV1RequiresAuth - по v1.11 (MSC3916) все /_matrix/client/v1/media/*
// требуют access token; config и preview_url требуют его и на легаси-пути, как в Synapse.
func TestMediaV1RequiresAuth(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := upload(t, server, alice.AccessToken, "/_matrix/client/v1/media/upload", "image/png", bytes.NewReader(pngFixture(t, 4, 4)))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	for _, path := range []string{
		"/_matrix/client/v1/media/download/test.local/" + mediaID,
		"/_matrix/client/v1/media/download/test.local/" + mediaID + "/x.png",
		"/_matrix/client/v1/media/thumbnail/test.local/" + mediaID + "?width=32&height=32",
		"/_matrix/client/v1/media/config",
		"/_matrix/client/v1/media/preview_url?url=https://example.org/",
		"/_matrix/media/v3/config",
		"/_matrix/media/v3/preview_url?url=https://example.org/",
	} {
		rec := mediaDo(t, server, http.MethodGet, path, "", "", nil)
		if rec.Code != http.StatusUnauthorized || matrixErrCode(rec) != ErrCodeMissingToken {
			t.Errorf("%s без токена: ожидали 401 M_MISSING_TOKEN, получили %d %s", path, rec.Code, rec.Body.String())
		}

		if rec := mediaDo(t, server, http.MethodGet, path, alice.AccessToken, "", nil); rec.Code != http.StatusOK {
			t.Errorf("%s с токеном: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestMediaLegacyDownloadFrozen - как в Synapse (enable_authenticated_media):
// медиа, загруженные после включения аутентификации, не отдаются через
// неаутентифицированные /_matrix/media/v3/download|thumbnail.
func TestMediaLegacyDownloadFrozen(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "image/png", bytes.NewReader(pngFixture(t, 4, 4)))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	for _, path := range []string{
		"/_matrix/media/v3/download/test.local/" + mediaID,
		"/_matrix/media/v3/thumbnail/test.local/" + mediaID + "?width=32&height=32",
	} {
		// С токеном или без - легаси-путь токен не смотрит (как в Synapse).
		for _, token := range []string{"", alice.AccessToken} {
			rec := mediaDo(t, server, http.MethodGet, path, token, "", nil)
			if rec.Code != http.StatusNotFound || matrixErrCode(rec) != ErrCodeNotFound {
				t.Errorf("%s (token=%t): ожидали 404, получили %d %s", path, token != "", rec.Code, rec.Body.String())
			}
		}
	}
}

// TestMediaDownloadWaitsForUpload - download с timeout_ms ждет асинхронную
// загрузку (MSC2246) вместо немедленного 504.
func TestMediaDownloadWaitsForUpload(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	mediaID, _ := createMedia(t, server, alice.AccessToken)

	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v1/media/download/test.local/"+mediaID+"?timeout_ms=10000", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+alice.AccessToken)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		done <- rec
	}()

	time.Sleep(100 * time.Millisecond)

	if rec := mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/"+mediaID, alice.AccessToken,
		"text/plain", strings.NewReader("late")); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || rec.Body.String() != "late" {
			t.Fatalf("download с ожиданием: %d %q", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download не дождался загрузки")
	}
}

// TestMediaPendingLimitAndExpiry - число незаполненных placeholder ограничено
// (Synapse max_pending_media_uploads=5), просроченный placeholder - 404.
func TestMediaPendingLimitAndExpiry(t *testing.T) {
	t.Parallel()

	server, repo := newMediaServerRepo(t, 1<<20, config.RateLimit{})
	alice := registerUser(t, server, "alice", "1234567890")

	ids := make([]string, 0, 5)
	for range 5 {
		id, _ := createMedia(t, server, alice.AccessToken)
		ids = append(ids, id)
	}

	rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v1/create", alice.AccessToken, "", nil)
	if rec.Code != http.StatusTooManyRequests || matrixErrCode(rec) != ErrCodeLimitExceeded {
		t.Fatalf("6-й create: ожидали 429 M_LIMIT_EXCEEDED, получили %d %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "retry_after_ms") {
		t.Fatalf("429 без retry_after_ms: %s", rec.Body.String())
	}

	// Просроченный placeholder: PUT и download - 404, место в лимите освобождается.
	expired := ids[0]
	repo.setCreatedAt("mxc://test.local/"+expired, time.Now().Add(-25*time.Hour))

	rec = mediaDo(t, server, http.MethodGet, "/_matrix/client/v1/media/download/test.local/"+expired+"?timeout_ms=0", alice.AccessToken, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("download просроченного: %d %s", rec.Code, rec.Body.String())
	}

	rec = mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/"+expired, alice.AccessToken, "text/plain", strings.NewReader("x"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT в просроченный: %d %s", rec.Code, rec.Body.String())
	}

	createMedia(t, server, alice.AccessToken)
}

// TestMediaDownloadFileNameAndHeaders - имя из пути download переопределяет
// Content-Disposition; заголовки безопасности как в Synapse.
func TestMediaDownloadFileNameAndHeaders(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload?filename=orig.txt", "text/plain", strings.NewReader("hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	rec = mediaDo(t, server, http.MethodGet, "/_matrix/client/v1/media/download/test.local/"+mediaID+"/%D0%BE%D1%82%D1%87%D0%B5%D1%82.txt", alice.AccessToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}

	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") || !strings.Contains(cd, "filename*=utf-8''%D0%BE%D1%82%D1%87%D0%B5%D1%82.txt") {
		t.Fatalf("content-disposition = %q", cd)
	}

	for header, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "cross-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, ожидали %q", header, got, want)
		}
	}

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "media-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

// TestMediaUploadWithoutContentType - как в Synapse: без Content-Type файл
// принимается как application/octet-stream, а не 400.
func TestMediaUploadWithoutContentType(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := mediaDo(t, server, http.MethodPost, "/_matrix/media/v3/upload", alice.AccessToken, "", strings.NewReader("raw"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload без Content-Type: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMediaConfigOmitsUnlimited - при выключенном лимите m.upload.size не
// отдается (поле необязательное), а не 0: иначе клиент считает лимит нулевым.
func TestMediaConfigOmitsUnlimited(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 0)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := mediaDo(t, server, http.MethodGet, "/_matrix/client/v1/media/config", alice.AccessToken, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("config: %d %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "m.upload.size") {
		t.Fatalf("без лимита m.upload.size не отдаем: %s", rec.Body.String())
	}

	// И загрузка без лимита работает (MaxBytesReader с нулем резал все).
	mediaID, _ := createMedia(t, server, alice.AccessToken)
	if rec = mediaDo(t, server, http.MethodPut, "/_matrix/media/v3/upload/test.local/"+mediaID, alice.AccessToken,
		"text/plain", strings.NewReader("any size")); rec.Code != http.StatusOK {
		t.Fatalf("PUT без лимита: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMediaThumbnailV1 - миниатюры через аутентифицированный путь: scale и crop,
// PNG с прозрачностью остается PNG, не-картинка - 400 по спеке.
func TestMediaThumbnailV1(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "image/png", bytes.NewReader(pngFixture(t, 200, 100)))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	for _, tc := range []struct {
		method string
		w, h   int
	}{
		{"scale", 64, 64},
		{"crop", 32, 32},
	} {
		path := "/_matrix/client/v1/media/thumbnail/test.local/" + mediaID +
			"?method=" + tc.method + "&width=" + strconv.Itoa(tc.w) + "&height=" + strconv.Itoa(tc.h)

		rec := mediaDo(t, server, http.MethodGet, path, alice.AccessToken, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.method, rec.Code, rec.Body.String())
		}

		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Fatalf("%s: content-type = %q, ожидали image/png (прозрачность)", tc.method, ct)
		}

		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("%s: нет заголовков безопасности: %v", tc.method, rec.Header())
		}

		img, err := png.Decode(rec.Body)
		if err != nil {
			t.Fatalf("%s: decode: %v", tc.method, err)
		}

		b := img.Bounds()
		if b.Dx() > 200 || b.Dy() > 100 || b.Dx() == 0 {
			t.Fatalf("%s: размер %dx%d", tc.method, b.Dx(), b.Dy())
		}

		if tc.method == "crop" && b.Dx() != b.Dy() {
			t.Fatalf("crop должен дать квадрат, получили %dx%d", b.Dx(), b.Dy())
		}
	}

	rec = upload(t, server, alice.AccessToken, "/_matrix/media/v3/upload", "text/plain", strings.NewReader("not an image"))
	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	textID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

	rec = mediaDo(t, server, http.MethodGet, "/_matrix/client/v1/media/thumbnail/test.local/"+textID+"?width=32&height=32", alice.AccessToken, "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("thumbnail не-картинки: ожидали 400, получили %d %s", rec.Code, rec.Body.String())
	}
}

// pngFixture - PNG w x h с прозрачной половиной.
func pngFixture(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			if x < w/2 {
				img.Set(x, y, color.NRGBA{R: 200, G: 30, B: 30, A: 255})
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}

	return buf.Bytes()
}
