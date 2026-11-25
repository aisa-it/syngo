package media_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/media"
)

// Async upload (MSC2246) и доступ legacy-путей (MSC3916) на уровне сервиса.

// staleRepo отдает снимок метаданных, сделанный до загрузки: так выглядит
// гонка двух PUT, когда оба прочитали placeholder до записи первого.
type staleRepo struct {
	*memRepo
	snapshot media.Meta
}

func (r *staleRepo) ByID(context.Context, string, string) (media.Meta, error) {
	return r.snapshot, nil
}

func mediaIDOf(t *testing.T, contentURI string) string {
	t.Helper()

	return metaMediaID(t, contentURI)
}

// TestUploadToConcurrentPutsSingleWinner - из двух параллельных PUT побеждает
// один: проигравший получает ErrAlreadyUploaded, байты победителя не затерты.
func TestUploadToConcurrentPutsSingleWinner(t *testing.T) {
	t.Parallel()

	svc, repo, objects := newServiceStore(t, 1<<20)

	uri, _, err := svc.CreatePending(context.Background(), "@a:test.local")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	id := mediaIDOf(t, uri)
	snapshot := repo.byURI[uri]

	if _, err := svc.UploadTo(context.Background(), "@a:test.local", id, "text/plain", "", strings.NewReader("first"), 1<<20); err != nil {
		t.Fatalf("первый PUT: %v", err)
	}

	// Второй PUT читал placeholder до записи первого.
	stale := media.NewService(&staleRepo{memRepo: repo, snapshot: snapshot}, objects, "test.local", 1<<20)

	_, err = stale.UploadTo(context.Background(), "@a:test.local", id, "text/plain", "", strings.NewReader("second"), 1<<20)
	if !errors.Is(err, media.ErrAlreadyUploaded) {
		t.Fatalf("второй PUT: ожидали ErrAlreadyUploaded, получили %v", err)
	}

	_, body, err := svc.Download(context.Background(), "test.local", id)
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	defer func() { _ = body.Close() }()

	got, _ := io.ReadAll(body)
	if string(got) != "first" {
		t.Fatalf("байты победителя затерты: %q", got)
	}
}

// TestUploadToRejectsInvalidAndForeign - id вне алфавита - ErrInvalidMediaID,
// чужой placeholder - ErrForbidden.
func TestUploadToRejectsInvalidAndForeign(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)

	for _, id := range []string{".", "..", "a/b", "a.b", ""} {
		if _, err := svc.UploadTo(context.Background(), "@a:test.local", id, "text/plain", "", strings.NewReader("x"), 10); !errors.Is(err, media.ErrInvalidMediaID) {
			t.Errorf("id=%q: ожидали ErrInvalidMediaID, получили %v", id, err)
		}
	}

	uri, _, err := svc.CreatePending(context.Background(), "@a:test.local")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.UploadTo(context.Background(), "@b:test.local", mediaIDOf(t, uri), "text/plain", "", strings.NewReader("x"), 10); !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("чужой placeholder: ожидали ErrForbidden, получили %v", err)
	}
}

// TestCreatePendingLimit - предел незаполненных placeholder и retry по самому старому.
func TestCreatePendingLimit(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t, 1<<20)
	svc.Configure(media.Options{MaxPendingUploads: 2, UnusedExpiration: time.Hour, AuthenticatedMedia: true})

	for range 2 {
		if _, _, err := svc.CreatePending(context.Background(), "@a:test.local"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	_, _, err := svc.CreatePending(context.Background(), "@a:test.local")

	var limitErr *media.PendingLimitError
	if !errors.As(err, &limitErr) || !errors.Is(err, media.ErrPendingLimit) {
		t.Fatalf("ожидали PendingLimitError, получили %v", err)
	}

	if limitErr.RetryAfter <= 0 || limitErr.RetryAfter > time.Hour {
		t.Fatalf("RetryAfter = %v", limitErr.RetryAfter)
	}

	// Предел - на пользователя.
	if _, _, err := svc.CreatePending(context.Background(), "@b:test.local"); err != nil {
		t.Fatalf("другой пользователь: %v", err)
	}
}

// TestLegacyAccessFreeze - legacy-путь отдает медиа до freeze и не отдает после;
// при выключенном enable_authenticated_media отдает все.
func TestLegacyAccessFreeze(t *testing.T) {
	t.Parallel()

	svc, repo := newService(t, 1<<20)

	uri, err := svc.Upload(context.Background(), "@a:test.local", "image/png", "", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	id := mediaIDOf(t, uri)
	legacy := media.ReadOptions{Legacy: true}

	download := func() error {
		_, body, err := svc.DownloadWith(context.Background(), "test.local", id, legacy)
		if err == nil {
			_ = body.Close()
		}

		return err
	}

	freeze := time.Now().Add(-time.Hour)
	svc.Configure(media.Options{AuthenticatedMedia: true, LegacyFreeze: freeze})

	if err := download(); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("медиа после freeze через legacy: ожидали ErrNotFound, получили %v", err)
	}

	// Аутентифицированный путь отдает всегда.
	if _, body, err := svc.DownloadWith(context.Background(), "test.local", id, media.ReadOptions{}); err != nil {
		t.Fatalf("v1: %v", err)
	} else {
		_ = body.Close()
	}

	// Медиа до freeze - отдается.
	meta := repo.byURI[uri]
	meta.CreatedAt = freeze.Add(-time.Minute)
	repo.byURI[uri] = meta

	if err := download(); err != nil {
		t.Fatalf("медиа до freeze через legacy: %v", err)
	}

	// Без аутентифицированного медиа legacy отдает все.
	meta.CreatedAt = time.Now()
	repo.byURI[uri] = meta
	svc.Configure(media.Options{AuthenticatedMedia: false})

	if err := download(); err != nil {
		t.Fatalf("enable_authenticated_media=false: %v", err)
	}

	// Миниатюра идет через ту же проверку (до кеша).
	svc.Configure(media.Options{AuthenticatedMedia: true, LegacyFreeze: freeze})

	if _, err := svc.Thumbnail(context.Background(), "test.local", id, media.ThumbnailParams{Width: 32, Height: 32}, legacy); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("миниатюра после freeze через legacy: %v", err)
	}
}
