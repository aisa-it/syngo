package media_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/media"
)

// fakeFetcher - сервер-владелец медиа.
type fakeFetcher struct {
	calls atomic.Int32
	files map[string]string
	gate  chan struct{}
	err   error
}

func (f *fakeFetcher) FetchRemote(_ context.Context, serverName, mediaID string) (media.RemoteFile, error) {
	f.calls.Add(1)

	if f.gate != nil {
		<-f.gate
	}

	if f.err != nil {
		return media.RemoteFile{}, f.err
	}

	data, ok := f.files[serverName+"/"+mediaID]
	if !ok {
		return media.RemoteFile{}, media.ErrNotFound
	}

	return media.RemoteFile{ContentType: "text/plain", FileName: "r.txt", Body: io.NopCloser(strings.NewReader(data))}, nil
}

func (f *fakeFetcher) FetchRemoteThumbnail(context.Context, string, string, media.ThumbnailParams) (media.RemoteFile, error) {
	return media.RemoteFile{}, media.ErrNotFound
}

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()

	defer func() { _ = body.Close() }()

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(data)
}

// TestRemoteMediaFetchedAndCached - медиа другого сервера скачивается у
// владельца один раз, дальше отдается из кеша. Раньше - 404.
func TestRemoteMediaFetchedAndCached(t *testing.T) {
	t.Parallel()

	svc, repo := newService(t, 1<<20)
	svc.Configure(media.Options{AuthenticatedMedia: false})

	fetcher := &fakeFetcher{files: map[string]string{"remote.test/abc": "remote bytes"}}
	svc.SetRemoteFetcher(fetcher)

	for range 2 {
		meta, body, err := svc.Download(context.Background(), "remote.test", "abc")
		if err != nil {
			t.Fatalf("download: %v", err)
		}

		if got := readBody(t, body); got != "remote bytes" || meta.ContentType != "text/plain" || meta.FileName != "r.txt" {
			t.Fatalf("медиа: %q %+v", got, meta)
		}
	}

	if n := fetcher.calls.Load(); n != 1 {
		t.Fatalf("скачиваний %d, ожидали 1", n)
	}

	if meta := repo.byURI["mxc://remote.test/abc"]; !meta.IsRemote {
		t.Fatalf("кеш без is_remote: %+v", meta)
	}

	if _, _, err := svc.Download(context.Background(), "remote.test", "missing"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("нет у владельца: %v", err)
	}

	// Недопустимые имена не уходят на сервер.
	if _, _, err := svc.Download(context.Background(), "../etc", "abc"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("server_name с путем: %v", err)
	}
}

// TestRemoteMediaLimitAndErrors - больше лимита - ErrTooLarge без кеша;
// сбой владельца - ErrRemoteUnavailable; legacy-путь при authenticated media
// не скачивает новое.
func TestRemoteMediaLimitAndErrors(t *testing.T) {
	t.Parallel()

	svc, repo := newService(t, 4)

	fetcher := &fakeFetcher{files: map[string]string{"remote.test/big": "12345"}}
	svc.SetRemoteFetcher(fetcher)

	if _, _, err := svc.Download(context.Background(), "remote.test", "big"); !errors.Is(err, media.ErrTooLarge) {
		t.Fatalf("больше лимита: %v", err)
	}

	if len(repo.byURI) != 0 {
		t.Fatalf("закешировано сверх лимита: %+v", repo.byURI)
	}

	if _, _, err := svc.DownloadWith(context.Background(), "remote.test", "big",
		media.ReadOptions{Legacy: true}); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("legacy: %v", err)
	}

	calls := fetcher.calls.Load()
	if calls != 1 {
		t.Fatalf("legacy-путь скачивал (%d)", calls)
	}

	fetcher.err = errors.New("boom")

	if _, _, err := svc.Download(context.Background(), "remote.test", "x"); !errors.Is(err, media.ErrRemoteUnavailable) {
		t.Fatalf("сбой владельца: %v", err)
	}
}

// TestRemoteMediaSingleFlight - параллельные запросы одного медиа - одно скачивание.
func TestRemoteMediaSingleFlight(t *testing.T) {
	t.Parallel()

	objects, err := media.NewLocalStore(filepath.Join(t.TempDir(), "media"))
	if err != nil {
		t.Fatalf("local store: %v", err)
	}

	svc := media.NewService(&lockedRepo{memRepo: newMemRepo()}, objects, "test.local", 1<<20)

	fetcher := &fakeFetcher{files: map[string]string{"remote.test/abc": "x"}, gate: make(chan struct{})}
	svc.SetRemoteFetcher(fetcher)

	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, body, err := svc.Download(context.Background(), "remote.test", "abc"); err == nil {
				_ = body.Close()
			}
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for fetcher.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Остальные запросы успевают встать в ожидание.
	time.Sleep(20 * time.Millisecond)

	close(fetcher.gate)
	wg.Wait()

	if n := fetcher.calls.Load(); n != 1 {
		t.Fatalf("скачиваний %d, ожидали 1", n)
	}
}

// lockedRepo - memRepo для параллельных запросов.
type lockedRepo struct {
	*memRepo
	mu sync.Mutex
}

func (r *lockedRepo) Insert(ctx context.Context, meta media.Meta) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memRepo.Insert(ctx, meta)
}

func (r *lockedRepo) ByID(ctx context.Context, serverName, mediaID string) (media.Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memRepo.ByID(ctx, serverName, mediaID)
}
