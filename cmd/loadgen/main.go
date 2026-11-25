// Command loadgen - генератор нагрузки на Matrix homeserver по сети.
//
// В отличие от cmd/syncload (слой /sync в процессе, без сети), loadgen нагружает
// запущенный сервер целиком - syngod или Synapse - через Client-Server API, как
// это делают клиенты:
//
//   - пул пользователей (регистрация через registration_shared_secret, открытую
//     регистрацию или вход уже созданных), у каждого устройство с ключами E2EE и
//     long-poll /sync с фильтром Element (lazy_load_members, timeline 20);
//   - смесь действий по весам: сообщения (часть - m.room.encrypted), typing,
//     read receipts, /messages назад, /keys/query и /keys/upload, загрузка и
//     скачивание медиа, создание комнат с приглашениями, вход и выход;
//   - закрытая модель (активные пользователи и think time) или открытая (целевая
//     частота действий), плавный рост, разогрев, окно замера, drain.
//
// Главная метрика - задержка доставки: от ответа на PUT /send у отправителя до
// появления события в /sync каждого получателя (p50/p95/p99). Плюс по эндпоинтам
// p50/p95/p99, RPS, коды ответов и 429; со стороны сервера - разности /metrics,
// RSS и CPU процесса, pg_stat_database и pg_stat_statements.
//
// Отчет - report.json и report.md в -out-dir. Код выхода 2 - не пройден гейт
// (доля ошибок, полнота доставки); 1 - сбой прогона.
//
// Примеры:
//
//	loadgen -target http://127.0.0.1:38008 -profile S -shared-secret "$SECRET"
//	make load-smoke
//	make load PROFILE=M
//
// Подробно: docs/load-testing.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/benchenv"
)

// Переменные окружения: секреты не передаются флагами (видны в ps).
const (
	//nolint:gosec // имя переменной окружения, а не секрет
	envSharedSecret = "LOADGEN_SHARED_SECRET"
	envPGDSN        = "LOADGEN_PG_DSN"
	//nolint:gosec // имя переменной окружения, а не секрет
	envPassword = "LOADGEN_PASSWORD"
)

// options - флаги запуска.
type options struct {
	target      string
	kind        string
	profile     string
	profileFile string
	register    string
	prefix      string
	tokenCache  string
	metricsURL  string
	serverPID   int
	pgDSN       string
	outDir      string
	reportName  string
	commit      string
	labels      labelFlags
	verbose     bool
	strictSLO   bool
	sampleEvery time.Duration
	th          thresholds

	// overrides - поля профиля, заданные флагами (применяются поверх профиля).
	users, rooms, roomsPerUser int
	duration, warmup, ramp     time.Duration
	think                      time.Duration
	rps                        float64
	mode                       string
	seed                       int64
}

// labelFlags - повторяемый -label key=value (конфиг стенда в отчет).
type labelFlags map[string]string

func (l labelFlags) String() string { return fmt.Sprint(map[string]string(l)) }

func (l labelFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("-label: ожидали key=value, получили %q", v)
	}

	l[strings.TrimSpace(k)] = strings.TrimSpace(val)

	return nil
}

func parseFlags(args []string) (options, error) {
	opts := options{labels: labelFlags{}}
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)

	fs.StringVar(&opts.target, "target", "http://127.0.0.1:38008", "базовый URL Client-Server API")
	fs.StringVar(&opts.kind, "server-kind", "auto", "syngo | synapse | auto (по /metrics и заголовку Server)")
	fs.StringVar(&opts.profile, "profile", "S", "встроенный профиль: S, M, L")
	fs.StringVar(&opts.profileFile, "profile-file", "", "JSON-профиль (поле base - встроенный профиль под ним)")
	fs.StringVar(&opts.register, "register", regSharedSecret, "как завести пользователей: shared-secret | open | login")
	fs.StringVar(&opts.prefix, "user-prefix", "lg", "префикс localpart пользователей и алиасов комнат")
	fs.StringVar(&opts.tokenCache, "token-cache", "var/loadtest/tokens.json", "кеш токенов пула между прогонами (\"\" - без кеша)")
	fs.StringVar(&opts.metricsURL, "metrics-url", "", "Prometheus /metrics сервера (ops-листенер syngod)")
	fs.IntVar(&opts.serverPID, "server-pid", 0, "PID процесса сервера: RSS и CPU из /proc")
	fs.StringVar(&opts.pgDSN, "pg-dsn", "", "DSN базы сервера для pg_stat_* (или "+envPGDSN+")")
	fs.StringVar(&opts.outDir, "out-dir", "var/loadtest", "каталог отчетов")
	fs.StringVar(&opts.reportName, "report-name", "", "имя подкаталога отчета (по умолчанию <время>-<профиль>-<сервер>)")
	fs.StringVar(&opts.commit, "commit", "", "коммит checkout (в отчет)")
	fs.Var(opts.labels, "label", "параметр стенда в отчет: key=value (повторяемый)")
	fs.BoolVar(&opts.verbose, "v", false, "подробный лог (ошибки отдельных запросов)")
	fs.BoolVar(&opts.strictSLO, "strict-slo", false, "непройденные SLO-ориентиры тоже дают код выхода 2")
	fs.DurationVar(&opts.sampleEvery, "sample-interval", 5*time.Second, "шаг ряда RSS/CPU/соединений")
	fs.Float64Var(&opts.th.maxErrorRate, "max-error-rate", 0.01, "гейт: доля ошибок без 429")
	fs.Float64Var(&opts.th.minDelivery, "min-delivery", 0.99, "гейт: полнота доставки")
	fs.DurationVar(&opts.th.sloDeliveryP95, "slo-delivery-p95", time.Second, "SLO: p95 доставки")
	fs.DurationVar(&opts.th.sloDeliveryP99, "slo-delivery-p99", 2*time.Second, "SLO: p99 доставки")
	fs.DurationVar(&opts.th.sloSendP95, "slo-send-p95", 500*time.Millisecond, "SLO: p95 PUT /send")

	fs.IntVar(&opts.users, "users", 0, "переопределить users профиля")
	fs.IntVar(&opts.rooms, "rooms", 0, "переопределить rooms")
	fs.IntVar(&opts.roomsPerUser, "rooms-per-user", 0, "переопределить rooms_per_user")
	fs.DurationVar(&opts.duration, "duration", 0, "переопределить duration (окно замера)")
	fs.DurationVar(&opts.warmup, "warmup", 0, "переопределить warmup")
	fs.DurationVar(&opts.ramp, "ramp", 0, "переопределить ramp")
	fs.DurationVar(&opts.think, "think", 0, "переопределить think_time")
	fs.Float64Var(&opts.rps, "rps", 0, "открытая модель с этой частотой действий (mode rps)")
	fs.StringVar(&opts.mode, "mode", "", "переопределить mode: users | rps")
	fs.Int64Var(&opts.seed, "seed", 0, "переопределить seed")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	if opts.pgDSN == "" {
		opts.pgDSN = os.Getenv(envPGDSN)
	}

	opts.target = strings.TrimRight(opts.target, "/")

	return opts, nil
}

// applyOverrides - флаги поверх профиля (ненулевые значения).
func applyOverrides(p *Profile, o options) {
	setInt := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}

	setDur := func(dst *Duration, v time.Duration) {
		if v > 0 {
			*dst = Duration(v)
		}
	}

	setInt(&p.Users, o.users)
	setInt(&p.Rooms, o.rooms)
	setInt(&p.RoomsPerUser, o.roomsPerUser)
	setDur(&p.Duration, o.duration)
	setDur(&p.Warmup, o.warmup)
	setDur(&p.Ramp, o.ramp)
	setDur(&p.ThinkTime, o.think)

	if o.rps > 0 {
		p.Mode, p.TargetRPS = modeRPS, o.rps
	}

	if o.mode != "" {
		p.Mode = o.mode
	}

	if o.seed != 0 {
		p.Seed = o.seed
	}

	// Меньше комнат, чем rooms_per_user, после -rooms: подрезаем, а не падаем.
	if p.RoomsPerUser > p.Rooms {
		p.RoomsPerUser = p.Rooms
	}

	// Ramp длиннее warmup после -warmup: замер на неполной нагрузке, ramp подрезаем.
	if p.Ramp > p.Warmup {
		p.Ramp = p.Warmup
	}
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
	}

	os.Exit(code)
}

// newRunID - короткий id прогона: метка сообщений и OTK, имя устройства.
func newRunID() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)

	return hex.EncodeToString(buf)
}

func run(args []string) (int, error) {
	opts, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}

		return 1, err
	}

	prof, err := loadProfile(opts.profile, opts.profileFile)
	if err != nil {
		return 1, err
	}

	applyOverrides(&prof, opts)

	if err := prof.Validate(); err != nil {
		return 1, fmt.Errorf("профиль: %w", err)
	}

	setup := setupOptions{
		register: opts.register, sharedSecret: os.Getenv(envSharedSecret), prefix: opts.prefix,
		password: os.Getenv(envPassword),
	}
	if setup.password == "" {
		setup.password = "loadgen-" + opts.prefix + "-password"
	}

	if setup.register == regSharedSecret && setup.sharedSecret == "" {
		return 1, errors.New("-register shared-secret: нужен " + envSharedSecret)
	}

	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep, err := execute(ctx, log, opts, prof, setup)
	if err != nil {
		return 1, err
	}

	dir := filepath.Join(opts.outDir, reportDirName(opts, rep))

	jsonPath, mdPath, err := writeReport(dir, rep)
	if err != nil {
		return 1, err
	}

	printSummary(rep, jsonPath, mdPath)

	if bad := failed(rep.Checks, opts.strictSLO); len(bad) > 0 {
		for _, c := range bad {
			fmt.Fprintf(os.Stderr, "loadgen: проверка не пройдена: %s = %g (порог %s %g)\n", c.Name, c.Value, c.Op, c.Limit)
		}

		return 2, nil
	}

	return 0, nil
}

func reportDirName(opts options, rep *report) string {
	if opts.reportName != "" {
		return opts.reportName
	}

	return fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102-150405"), rep.Profile.Name, rep.Target.Kind)
}

// execute - подготовка, нагрузка и сбор отчета.
func execute(ctx context.Context, log *slog.Logger, opts options, prof Profile, setup setupOptions) (*report, error) {
	cache, err := loadTokenCache(opts.tokenCache)
	if err != nil {
		return nil, err
	}

	httpClient := newHTTPClient(prof.Users, prof.SyncTimeout.D()+30*time.Second)
	runID := newRunID()

	agents := make([]*agent, prof.Users)
	for i := range agents {
		agents[i] = newAgent(i, prof.Seed)
		agents[i].localpart = fmt.Sprintf("%s-%05d", opts.prefix, i)
	}

	setRec := newRecorder()
	setRec.measuring.Store(true)

	e := &engine{
		prof: prof, target: opts.target, runID: runID, log: log, setup: setup, cache: cache,
		rec: newRecorder(), setRec: setRec, tracker: newDeliveryTracker(runID), w: newWorld(agents),
		picker: newPicker(prof.sortedWeights()), payload: make([]byte, prof.MediaKB*1024*3/2+1),
	}
	_, _ = rand.Read(e.payload)
	e.c = &client{base: opts.target, http: httpClient, rec: e.rec}
	e.setupC = &client{base: opts.target, http: httpClient, rec: setRec}

	target := probeTarget(ctx, httpClient, opts)

	var pgConn *pgx.Conn

	pgVersion := ""

	if opts.pgDSN != "" {
		pgConn, err = pgx.Connect(ctx, opts.pgDSN)
		if err != nil {
			log.Warn("postgres недоступен, pg_stat_* в отчет не попадут", "error", err)
		} else {
			defer pgConn.Close(context.WithoutCancel(ctx))

			_ = pgConn.QueryRow(ctx, `SELECT 'PostgreSQL ' || current_setting('server_version')`).Scan(&pgVersion)
		}
	}

	col := newCollector(opts.metricsURL, opts.serverPID, pgConn)

	log.Info("подготовка", "target", opts.target, "kind", target.Kind, "profile", prof.Name,
		"users", prof.Users, "rooms", prof.Rooms, "run", runID)

	st, err := e.runSetup(ctx)
	if err != nil {
		return nil, fmt.Errorf("подготовка: %w", err)
	}

	log.Info("подготовка завершена", "seconds", st.Seconds, "memberships", st.Memberships, "largest_room", st.LargestRoom)

	sampleCtx, stopSamples := context.WithCancel(ctx)
	defer stopSamples()

	origin := time.Now()
	e.onWindowStart = func() {
		col.snapshot(ctx, false)

		go col.sampleLoop(sampleCtx, opts.sampleEvery, origin)
	}
	e.onWindowEnd = func() {
		stopSamples()
		col.snapshot(ctx, true)
	}

	e.runLoad(ctx)

	if ctx.Err() != nil {
		return nil, errors.New("прерван")
	}

	window := e.window()
	eps, counters := e.rec.snapshot(window)
	setupEps, _ := setRec.snapshot(0)
	srv, proc, pg, samples := col.reports()
	rooms, memberships := e.w.roomStats()

	if target.Build == "" {
		target.Build = srv.Build
	}

	if target.Kind == "auto" {
		target.Kind = "unknown"
		if srv.Kind != "" {
			target.Kind = srv.Kind
		}
	}

	rep := &report{
		Tool: "loadgen", Format: reportFormat, RunID: runID, Generated: time.Now().UTC().Format(time.RFC3339),
		Commit: opts.commit, Target: target, Hardware: benchenv.DetectHardware(pgVersion), Profile: prof,
		Config: opts.labels, Setup: st, SetupEndpoints: setupEps, Endpoints: eps, Counters: counters,
		Delivery: e.tracker.report(), Server: srv, Process: proc, Postgres: pg, Samples: samples,
	}

	rep.Summary = summary{
		totals: sumEndpoints(eps, window), WindowSeconds: round3(window.Seconds()),
		Actions: counters["actions"], ExpectedActionsRPS: round3(prof.expectedActionRate()),
		ActiveUsers: len(agents), Rooms: rooms, Memberships: memberships,
	}

	if window > 0 {
		rep.Summary.ActionsRPS = round3(float64(counters["actions"]) / window.Seconds())
	}

	rep.Checks = buildChecks(rep, opts.th)

	return rep, nil
}

// probeTarget - версии CS API и заголовок Server; вид сервера по Server, если не задан.
func probeTarget(ctx context.Context, hc *http.Client, opts options) targetInfo {
	info := targetInfo{BaseURL: opts.target, Kind: opts.kind}

	var versions struct {
		Versions []string `json:"versions"`
	}

	c := &client{base: opts.target, http: hc, rec: newRecorder()}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, opts.target+"/_matrix/client/versions", http.NoBody)
	if err == nil {
		if resp, err := hc.Do(req); err == nil {
			info.ServerHeader = resp.Header.Get("Server")
			_ = resp.Body.Close()
		}
	}

	if err := c.call(ctx, request{endpoint: "versions", method: http.MethodGet, path: "/_matrix/client/versions", out: &versions}); err == nil {
		info.Versions = versions.Versions
	}

	if info.Kind == "auto" && strings.Contains(strings.ToLower(info.ServerHeader), "synapse") {
		info.Kind = "synapse"
	}

	return info
}

// printSummary - короткий итог в stdout.
func printSummary(r *report, jsonPath, mdPath string) {
	d := r.Delivery
	fmt.Printf("профиль %s, %s: окно %.0f с, действий %.1f/с, запросов %.1f/с, ошибок %d (429: %d)\n",
		r.Profile.Name, r.Target.Kind, r.Summary.WindowSeconds, r.Summary.ActionsRPS, r.Summary.RPS,
		r.Summary.Errors, r.Summary.RateLimited)
	fmt.Printf("доставка: p50 %.1f мс, p95 %.1f мс, p99 %.1f мс, полнота %.3f (%d из %d)\n",
		d.FromAck.P50MS, d.FromAck.P95MS, d.FromAck.P99MS, d.Completeness, d.Delivered, d.Expected)
	fmt.Printf("отчет: %s\n        %s\n", mdPath, jsonPath)
}
