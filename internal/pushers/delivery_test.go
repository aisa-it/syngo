package pushers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/pushers"
)

func newDispatcher(t *testing.T, opts pushers.DeliveryOptions) *pushers.Dispatcher {
	t.Helper()

	if opts.MinBackoff == 0 {
		opts.MinBackoff = 10 * time.Millisecond
		opts.MaxBackoff = 20 * time.Millisecond
	}

	d := pushers.NewDispatcher(opts)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = d.Close(ctx)
	})

	return d
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return true
		}

		time.Sleep(5 * time.Millisecond)
	}

	return cond()
}

// TestDispatcherSlowGatewayIsolated - зависший шлюз одного
// pusher не задерживает доставку на другой.
func TestDispatcherSlowGatewayIsolated(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	var fastHits atomic.Int32

	fast := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		fastHits.Add(1)
	}))
	t.Cleanup(fast.Close)

	d := newDispatcher(t, pushers.DeliveryOptions{})

	for i := range 3 {
		d.Enqueue(pushers.Notification{UserID: "@slow:t", AppID: "a", Pushkey: "k", URL: slow.URL, Body: []byte(`{}`)})

		if i == 0 {
			// Первое уведомление зависшего шлюза уже в работе.
			time.Sleep(20 * time.Millisecond)
		}
	}

	start := time.Now()

	if !d.Enqueue(pushers.Notification{UserID: "@fast:t", AppID: "a", Pushkey: "k", URL: fast.URL, Body: []byte(`{}`)}) {
		t.Fatal("Enqueue быстрого pusher отклонен")
	}

	if !waitUntil(t, time.Second, func() bool { return fastHits.Load() == 1 }) {
		t.Fatalf("push на быстрый шлюз не доставлен за %v", time.Since(start))
	}
}

// TestDispatcherRetry - 5xx и 429 повторяются с backoff, 4xx - нет.
func TestDispatcherRetry(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch hits.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(flaky.Close)

	d := newDispatcher(t, pushers.DeliveryOptions{})
	d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k", URL: flaky.URL, Body: []byte(`{}`)})

	if !waitUntil(t, 2*time.Second, func() bool { return d.Stats().Sent == 1 }) {
		t.Fatalf("после 503 и 429 уведомление не доставлено: %+v, попыток %d", d.Stats(), hits.Load())
	}

	if st := d.Stats(); st.Retried != 2 || hits.Load() != 3 {
		t.Fatalf("повторы: %+v, попыток %d", st, hits.Load())
	}

	var badHits atomic.Int32

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(bad.Close)

	d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k2", URL: bad.URL, Body: []byte(`{}`)})

	if !waitUntil(t, time.Second, func() bool { return d.Stats().Failed == 1 }) {
		t.Fatalf("400: %+v", d.Stats())
	}

	time.Sleep(50 * time.Millisecond)

	if badHits.Load() != 1 {
		t.Fatalf("400 повторялся: %d попыток", badHits.Load())
	}
}

// TestDispatcherRetryGivesUp - после MaxAttempts уведомление отбрасывается.
func TestDispatcherRetryGivesUp(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)

	d := newDispatcher(t, pushers.DeliveryOptions{MaxAttempts: 3})
	d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k", URL: down.URL, Body: []byte(`{}`)})

	if !waitUntil(t, 2*time.Second, func() bool { return d.Stats().Failed == 1 }) {
		t.Fatalf("stats: %+v", d.Stats())
	}

	if hits.Load() != 3 {
		t.Fatalf("попыток %d, ожидали 3", hits.Load())
	}
}

// TestDispatcherRejected - pushkey из rejected передается на
// удаление; чужие pushkeys из ответа шлюза игнорируются.
func TestDispatcherRejected(t *testing.T) {
	t.Parallel()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rejected":["mine","someone-else"]}`))
	}))
	t.Cleanup(gateway.Close)

	var (
		mu  sync.Mutex
		got []string
	)

	d := newDispatcher(t, pushers.DeliveryOptions{
		OnRejected: func(_ context.Context, userID, appID string, keys []string) {
			mu.Lock()
			defer mu.Unlock()

			got = append(got, userID+"|"+appID+"|"+strings.Join(keys, ","))
		},
	})
	d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "app", Pushkey: "mine", URL: gateway.URL, Body: []byte(`{}`)})

	ok := waitUntil(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(got) == 1
	})

	mu.Lock()
	defer mu.Unlock()

	if !ok || got[0] != "@u:t|app|mine" {
		t.Fatalf("rejected: %v", got)
	}
}

// TestDispatcherResponseLimit - ответ шлюза читается не больше MaxResponseBytes.
func TestDispatcherResponseLimit(t *testing.T) {
	t.Parallel()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", 4096) + `","rejected":["k"]}`))
	}))
	t.Cleanup(gateway.Close)

	var called atomic.Bool

	d := newDispatcher(t, pushers.DeliveryOptions{
		MaxResponseBytes: 1024,
		OnRejected:       func(context.Context, string, string, []string) { called.Store(true) },
	})
	d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k", URL: gateway.URL, Body: []byte(`{}`)})

	if !waitUntil(t, time.Second, func() bool { return d.Stats().Sent == 1 }) {
		t.Fatalf("stats: %+v", d.Stats())
	}

	if called.Load() {
		t.Fatal("ответ больше лимита прочитан целиком")
	}
}

// TestDispatcherQueueBounded - переполненная очередь pusher не блокирует Enqueue.
func TestDispatcherQueueBounded(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	d := newDispatcher(t, pushers.DeliveryOptions{QueueSize: 2, MaxActive: 1})

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 10 {
			d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k", URL: slow.URL, Body: []byte(`{}`)})
		}

		// Предел активных очередей: второй pusher не получает обработчика.
		d.Enqueue(pushers.Notification{UserID: "@v:t", AppID: "a", Pushkey: "k", URL: slow.URL, Body: []byte(`{}`)})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue заблокировался на переполненной очереди")
	}

	if st := d.Stats(); st.Dropped < 7 {
		t.Fatalf("dropped = %d, ожидали не меньше 7", st.Dropped)
	}
}

// TestDispatcherPending - зависший шлюз - уведомления копятся в
// очереди, Pending это показывает (метрика и алерт очереди push).
func TestDispatcherPending(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	d := newDispatcher(t, pushers.DeliveryOptions{QueueSize: 8, MaxActive: 3})

	if d.MaxActive() != 3 {
		t.Fatalf("MaxActive = %d", d.MaxActive())
	}

	for range 4 {
		d.Enqueue(pushers.Notification{UserID: "@u:t", AppID: "a", Pushkey: "k", URL: slow.URL, Body: []byte(`{}`)})
	}

	// Первое уведомление взято в доставку и висит на шлюзе, три ждут в очереди.
	if !waitUntil(t, 2*time.Second, func() bool { return d.Pending() == 3 }) {
		t.Fatalf("Pending = %d, ожидали 3", d.Pending())
	}
}

// TestRedactURL - в логах только схема и хост шлюза.
func TestRedactURL(t *testing.T) {
	t.Parallel()

	got := pushers.RedactURL("https://user:pw@push.example:8443/_matrix/push/v1/notify?token=secret")
	if got != "https://push.example:8443" {
		t.Fatalf("RedactURL = %q", got)
	}
}
