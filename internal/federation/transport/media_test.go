package transport_test

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// mediaClient - клиент федерации, направленный на httptest-сервер.
func mediaClient(t *testing.T, serverURL string) *transport.Client {
	t.Helper()

	key, err := signing.Generate("k1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	host := strings.TrimPrefix(serverURL, "http://")
	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: host, Scheme: "http"}}, nil
	}

	return transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", AddressPolicy: transport.AllowAllAddresses(),
	})
}

// writeMultipart - ответ федеративного media API: JSON и файл (или Location).
func writeMultipart(w http.ResponseWriter, file, location string) {
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())

	meta, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	_, _ = meta.Write([]byte(`{}`))

	header := textproto.MIMEHeader{"Content-Type": {"image/png"}, "Content-Disposition": {`inline; filename="cat.png"`}}
	if location != "" {
		header = textproto.MIMEHeader{"Location": {location}}
	}

	part, _ := mw.CreatePart(header)
	_, _ = part.Write([]byte(file))
	_ = mw.Close()
}

func readAll(t *testing.T, file transport.MediaFile) string {
	t.Helper()

	defer func() { _ = file.Body.Close() }()

	data, err := io.ReadAll(file.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(data)
}

// TestFetchMediaMultipart - федеративный media API: multipart с файлом и с
// Location; запрос подписан.
func TestFetchMediaMultipart(t *testing.T) {
	t.Parallel()

	var srvURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/federation/v1/media/download/abc":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "X-Matrix ") {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}

			writeMultipart(w, "PNGDATA", "")
		case "/_matrix/federation/v1/media/download/redir":
			writeMultipart(w, "", srvURL+"/cdn/redir")
		case "/cdn/redir":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("JPEGDATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	srvURL = srv.URL
	client := mediaClient(t, srv.URL)

	file, err := client.FetchMedia(context.Background(), "remote.test", "abc", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if file.ContentType != "image/png" || file.FileName != "cat.png" || readAll(t, file) != "PNGDATA" {
		t.Fatalf("файл: %+v", file)
	}

	file, err = client.FetchMedia(context.Background(), "remote.test", "redir", nil)
	if err != nil {
		t.Fatalf("fetch location: %v", err)
	}

	if file.ContentType != "image/jpeg" || readAll(t, file) != "JPEGDATA" {
		t.Fatalf("файл по Location: %+v", file)
	}
}

// TestFetchMediaLegacyFallback - сервер без федеративного media API (404
// M_UNRECOGNIZED) - фолбэк на /_matrix/media/v3/download с allow_remote=false;
// 404 M_NOT_FOUND - ErrMediaNotFound без фолбэка.
func TestFetchMediaLegacyFallback(t *testing.T) {
	t.Parallel()

	var legacyCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/federation/v1/media/download/old":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`))
		case "/_matrix/federation/v1/media/download/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Not found"}`))
		case "/_matrix/media/v3/download/remote.test/old":
			legacyCalls++

			if r.URL.Query().Get("allow_remote") != "false" {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename="a.txt"`)
			_, _ = w.Write([]byte("legacy"))
		default:
			legacyCalls++

			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := mediaClient(t, srv.URL)

	file, err := client.FetchMedia(context.Background(), "remote.test", "old", nil)
	if err != nil {
		t.Fatalf("fetch legacy: %v", err)
	}

	if file.FileName != "a.txt" || readAll(t, file) != "legacy" {
		t.Fatalf("legacy файл: %+v", file)
	}

	before := legacyCalls

	if _, err := client.FetchMedia(context.Background(), "remote.test", "gone", nil); !errors.Is(err, transport.ErrMediaNotFound) {
		t.Fatalf("M_NOT_FOUND: %v", err)
	}

	if legacyCalls != before {
		t.Fatalf("M_NOT_FOUND ушел в фолбэк")
	}
}

// TestFetchMediaErrors - миниатюра с параметрами, не-multipart ответ, плохой
// Location, 5xx и запрет политикой.
func TestFetchMediaErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_matrix/federation/v1/media/thumbnail/abc":
			if r.URL.Query().Get("width") != "32" {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			writeMultipart(w, "THUMB", "")
		case "/_matrix/federation/v1/media/download/plain":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("x"))
		case "/_matrix/federation/v1/media/download/badloc":
			writeMultipart(w, "", "ftp://example.test/x")
		case "/_matrix/federation/v1/media/download/legacyfail":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	client := mediaClient(t, srv.URL)
	ctx := context.Background()

	file, err := client.FetchMedia(ctx, "remote.test", "abc", map[string][]string{"width": {"32"}, "height": {"32"}})
	if err != nil || readAll(t, file) != "THUMB" {
		t.Fatalf("миниатюра: %v", err)
	}

	for _, id := range []string{"plain", "badloc", "legacyfail", "broken"} {
		if _, err := client.FetchMedia(ctx, "remote.test", id, nil); err == nil {
			t.Fatalf("%s: ожидали ошибку", id)
		}
	}

	denied := transport.NewClient(transport.Options{DenyHosts: []string{"remote.test"}})
	if _, err := denied.FetchMedia(ctx, "remote.test", "abc", nil); !errors.Is(err, transport.ErrDeniedByPolicy) {
		t.Fatalf("политика: %v", err)
	}
}

// TestFetchMediaAddressPolicy - скачивание медиа и ссылка Location проходят
// политику адресов: внутренний адрес по умолчанию запрещен.
func TestFetchMediaAddressPolicy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeMultipart(w, "x", "")
	}))
	defer srv.Close()

	key, err := signing.Generate("k1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: strings.TrimPrefix(srv.URL, "http://"), Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{Resolver: resolver, Key: &key, Origin: "ours.test"})

	if _, err := client.FetchMedia(context.Background(), "remote.test", "abc", nil); !errors.Is(err, transport.ErrBlockedAddress) {
		t.Fatalf("внутренний адрес: %v", err)
	}

	// Location на запрещенный адрес при разрешенном владельце: ссылка не
	// обходит политику (127.0.0.2 запрещен, 127.0.0.1 владельца - нет).
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeMultipart(w, "", "http://127.0.0.2:1/secret")
	}))
	defer owner.Close()

	policy, err := transport.NewAddressPolicy([]string{"127.0.0.2/32"}, nil)
	if err != nil {
		t.Fatalf("политика: %v", err)
	}

	ownerResolver := transport.NewResolver()
	ownerResolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: strings.TrimPrefix(owner.URL, "http://"), Scheme: "http"}}, nil
	}

	client = transport.NewClient(transport.Options{Resolver: ownerResolver, Key: &key, Origin: "ours.test", AddressPolicy: policy})

	if _, err := client.FetchMedia(context.Background(), "remote.test", "abc", nil); !errors.Is(err, transport.ErrBlockedAddress) {
		t.Fatalf("Location на запрещенный адрес: %v", err)
	}
}
