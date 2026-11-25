package media_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/media"
)

// memRepo — метаданные в памяти: тесты media-слоя не требуют PostgreSQL.
type memRepo struct {
	byURI map[string]media.Meta
}

func newMemRepo() *memRepo { return &memRepo{byURI: map[string]media.Meta{}} }

func (r *memRepo) Insert(_ context.Context, meta media.Meta) error {
	r.byURI[meta.ContentURI] = meta

	return nil
}

func (r *memRepo) ByID(_ context.Context, serverName, mediaID string) (media.Meta, error) {
	for i := range r.byURI {
		meta := r.byURI[i]
		if meta.ServerName == serverName && meta.MediaID == mediaID {
			return meta, nil
		}
	}

	return media.Meta{}, media.ErrNotFound
}

func (r *memRepo) TouchAccess(context.Context, string) error { return nil }

func (r *memRepo) InsertPending(_ context.Context, meta media.Meta) error {
	r.byURI[meta.ContentURI] = meta

	return nil
}

func (r *memRepo) UpdateUploaded(_ context.Context, meta media.Meta) error {
	if current, ok := r.byURI[meta.ContentURI]; ok && current.Uploaded {
		return media.ErrAlreadyUploaded
	}

	r.byURI[meta.ContentURI] = meta

	return nil
}

func (r *memRepo) PendingStats(_ context.Context, uploader string, since time.Time) (int, time.Time, error) {
	var (
		count  int
		oldest time.Time
	)

	for uri := range r.byURI {
		meta := r.byURI[uri]
		if meta.Uploader == uploader && !meta.Uploaded && !meta.CreatedAt.Before(since) {
			count++

			if oldest.IsZero() || meta.CreatedAt.Before(oldest) {
				oldest = meta.CreatedAt
			}
		}
	}

	return count, oldest, nil
}

func (r *memRepo) DeleteExpiredPending(_ context.Context, uploader string, before time.Time) error {
	for uri := range r.byURI {
		if meta := r.byURI[uri]; meta.Uploader == uploader && !meta.Uploaded && meta.CreatedAt.Before(before) {
			delete(r.byURI, uri)
		}
	}

	return nil
}

func newService(t *testing.T, maxBytes int64) (*media.Service, *memRepo) {
	t.Helper()

	svc, repo, _ := newServiceStore(t, maxBytes)

	return svc, repo
}

// newServiceStore - то же, но отдает и объектное хранилище (второй
// сервис над теми же байтами для проверки гонки PUT).
func newServiceStore(t *testing.T, maxBytes int64) (*media.Service, *memRepo, *media.LocalStore) {
	t.Helper()

	store, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"))
	if err != nil {
		t.Fatalf("local store: %v", err)
	}

	repo := newMemRepo()

	return media.NewService(repo, store, "test.local", maxBytes), repo, store
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)
	payload := []byte("hello media")

	contentURI, err := svc.Upload(context.Background(), "@alice:test.local", "text/plain", "greeting.txt", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if contentURI != "mxc://test.local/"+contentURI[len("mxc://test.local/"):] {
		t.Fatalf("content_uri = %q, ожидали mxc://test.local/<id>", contentURI)
	}

	meta, body, err := svc.Download(context.Background(), "test.local", metaMediaID(t, contentURI))
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("скачанные байты не совпали: %q != %q", got, payload)
	}

	if meta.ContentType != "text/plain" {
		t.Fatalf("content-type = %q", meta.ContentType)
	}

	if meta.SizeBytes != int64(len(payload)) {
		t.Fatalf("size = %d, ожидали %d", meta.SizeBytes, len(payload))
	}

	if len(meta.SHA256) != 32 {
		t.Fatalf("sha256 должен быть 32 байта, получили %d", len(meta.SHA256))
	}

	if meta.FileName != "greeting.txt" {
		t.Fatalf("file_name = %q", meta.FileName)
	}
}

func TestUploadTooLarge(t *testing.T) {
	t.Parallel()

	svc, repo := newService(t, 4)

	_, err := svc.Upload(context.Background(), "@alice:test.local", "text/plain", "big.bin",
		bytes.NewReader([]byte("more than four bytes")))
	if !errors.Is(err, media.ErrTooLarge) {
		t.Fatalf("ожидали ErrTooLarge, получили %v", err)
	}

	if len(repo.byURI) != 0 {
		t.Fatalf("при превышении лимита метаданные не должны сохраняться (%d)", len(repo.byURI))
	}
}

func TestUploadRequiresContentType(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)

	if _, err := svc.Upload(context.Background(), "@a:test.local", "  ", "", bytes.NewReader([]byte("x"))); !errors.Is(err, media.ErrUnsupportedType) {
		t.Fatalf("ожидали ErrUnsupportedType, получили %v", err)
	}
}

func TestDownloadUnknownMedia(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)

	if _, _, err := svc.Download(context.Background(), "test.local", "nope"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("ожидали ErrNotFound, получили %v", err)
	}

	// Чужой сервер: федерация вне скоупа — тоже ErrNotFound.
	if _, _, err := svc.Download(context.Background(), "other.host", "nope"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("чужой сервер: ожидали ErrNotFound, получили %v", err)
	}
}

func TestFileNameSanitized(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)

	uri, err := svc.Upload(context.Background(), "@a:test.local", "image/png", "../../etc/passwd", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	meta, body, err := svc.Download(context.Background(), "test.local", metaMediaID(t, uri))
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	_ = body.Close()

	if meta.FileName != "passwd" {
		t.Fatalf("file_name = %q, ожидали passwd (путь должен быть отброшен)", meta.FileName)
	}
}

func TestMediaIDsUnique(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)
	seen := map[string]bool{}

	for range 50 {
		uri, err := svc.Upload(context.Background(), "@a:test.local", "text/plain", "", bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatalf("upload: %v", err)
		}

		if seen[uri] {
			t.Fatalf("content_uri повторился: %s", uri)
		}

		seen[uri] = true
	}
}

// metaMediaID вырезает media_id из mxc://server/media_id.
func metaMediaID(t *testing.T, contentURI string) string {
	t.Helper()

	const prefix = "mxc://test.local/"

	if len(contentURI) <= len(prefix) || contentURI[:len(prefix)] != prefix {
		t.Fatalf("неожиданный content_uri: %q", contentURI)
	}

	return contentURI[len(prefix):]
}
