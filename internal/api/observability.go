package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/aisa-it/syngo/internal/build"
	"github.com/aisa-it/syngo/internal/pushers"
	"github.com/aisa-it/syngo/internal/state"
	"github.com/aisa-it/syngo/internal/store"
)

// SetOutboxStats подключает источник статистики федеративной очереди (main).
func (s *Server) SetOutboxStats(fn func(context.Context) (store.OutboxStats, error)) {
	s.outboxStats = fn
}

// BeginDrain переводит сервер в режим завершения: long-poll /sync больше не ждёт,
// а сразу отдаёт текущую позицию, чтобы SIGTERM не рвал соединения.
func (s *Server) BeginDrain() {
	s.draining.Store(true)

	if s.syncNotifier != nil {
		s.syncNotifier.Notify()
	}
}

// Draining сообщает, что сервер завершает работу.
func (s *Server) Draining() bool { return s.draining.Load() }

// writeMetrics пишет метрики процесса в текстовом формате Prometheus.
func (s *Server) writeMetrics(ctx context.Context, w io.Writer) {
	state.DefaultMetrics.WritePrometheus(w)

	s.writeProcessMetrics(w)
	s.writeReadyMetric(ctx, w)

	s.httpRequests.write(w)
	s.httpDuration.write(w)
	s.syncDuration.write(w)

	fmt.Fprintf(w, "# HELP syngo_sync_requests_total Число запросов /sync.\n")
	fmt.Fprintf(w, "# TYPE syngo_sync_requests_total counter\n")
	fmt.Fprintf(w, "syngo_sync_requests_total %d\n", s.syncCount.Load())
	fmt.Fprintf(w, "# HELP syngo_sync_longpoll_waiters Число ожидающих long-poll /sync.\n")
	fmt.Fprintf(w, "# TYPE syngo_sync_longpoll_waiters gauge\n")

	waiters := 0
	if s.syncNotifier != nil {
		waiters = s.syncNotifier.Waiters()
	}

	fmt.Fprintf(w, "syngo_sync_longpoll_waiters %d\n", waiters)

	fmt.Fprintf(w, "# HELP syngo_rate_limited_total Ответы 429 по эндпоинтам.\n")
	fmt.Fprintf(w, "# TYPE syngo_rate_limited_total counter\n")
	fmt.Fprintf(w, "syngo_rate_limited_total %d\n", s.rateLimited.Load())

	s.writePushMetrics(w)

	if s.instanceLockHeld != nil {
		held := 0
		if s.instanceLockHeld() {
			held = 1
		}

		fmt.Fprintf(w, "# HELP syngo_instance_lock_held Блокировка экземпляра на базе удерживается.\n")
		fmt.Fprintf(w, "# TYPE syngo_instance_lock_held gauge\n")
		fmt.Fprintf(w, "syngo_instance_lock_held %d\n", held)
	}

	s.writeStorageMetrics(ctx, w)
	s.writeOutboxMetrics(ctx, w)
}

// writeOutboxMetrics - очередь федерации; -1 - статистика недоступна.
func (s *Server) writeOutboxMetrics(ctx context.Context, w io.Writer) {
	if s.outboxStats == nil {
		return
	}

	statsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)

	stats, err := s.outboxStats(statsCtx)
	cancel()

	fmt.Fprintf(w, "# HELP syngo_federation_outbox_depth Элементы исходящей очереди федерации.\n")
	fmt.Fprintf(w, "# TYPE syngo_federation_outbox_depth gauge\n")

	if err != nil {
		fmt.Fprintf(w, "syngo_federation_outbox_depth -1\n")

		return
	}

	fmt.Fprintf(w, "syngo_federation_outbox_depth %d\n", stats.Depth)
	fmt.Fprintf(w, "# HELP syngo_federation_outbox_oldest_seconds Возраст старейшего элемента очереди.\n")
	fmt.Fprintf(w, "# TYPE syngo_federation_outbox_oldest_seconds gauge\n")
	fmt.Fprintf(w, "syngo_federation_outbox_oldest_seconds %.3f\n", stats.Oldest.Seconds())
	fmt.Fprintf(w, "# HELP syngo_federation_outbox_attempts_total Попытки доставки в федерацию.\n")
	fmt.Fprintf(w, "# TYPE syngo_federation_outbox_attempts_total counter\n")
	fmt.Fprintf(w, "syngo_federation_outbox_attempts_total %d\n", stats.Attempts)
	// Недоставляемые элементы (предел попыток, отказ получателя).
	fmt.Fprintf(w, "# HELP syngo_federation_outbox_dead_letters Элементы outbox в dead-letter.\n")
	fmt.Fprintf(w, "# TYPE syngo_federation_outbox_dead_letters gauge\n")
	fmt.Fprintf(w, "syngo_federation_outbox_dead_letters %d\n", stats.DeadLetters)
}

// writeReadyMetric - готовность инстанса числом: алерт на
// недоступность не зависит от того, опрашивает ли балансировщик пробу.
func (s *Server) writeReadyMetric(ctx context.Context, w io.Writer) {
	status, resp := s.readiness(ctx, false)

	ready := 0
	if status == http.StatusOK {
		ready = 1
	}

	fmt.Fprintf(w, "# HELP syngo_ready Инстанс готов принимать запросы (как /health/ready).\n")
	fmt.Fprintf(w, "# TYPE syngo_ready gauge\n")
	fmt.Fprintf(w, "syngo_ready %d\n", ready)
	fmt.Fprintf(w, "# HELP syngo_ready_check Проверки готовности: 1 - ok, 0 - отказ.\n")
	fmt.Fprintf(w, "# TYPE syngo_ready_check gauge\n")

	for _, check := range sortedKeys(resp.Checks) {
		ok := 0
		if resp.Checks[check] == "ok" {
			ok = 1
		}

		fmt.Fprintf(w, "syngo_ready_check{check=\"%s\"} %d\n", labelEscaper.Replace(check), ok)
	}
}

// writeProcessMetrics - сборка, рантайм Go и время старта процесса.
func (s *Server) writeProcessMetrics(w io.Writer) {
	var mem runtime.MemStats

	runtime.ReadMemStats(&mem)

	fmt.Fprintf(w, "# HELP syngo_build_info Версия сборки.\n")
	fmt.Fprintf(w, "# TYPE syngo_build_info gauge\n")
	fmt.Fprintf(w, "syngo_build_info{version=\"%s\",synapse_compat=\"%s\",commit=\"%s\",goversion=\"%s\"} 1\n",
		labelEscaper.Replace(build.Version), build.SynapseCompat, labelEscaper.Replace(build.Commit), runtime.Version())
	fmt.Fprintf(w, "# HELP process_start_time_seconds Время старта процесса (unix).\n")
	fmt.Fprintf(w, "# TYPE process_start_time_seconds gauge\n")
	fmt.Fprintf(w, "process_start_time_seconds %d\n", s.startedAt.Unix())
	fmt.Fprintf(w, "# HELP go_goroutines Число горутин.\n")
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\n")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# HELP go_memstats_heap_alloc_bytes Занятая куча.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_heap_alloc_bytes %d\n", mem.HeapAlloc)
	fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Память, полученная рантаймом от ОС.\n")
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\n")
	fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", mem.Sys)
	fmt.Fprintf(w, "# HELP go_gc_cycles_total Завершенные циклы GC.\n")
	fmt.Fprintf(w, "# TYPE go_gc_cycles_total counter\n")
	fmt.Fprintf(w, "go_gc_cycles_total %d\n", mem.NumGC)
	fmt.Fprintf(w, "# HELP go_gc_pause_seconds_total Суммарные паузы GC.\n")
	fmt.Fprintf(w, "# TYPE go_gc_pause_seconds_total counter\n")
	fmt.Fprintf(w, "go_gc_pause_seconds_total %.6f\n", time.Duration(mem.PauseTotalNs).Seconds()) //nolint:gosec // наносекунды пауз не переполняют int64
}

// writeStorageMetrics - пул соединений PostgreSQL, размер базы (через
// core.HealthService) и место на томе медиа.
func (s *Server) writeStorageMetrics(ctx context.Context, w io.Writer) {
	if st, ok := s.health.PoolStats(); ok {
		fmt.Fprintf(w, "# HELP syngo_db_pool_connections Соединения пула PostgreSQL по состоянию.\n")
		fmt.Fprintf(w, "# TYPE syngo_db_pool_connections gauge\n")
		fmt.Fprintf(w, "syngo_db_pool_connections{state=\"acquired\"} %d\n", st.Acquired)
		fmt.Fprintf(w, "syngo_db_pool_connections{state=\"idle\"} %d\n", st.Idle)
		fmt.Fprintf(w, "syngo_db_pool_connections{state=\"constructing\"} %d\n", st.Constructing)
		fmt.Fprintf(w, "# HELP syngo_db_pool_max_connections Предел пула (database.max_conns).\n")
		fmt.Fprintf(w, "# TYPE syngo_db_pool_max_connections gauge\n")
		fmt.Fprintf(w, "syngo_db_pool_max_connections %d\n", st.Max)
		fmt.Fprintf(w, "# HELP syngo_db_pool_acquires_total Выдачи соединений из пула.\n")
		fmt.Fprintf(w, "# TYPE syngo_db_pool_acquires_total counter\n")
		fmt.Fprintf(w, "syngo_db_pool_acquires_total %d\n", st.Acquires)
		fmt.Fprintf(w, "# HELP syngo_db_pool_empty_acquires_total Выдачи с ожиданием свободного соединения.\n")
		fmt.Fprintf(w, "# TYPE syngo_db_pool_empty_acquires_total counter\n")
		fmt.Fprintf(w, "syngo_db_pool_empty_acquires_total %d\n", st.EmptyAcquires)
		fmt.Fprintf(w, "# HELP syngo_db_pool_acquire_wait_seconds_total Суммарное ожидание соединения.\n")
		fmt.Fprintf(w, "# TYPE syngo_db_pool_acquire_wait_seconds_total counter\n")
		fmt.Fprintf(w, "syngo_db_pool_acquire_wait_seconds_total %.6f\n", st.EmptyAcquireWait.Seconds())

		sizeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		size, err := s.health.DatabaseSize(sizeCtx)

		cancel()

		if err == nil {
			fmt.Fprintf(w, "# HELP syngo_db_size_bytes Размер базы (pg_database_size).\n")
			fmt.Fprintf(w, "# TYPE syngo_db_size_bytes gauge\n")
			fmt.Fprintf(w, "syngo_db_size_bytes %d\n", size)
		}
	}

	if s.cfg.Media.StorageBackend != "local" || s.cfg.Media.Directory == "" {
		return
	}

	free, total, err := diskSpace(s.cfg.Media.Directory)
	if err != nil {
		return
	}

	fmt.Fprintf(w, "# HELP syngo_media_disk_free_bytes Свободно на томе медиа (для непривилегированного процесса).\n")
	fmt.Fprintf(w, "# TYPE syngo_media_disk_free_bytes gauge\n")
	fmt.Fprintf(w, "syngo_media_disk_free_bytes %d\n", free)
	fmt.Fprintf(w, "# HELP syngo_media_disk_size_bytes Размер тома медиа.\n")
	fmt.Fprintf(w, "# TYPE syngo_media_disk_size_bytes gauge\n")
	fmt.Fprintf(w, "syngo_media_disk_size_bytes %d\n", total)
}

// instrument считает запросы и длительность по шаблону маршрута и коду ответа.
// Метод вне списка - "other": метки не растут от мусорных запросов.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			method := r.Method

			switch method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete,
				http.MethodOptions, http.MethodHead, http.MethodPatch:
			default:
				method = "other"
			}

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}

			route := routePattern(r)
			s.httpRequests.inc(method, route, strconv.Itoa(status))
			s.httpDuration.observe(time.Since(start), method, route)
		}()

		next.ServeHTTP(ww, r)
	})
}

// writePushMetrics - счетчики доставки push.
func (s *Server) writePushMetrics(w io.Writer) {
	var (
		stats                      pushers.DeliveryStats
		active, pending, maxActive int
	)

	if d := s.pushQueue.Load(); d != nil {
		stats, active, pending, maxActive = d.Stats(), d.Active(), d.Pending(), d.MaxActive()
	}

	fmt.Fprintf(w, "# HELP syngo_push_notifications_total Уведомления push по исходу доставки.\n")
	fmt.Fprintf(w, "# TYPE syngo_push_notifications_total counter\n")
	fmt.Fprintf(w, "syngo_push_notifications_total{result=\"sent\"} %d\n", stats.Sent)
	fmt.Fprintf(w, "syngo_push_notifications_total{result=\"failed\"} %d\n", stats.Failed)
	fmt.Fprintf(w, "syngo_push_notifications_total{result=\"dropped\"} %d\n", stats.Dropped)
	fmt.Fprintf(w, "# HELP syngo_push_retries_total Повторные попытки доставки push.\n")
	fmt.Fprintf(w, "# TYPE syngo_push_retries_total counter\n")
	fmt.Fprintf(w, "syngo_push_retries_total %d\n", stats.Retried)
	fmt.Fprintf(w, "# HELP syngo_push_rejected_total Pushkeys, отвергнутые шлюзом (pusher удален).\n")
	fmt.Fprintf(w, "# TYPE syngo_push_rejected_total counter\n")
	fmt.Fprintf(w, "syngo_push_rejected_total %d\n", stats.Rejected)
	fmt.Fprintf(w, "# HELP syngo_push_active_queues Очереди pushers с недоставленными уведомлениями.\n")
	fmt.Fprintf(w, "# TYPE syngo_push_active_queues gauge\n")
	fmt.Fprintf(w, "syngo_push_active_queues %d\n", active)
	fmt.Fprintf(w, "# HELP syngo_push_max_active_queues Предел активных очередей pushers.\n")
	fmt.Fprintf(w, "# TYPE syngo_push_max_active_queues gauge\n")
	fmt.Fprintf(w, "syngo_push_max_active_queues %d\n", maxActive)
	fmt.Fprintf(w, "# HELP syngo_push_queued_notifications Уведомления в очередях, ожидающие доставки.\n")
	fmt.Fprintf(w, "# TYPE syngo_push_queued_notifications gauge\n")
	fmt.Fprintf(w, "syngo_push_queued_notifications %d\n", pending)
}
