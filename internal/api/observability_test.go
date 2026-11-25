package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/store"
)

// TestMetricsExposition — /metrics отдаёт метрики /sync, лимитов, очереди и state resolution.
func TestMetricsExposition(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.syncDuration.observe(10 * time.Millisecond)
	srv.syncDuration.observe(200 * time.Millisecond)
	srv.syncCount.Add(2)
	srv.rateLimited.Add(3)
	srv.SetOutboxStats(func(context.Context) (store.OutboxStats, error) {
		return store.OutboxStats{Depth: 7, Oldest: 2 * time.Minute, Attempts: 11, DeadLetters: 4}, nil
	})

	rec := httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE syngo_sync_duration_seconds histogram",
		`syngo_sync_duration_seconds_bucket{le="0.01"} 1`,
		`syngo_sync_duration_seconds_bucket{le="0.25"} 2`,
		`syngo_sync_duration_seconds_bucket{le="+Inf"} 2`,
		"syngo_sync_duration_seconds_sum 0.210000",
		"syngo_sync_duration_seconds_count 2",
		"syngo_sync_requests_total 2",
		"syngo_rate_limited_total 3",
		"syngo_federation_outbox_depth 7",
		"syngo_federation_outbox_attempts_total 11",
		"syngo_federation_outbox_dead_letters 4",
		"syngo_state_resolution_duration_seconds_total",
		"syngo_federation_outbox_oldest_seconds 120.000",
		`syngo_push_notifications_total{result="sent"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("в /metrics нет %q\n%s", want, body)
		}
	}
}

// TestReadyOutbox — очередь федерации не влияет на готовность (один
// недоступный пир выводил из балансировки все инстансы), дренаж - влияет.
func TestReadyOutbox(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.db = nil

	readyChecks := func() (int, readyResponse) {
		rec := httptest.NewRecorder()
		srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", http.NoBody))

		var body readyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		return rec.Code, body
	}

	srv.SetOutboxStats(func(context.Context) (store.OutboxStats, error) {
		return store.OutboxStats{Depth: 100, Oldest: time.Hour}, nil
	})

	if _, body := readyChecks(); body.Checks["federation_outbox"] != "" {
		t.Fatalf("очередь федерации в readiness: %+v", body)
	}

	srv.SetOutboxStats(func(context.Context) (store.OutboxStats, error) {
		return store.OutboxStats{}, errors.New("очередь недоступна")
	})

	if _, body := readyChecks(); body.Checks["federation_outbox"] != "" {
		t.Fatalf("ошибка очереди в readiness: %+v", body)
	}

	srv.BeginDrain()

	if code, body := readyChecks(); code != http.StatusServiceUnavailable || body.Checks["draining"] == "" {
		t.Fatalf("при дренаже: %d %+v", code, body)
	}
}

// TestBeginDrainWakesSyncWaiters — дренаж будит long-poll /sync.
func TestBeginDrainWakesSyncWaiters(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	done := make(chan bool, 1)

	go func() { done <- srv.syncNotifier.Wait(context.Background(), 5*time.Second) }()

	deadline := time.Now().Add(time.Second)

	for srv.syncNotifier.Waiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	srv.BeginDrain()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("ожидающий не разбужен")
		}
	case <-time.After(time.Second):
		t.Fatal("long-poll не разбужен за секунду")
	}

	if !srv.Draining() {
		t.Fatal("Draining() = false")
	}
}

// TestReadyInstanceLock - без блокировки экземпляра (обрыв соединения
// с ней) инстанс выходит из балансировки, метрика показывает потерю.
func TestReadyInstanceLock(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.db = nil

	var held atomic.Bool

	held.Store(true)
	srv.SetInstanceLock(held.Load)

	ready := func() (int, readyResponse, string) {
		rec := httptest.NewRecorder()
		srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", http.NoBody))

		var body readyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		metrics := httptest.NewRecorder()
		srv.OpsHandler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

		return rec.Code, body, metrics.Body.String()
	}

	if _, body, metrics := ready(); body.Checks["instance_lock"] != "ok" || !strings.Contains(metrics, "syngo_instance_lock_held 1") {
		t.Fatalf("блокировка удерживается: %+v\n%s", body, metrics)
	}

	held.Store(false)

	code, body, metrics := ready()
	if code != http.StatusServiceUnavailable || body.Checks["instance_lock"] == "ok" || body.Checks["instance_lock"] == "" {
		t.Fatalf("блокировка потеряна: %d %+v", code, body)
	}

	if !strings.Contains(metrics, "syngo_instance_lock_held 0") {
		t.Fatalf("метрика при потере блокировки:\n%s", metrics)
	}
}

// TestHTTPMetrics - запросы считаются по шаблону маршрута и коду
// ответа, паника в обработчике - как 500, путь с идентификаторами в метки не
// попадает.
func TestHTTPMetrics(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	h := srv.Handler()

	for _, path := range []string{"/_matrix/client/versions", "/_matrix/client/versions", "/_matrix/client/v3/nope/@secret:host", "/nope/@secret:host"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, http.NoBody))
	}

	panics := srv.instrument(srv.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("сбой")
	})))
	panics.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/x", http.NoBody))

	rec := httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE syngo_http_requests_total counter",
		`syngo_http_requests_total{method="GET",route="/_matrix/client/versions",code="200"} 2`,
		`syngo_http_requests_total{method="GET",route="unmatched",code="404"} 1`,
		`syngo_http_requests_total{method="GET",route="/_matrix/client/v3/*",code="404"} 1`,
		`syngo_http_requests_total{method="other",route="unmatched",code="500"} 1`,
		"# TYPE syngo_http_request_duration_seconds histogram",
		`syngo_http_request_duration_seconds_count{method="GET",route="/_matrix/client/versions"} 2`,
		`syngo_http_request_duration_seconds_bucket{method="GET",route="/_matrix/client/versions",le="+Inf"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("в /metrics нет %q\n%s", want, body)
		}
	}

	if strings.Contains(body, "@secret:host") {
		t.Fatal("путь запроса попал в метки")
	}
}

// TestProcessMetrics - рантайм, сборка, готовность и очередь push.
func TestProcessMetrics(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.cfg.Media.Directory = t.TempDir()

	rec := httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	body := rec.Body.String()

	for _, want := range []string{
		"syngo_build_info{version=",
		"process_start_time_seconds ",
		"go_goroutines ",
		"go_memstats_heap_alloc_bytes ",
		"go_gc_cycles_total ",
		// Без БД инстанс не готов: алерт SyngoNotReady.
		"syngo_ready 0",
		`syngo_ready_check{check="database"} 0`,
		"syngo_push_queued_notifications 0",
		"syngo_push_max_active_queues ",
		"syngo_media_disk_free_bytes ",
		"syngo_media_disk_size_bytes ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("в /metrics нет %q\n%s", want, body)
		}
	}
}

// TestSyncDurationExcludesLongPoll - ожидание long-poll не входит в
// syngo_sync_duration_seconds, иначе p99 равен таймауту клиента.
func TestSyncDurationExcludesLongPoll(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	first := decodeSync(t, rec.Body.Bytes())

	countBefore, sumBefore := server.syncDuration.snapshot()
	started := time.Now()

	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?timeout=400&since="+url.QueryEscape(first.NextBatch), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("long-poll: %d %s", rec.Code, rec.Body.String())
	}

	if elapsed := time.Since(started); elapsed < 350*time.Millisecond {
		t.Fatalf("long-poll не ждал: %v", elapsed)
	}

	count, sum := server.syncDuration.snapshot()
	if count != countBefore+1 {
		t.Fatalf("count = %d, ожидали %d", count, countBefore+1)
	}

	if processed := sum - sumBefore; processed > 0.2 {
		t.Fatalf("в длительность /sync вошло ожидание long-poll: %.3f с", processed)
	}
}
