package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/benchenv"
)

// sample - точка временного ряда ресурсов за прогон.
type sample struct {
	T          float64 `json:"t"`
	RSSKB      int64   `json:"rss_kb,omitempty"`
	CPUPercent float64 `json:"cpu_pct,omitempty"`
	Threads    int64   `json:"threads,omitempty"`
	Waiters    float64 `json:"longpoll_waiters,omitempty"`
	Goroutines float64 `json:"goroutines,omitempty"`
	PGConns    int64   `json:"pg_conns,omitempty"`
	PGActive   int64   `json:"pg_active,omitempty"`
}

// collector - метрики сервера (Prometheus /metrics), процесса (/proc/<pid>) и
// PostgreSQL за окно замера. Любой источник необязателен: недоступный пишется
// в отчет причиной, прогон не падает.
type collector struct {
	metricsURL string
	pid        int
	pg         *pgx.Conn
	http       *http.Client

	mu       sync.Mutex
	samples  []sample
	promA    promSet
	promB    promSet
	promErr  string
	procA    *benchenv.ProcessSample
	procB    *benchenv.ProcessSample
	procErr  string
	pgA, pgB *pgSnapshot
	pgErr    string
	started  time.Time
	finished time.Time
}

func newCollector(metricsURL string, pid int, pg *pgx.Conn) *collector {
	return &collector{metricsURL: metricsURL, pid: pid, pg: pg, http: &http.Client{Timeout: 10 * time.Second}}
}

// scrape снимает /metrics.
func (c *collector) scrape(ctx context.Context) (promSet, error) {
	if c.metricsURL == "" {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metricsURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics: HTTP %d", resp.StatusCode)
	}

	return parsePrometheus(resp.Body)
}

// snapshot - снимок всех источников (начало или конец окна).
func (c *collector) snapshot(ctx context.Context, end bool) {
	prom, promErr := c.scrape(ctx)

	var (
		proc    *benchenv.ProcessSample
		procErr error
	)

	if c.pid > 0 {
		var p benchenv.ProcessSample

		p, procErr = benchenv.ReadProcess(c.pid)
		if procErr == nil {
			proc = &p
		}
	}

	var (
		pg    *pgSnapshot
		pgErr error
	)

	if c.pg != nil {
		pg, pgErr = takePGSnapshot(ctx, c.pg)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if end {
		c.promB, c.procB, c.pgB, c.finished = prom, proc, pg, time.Now()
	} else {
		c.promA, c.procA, c.pgA, c.started = prom, proc, pg, time.Now()
	}

	if promErr != nil {
		c.promErr = promErr.Error()
	}

	if procErr != nil {
		c.procErr = procErr.Error()
	}

	if pgErr != nil {
		c.pgErr = pgErr.Error()
	}
}

// sampleLoop - ряд ресурсов с шагом interval до отмены ctx.
func (c *collector) sampleLoop(ctx context.Context, interval time.Duration, origin time.Time) {
	var prevCPU, prevT float64

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s := sample{T: round3(time.Since(origin).Seconds())}

		if c.pid > 0 {
			if p, err := benchenv.ReadProcess(c.pid); err == nil {
				s.RSSKB, s.Threads = p.RSSKB, p.Threads
				if prevT > 0 && s.T > prevT {
					s.CPUPercent = round3((p.CPUSeconds - prevCPU) / (s.T - prevT) * 100)
				}

				prevCPU, prevT = p.CPUSeconds, s.T
			}
		}

		if prom, err := c.scrape(ctx); err == nil && prom != nil {
			s.Waiters, _ = prom.sum("syngo_sync_longpoll_waiters", nil)
			s.Goroutines, _ = prom.sum("go_goroutines", nil)
		}

		if c.pg != nil {
			s.PGConns, s.PGActive = pgActivity(ctx, c.pg)
		}

		c.mu.Lock()
		c.samples = append(c.samples, s)
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pgSnapshot - счетчики pg_stat_database текущей базы и pg_stat_statements.
type pgSnapshot struct {
	Version    string
	DBSize     int64
	Counters   map[string]int64
	Statements map[int64]pgStatement
	StmtErr    string
}

type pgStatement struct {
	Query   string
	Calls   int64
	TotalMS float64
	Rows    int64
}

// pgDatabaseCounters - поля pg_stat_database, которые идут в отчет разностью.
var pgDatabaseCounters = []string{
	"xact_commit", "xact_rollback", "blks_read", "blks_hit", "tup_returned", "tup_fetched",
	"tup_inserted", "tup_updated", "tup_deleted", "temp_files", "temp_bytes", "deadlocks",
}

// pgActivityQuery - соединения текущей базы, всего и активных.
const pgActivityQuery = `SELECT count(*), count(*) FILTER (WHERE state = 'active')
	FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`

func pgActivity(ctx context.Context, conn *pgx.Conn) (total, active int64) {
	pgConnMu.Lock()
	defer pgConnMu.Unlock()

	_ = conn.QueryRow(ctx, pgActivityQuery).Scan(&total, &active)

	return total, active
}

// pgConnMu - pgx.Conn не потокобезопасен: снимки и ряд идут из разных горутин.
var pgConnMu sync.Mutex

func takePGSnapshot(ctx context.Context, conn *pgx.Conn) (*pgSnapshot, error) {
	pgConnMu.Lock()
	defer pgConnMu.Unlock()

	snap := &pgSnapshot{Counters: map[string]int64{}}

	if err := conn.QueryRow(ctx, `SELECT version(), pg_database_size(current_database())`).Scan(&snap.Version, &snap.DBSize); err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	cols := strings.Join(pgDatabaseCounters, ", ")
	values := make([]any, len(pgDatabaseCounters))
	ints := make([]int64, len(pgDatabaseCounters))

	for i := range ints {
		values[i] = &ints[i]
	}

	//nolint:gosec // список колонок - константа pgDatabaseCounters
	if err := conn.QueryRow(ctx, `SELECT `+cols+` FROM pg_stat_database WHERE datname = current_database()`).Scan(values...); err != nil {
		return nil, fmt.Errorf("pg_stat_database: %w", err)
	}

	for i, name := range pgDatabaseCounters {
		snap.Counters[name] = ints[i]
	}

	snap.Statements, snap.StmtErr = pgStatements(ctx, conn)

	return snap, nil
}

// pgStatements - pg_stat_statements текущей базы; нет расширения или
// shared_preload_libraries - причина строкой.
func pgStatements(ctx context.Context, conn *pgx.Conn) (stmts map[int64]pgStatement, errText string) {
	rows, err := conn.Query(ctx, `SELECT queryid, left(query, 300), calls, total_exec_time, rows
		FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		return nil, err.Error()
	}
	defer rows.Close()

	out := map[int64]pgStatement{}

	for rows.Next() {
		var (
			id int64
			st pgStatement
		)

		if err := rows.Scan(&id, &st.Query, &st.Calls, &st.TotalMS, &st.Rows); err != nil {
			return nil, err.Error()
		}

		out[id] = st
	}

	if err := rows.Err(); err != nil {
		return nil, err.Error()
	}

	return out, ""
}

// serverReport - сторона сервера в отчете.
type serverReport struct {
	MetricsURL string `json:"metrics_url,omitempty"`
	Error      string `json:"error,omitempty"`
	Build      string `json:"build,omitempty"`
	// Kind - чьи метрики разобраны: syngo или synapse.
	Kind string `json:"kind,omitempty"`
	// Requests - прирост syngo_http_requests_total по "метод маршрут".
	Requests map[string]float64 `json:"requests,omitempty"`
	// Status - прирост по кодам ответа.
	Status map[string]float64 `json:"status,omitempty"`
	// RouteP95MS, RouteP99MS - квантили syngo_http_request_duration_seconds за окно
	// (интерполяция по корзинам сервера; для /sync включает long-poll).
	RouteP95MS map[string]float64 `json:"route_p95_ms,omitempty"`
	RouteP99MS map[string]float64 `json:"route_p99_ms,omitempty"`
	// Sync - обработка /sync без ожидания long-poll (syngo_sync_duration_seconds).
	SyncP50MS   float64 `json:"sync_p50_ms,omitempty"`
	SyncP95MS   float64 `json:"sync_p95_ms,omitempty"`
	SyncP99MS   float64 `json:"sync_p99_ms,omitempty"`
	RateLimited float64 `json:"rate_limited,omitempty"`
	// DBPoolEmptyAcquires - выдачи соединения с ожиданием (пул мал для нагрузки).
	DBPoolEmptyAcquires float64 `json:"db_pool_empty_acquires,omitempty"`
	DBPoolWaitSeconds   float64 `json:"db_pool_wait_seconds,omitempty"`
	DBPoolMax           float64 `json:"db_pool_max,omitempty"`
	GCCycles            float64 `json:"gc_cycles,omitempty"`
	GCPauseSeconds      float64 `json:"gc_pause_seconds,omitempty"`
	HeapEndBytes        float64 `json:"heap_end_bytes,omitempty"`
	LongpollWaitersMax  float64 `json:"longpoll_waiters_max,omitempty"`
	GoroutinesMax       float64 `json:"goroutines_max,omitempty"`
}

// processReport - процесс сервера (/proc/<pid>).
type processReport struct {
	PID           int     `json:"pid,omitempty"`
	Error         string  `json:"error,omitempty"`
	RSSStartKB    int64   `json:"rss_start_kb,omitempty"`
	RSSEndKB      int64   `json:"rss_end_kb,omitempty"`
	RSSMaxKB      int64   `json:"rss_max_kb,omitempty"`
	CPUSeconds    float64 `json:"cpu_seconds,omitempty"`
	CPUAvgPercent float64 `json:"cpu_avg_pct,omitempty"`
	CPUMaxPercent float64 `json:"cpu_max_pct,omitempty"`
	ThreadsMax    int64   `json:"threads_max,omitempty"`
}

// postgresReport - PostgreSQL за окно.
type postgresReport struct {
	Error         string           `json:"error,omitempty"`
	Version       string           `json:"version,omitempty"`
	DBSizeStartMB float64          `json:"db_size_start_mb,omitempty"`
	DBSizeEndMB   float64          `json:"db_size_end_mb,omitempty"`
	Counters      map[string]int64 `json:"counters,omitempty"`
	TPS           float64          `json:"tps,omitempty"`
	CacheHitRatio float64          `json:"cache_hit_ratio,omitempty"`
	ConnsMax      int64            `json:"conns_max,omitempty"`
	ActiveMax     int64            `json:"active_max,omitempty"`
	// Statements - топ запросов по суммарному времени за окно; StatementsError -
	// почему pg_stat_statements недоступен.
	Statements      []statementReport `json:"statements,omitempty"`
	StatementsError string            `json:"statements_error,omitempty"`
}

type statementReport struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
}

// topStatements - сколько запросов pg_stat_statements в отчете.
const topStatements = 15

// reports собирает разделы отчета из снимков.
func (c *collector) reports() (serverReport, processReport, postgresReport, []sample) {
	c.mu.Lock()
	defer c.mu.Unlock()

	window := c.finished.Sub(c.started)

	srv := serverReport{MetricsURL: c.metricsURL, Error: c.promErr}
	if c.promA != nil && c.promB != nil {
		fillServer(&srv, c.promA, c.promB)
	}

	proc := processReport{PID: c.pid, Error: c.procErr}
	if c.procA != nil && c.procB != nil {
		proc.RSSStartKB, proc.RSSEndKB = c.procA.RSSKB, c.procB.RSSKB
		proc.CPUSeconds = round3(c.procB.CPUSeconds - c.procA.CPUSeconds)

		if window > 0 {
			proc.CPUAvgPercent = round3(proc.CPUSeconds / window.Seconds() * 100)
		}
	}

	pg := postgresReport{Error: c.pgErr}
	if c.pgA != nil && c.pgB != nil {
		fillPostgres(&pg, c.pgA, c.pgB, window)
	}

	for _, s := range c.samples {
		proc.RSSMaxKB = max(proc.RSSMaxKB, s.RSSKB)
		proc.CPUMaxPercent = max(proc.CPUMaxPercent, s.CPUPercent)
		proc.ThreadsMax = max(proc.ThreadsMax, s.Threads)
		srv.LongpollWaitersMax = max(srv.LongpollWaitersMax, s.Waiters)
		srv.GoroutinesMax = max(srv.GoroutinesMax, s.Goroutines)
		pg.ConnsMax = max(pg.ConnsMax, s.PGConns)
		pg.ActiveMax = max(pg.ActiveMax, s.PGActive)
	}

	return srv, proc, pg, append([]sample(nil), c.samples...)
}

// fillServer - разности метрик сервера за окно: syngod (syngo_*) или Synapse
// (synapse_http_server_*: те же разрезы по маршрутам, без метрик пула и GC Go).
func fillServer(srv *serverReport, a, b promSet) {
	for _, s := range b {
		switch s.Name {
		case "syngo_build_info":
			srv.Build = strings.TrimSpace(s.Labels["version"] + " " + s.Labels["commit"] + " " + s.Labels["goversion"])
		case "synapse_build_info":
			srv.Build = strings.TrimSpace("Synapse " + s.Labels["version"] + " " + s.Labels["pythonversion"])
		}
	}

	if _, ok := b.sum("synapse_http_server_requests_received_total", nil); ok {
		srv.Kind = "synapse"
		srv.Requests = roundMap(groupedDelta(a, b, "synapse_http_server_requests_received_total", "method", "servlet"))
		srv.Status = roundMap(groupedDelta(a, b, "synapse_http_server_responses_total", "code"))
		srv.RouteP95MS = msMap(histogramQuantiles(a, b, "synapse_http_server_response_time_seconds", 0.95, "method", "servlet"))
		srv.RouteP99MS = msMap(histogramQuantiles(a, b, "synapse_http_server_response_time_seconds", 0.99, "method", "servlet"))

		return
	}

	srv.Kind = "syngo"
	srv.Requests = roundMap(groupedDelta(a, b, "syngo_http_requests_total", "method", "route"))
	srv.Status = roundMap(groupedDelta(a, b, "syngo_http_requests_total", "code"))
	srv.RouteP95MS = msMap(histogramQuantiles(a, b, "syngo_http_request_duration_seconds", 0.95, "method", "route"))
	srv.RouteP99MS = msMap(histogramQuantiles(a, b, "syngo_http_request_duration_seconds", 0.99, "method", "route"))

	for q, dst := range map[float64]*float64{0.5: &srv.SyncP50MS, 0.95: &srv.SyncP95MS, 0.99: &srv.SyncP99MS} {
		if v, ok := histogramQuantiles(a, b, "syngo_sync_duration_seconds", q)[""]; ok {
			*dst = round3(v * 1000)
		}
	}

	srv.RateLimited, _ = delta(a, b, "syngo_rate_limited_total")
	srv.DBPoolEmptyAcquires, _ = delta(a, b, "syngo_db_pool_empty_acquires_total")
	srv.DBPoolWaitSeconds, _ = delta(a, b, "syngo_db_pool_acquire_wait_seconds_total")
	srv.DBPoolWaitSeconds = round3(srv.DBPoolWaitSeconds)
	srv.DBPoolMax, _ = b.sum("syngo_db_pool_max_connections", nil)
	srv.GCCycles, _ = delta(a, b, "go_gc_cycles_total")
	srv.GCPauseSeconds, _ = delta(a, b, "go_gc_pause_seconds_total")
	srv.GCPauseSeconds = round3(srv.GCPauseSeconds)
	srv.HeapEndBytes, _ = b.sum("go_memstats_heap_alloc_bytes", nil)
}

func fillPostgres(pg *postgresReport, a, b *pgSnapshot, window time.Duration) {
	pg.Version = b.Version
	pg.DBSizeStartMB = round3(float64(a.DBSize) / (1 << 20))
	pg.DBSizeEndMB = round3(float64(b.DBSize) / (1 << 20))
	pg.Counters = map[string]int64{}

	for _, name := range pgDatabaseCounters {
		pg.Counters[name] = b.Counters[name] - a.Counters[name]
	}

	if window > 0 {
		pg.TPS = round3(float64(pg.Counters["xact_commit"]+pg.Counters["xact_rollback"]) / window.Seconds())
	}

	if reads := pg.Counters["blks_hit"] + pg.Counters["blks_read"]; reads > 0 {
		pg.CacheHitRatio = round3(float64(pg.Counters["blks_hit"]) / float64(reads))
	}

	pg.StatementsError = b.StmtErr
	if b.Statements == nil {
		return
	}

	for id, st := range b.Statements {
		// Собственные запросы генератора к pg_stat_* в топ не идут.
		if strings.Contains(st.Query, "pg_stat_") {
			continue
		}

		prev := a.Statements[id]

		calls := st.Calls - prev.Calls
		if calls <= 0 {
			continue
		}

		total := st.TotalMS - prev.TotalMS
		pg.Statements = append(pg.Statements, statementReport{
			Query: strings.Join(strings.Fields(st.Query), " "), Calls: calls,
			TotalMS: round3(total), MeanMS: round3(total / float64(calls)), Rows: st.Rows - prev.Rows,
		})
	}

	sort.Slice(pg.Statements, func(i, j int) bool { return pg.Statements[i].TotalMS > pg.Statements[j].TotalMS })

	if len(pg.Statements) > topStatements {
		pg.Statements = pg.Statements[:topStatements]
	}
}

func roundMap(m map[string]float64) map[string]float64 {
	for k, v := range m {
		m[k] = round3(v)
	}

	return m
}

func msMap(m map[string]float64) map[string]float64 {
	for k, v := range m {
		m[k] = round3(v * 1000)
	}

	return m
}
