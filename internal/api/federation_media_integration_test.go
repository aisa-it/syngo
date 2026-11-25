//go:build integration

package api

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRemoteMediaDownloadViaFederation - клиент скачивает mxc:// другого
// сервера через /_matrix/client/v1/media/download: медиа берется у владельца
// по федеративному media API и кешируется. Раньше - всегда 404.
func TestRemoteMediaDownloadViaFederation(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICEDEV")

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/federation/v1/media/download/remotecat" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))

			return
		}

		calls.Add(1)

		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())

		meta, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
		_, _ = meta.Write([]byte(`{}`))

		part, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":        {"text/plain"},
			"Content-Disposition": {`attachment; filename="cat.txt"`},
		})
		_, _ = part.Write([]byte("encrypted-attachment"))
		_ = mw.Close()
	}))
	t.Cleanup(srv.Close)
	env.setTransport(srv.URL)

	for range 2 {
		rec := do(t, env.server, http.MethodGet, "/_matrix/client/v1/media/download/remote.test/remotecat", "", alice)
		if rec.Code != http.StatusOK || rec.Body.String() != "encrypted-attachment" {
			t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
		}
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("скачиваний у владельца %d, ожидали 1 (кеш)", n)
	}

	rec := do(t, env.server, http.MethodGet, "/_matrix/client/v1/media/download/remote.test/missing", "", alice)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("нет у владельца: %d", rec.Code)
	}

	// Владелец недоступен - 502.
	srv.Close()

	rec = do(t, env.server, http.MethodGet, "/_matrix/client/v1/media/download/remote.test/other", "", alice)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("владелец недоступен: %d %s", rec.Code, rec.Body.String())
	}
}

// TestFederationMediaDownloadServesLocal - другой сервер скачивает наше медиа
// по GET /_matrix/federation/v1/media/download/{mediaId} (multipart/mixed).
func TestFederationMediaDownloadServesLocal(t *testing.T) {
	env := newFedEnv(t)
	alice := registerHTTP(t, env.server, "alice", "ALICEDEV")

	rec := do(t, env.server, http.MethodPost, "/_matrix/media/v3/upload?filename=doc.txt", "file-bytes", alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &uploaded)
	mediaID := uploaded.ContentURI[strings.LastIndex(uploaded.ContentURI, "/")+1:]

	rec = env.request(t, http.MethodGet, "/_matrix/federation/v1/media/download/"+mediaID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("federation download: %d %s", rec.Code, rec.Body.String())
	}

	mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("content-type: %q", rec.Header().Get("Content-Type"))
	}

	reader := multipart.NewReader(rec.Body, params["boundary"])

	meta, err := reader.NextPart()
	if err != nil || meta.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("metadata part: %v", err)
	}

	file, err := reader.NextPart()
	if err != nil {
		t.Fatalf("file part: %v", err)
	}

	data, _ := io.ReadAll(file)
	if string(data) != "file-bytes" {
		t.Fatalf("файл: %q", data)
	}

	if rec := env.request(t, http.MethodGet, "/_matrix/federation/v1/media/download/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("нет медиа: %d", rec.Code)
	}
}
