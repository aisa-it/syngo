// Command syngod — точка входа homeserver'а synapse-go.
//
// Сервис собирает конфигурацию, поднимает пул PostgreSQL (если задан DSN),
// запускает HTTP-сервер и корректно останавливается по SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/build"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/outbox"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to YAML config (default: $"+config.EnvConfig+" or "+config.DefaultPath+")")
		showVersion = flag.Bool("version", false, "print version and exit")
		healthcheck = flag.Bool("healthcheck", false,
			"check GET /health/ready of the running server ($"+config.EnvServerAddr+") and exit (container healthcheck)")
	)

	flag.Parse()

	if *healthcheck {
		return runHealthcheck()
	}

	if *showVersion {
		fmt.Println(build.String())

		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.Log, os.Stdout)
	slog.SetDefault(log)

	log.Info("starting syngod",
		"config", cfg.String(),
		"version", build.Version,
		"commit", build.Commit,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbCtx, cancel := context.WithTimeout(ctx, cfg.Database.ConnectTimeout.Std())
	defer cancel()

	db, err := store.Open(dbCtx, cfg.Database.DSN, cfg.Database.MaxConns, cfg.Database.MinConns)
	if err != nil {
		return err
	}

	defer db.Close()

	// Без федерации outbox некому доставлять: запись событий его не ставит
	// и не читает серверы комнаты под ее блокировкой.
	if !cfg.Federation.Enabled {
		db.DisableFederationOutbox()
	}

	if err := checkStartup(ctx, cfg, db); err != nil {
		return err
	}

	// Один экземпляр на базу (ADR-0009): второй syngod на той же БД
	// не стартует, а не теряет события /sync и шлет дубли федерации.
	errCh := make(chan error, 3)

	var instanceLock *store.InstanceLock

	if db != nil {
		instanceLock, err = acquireInstanceLock(ctx, cfg.Database.DSN, instanceLockWait, log)
		if err != nil {
			return err
		}

		defer instanceLock.Release(context.Background())

		go instanceLock.Watch(ctx, instanceLockCheckInterval,
			func(err error) { log.Warn("instance lock: check failed, will retry", "error", err) },
			func(err error) { errCh <- fmt.Errorf("instance lock lost: %w", err) })
	}

	if db == nil {
		log.Warn("database is not configured: readiness will answer 503 until SYNGOD_DATABASE_DSN is set")
	} else {
		// Метрика миграции паролей: сколько пользователей ещё на bcrypt (Synapse).
		argon, legacy, err := store.NewAuthStore(db).CountPasswordHashes(ctx)
		if err != nil {
			log.Warn("password hashes: count failed", "error", err)
		} else {
			log.Info("password hashes", "argon2id", argon, "bcrypt_legacy", legacy)
		}
	}

	if db != nil {
		if err := prepareRedactions(ctx, db, cfg.Redaction.RetentionPeriod.Std(), log); err != nil {
			return err
		}
	}

	// Отметки активности (last_validated_at токенов, last_accessed_at медиа)
	// пишутся пачкой в фоне, а не в пути запроса. Финальный сброс -
	// после остановки HTTP (defer выполняется до db.Close).
	if db != nil {
		activity := db.EnableActivity(store.ActivityOptions{
			FlushInterval: cfg.Database.ActivityFlushInterval.Std(),
			TokenInterval: cfg.Auth.LastValidatedInterval.Std(),
			OnError: func(err error) {
				log.Warn("activity: flush failed, will retry", "error", err)
			},
		})
		activity.Start()

		defer closeActivity(activity, log)
	}

	srv := api.New(cfg, db, log)

	if instanceLock != nil {
		srv.SetInstanceLock(instanceLock.Held)
	}

	// Readiness сверяет версию схемы, а не только доступность БД:
	// откат или ручная правка схемы на живом инстансе выводит его из балансировки.
	if db != nil {
		want, err := migrations.Latest(migrations.FS)
		if err != nil {
			return fmt.Errorf("startup: %w", err)
		}

		srv.SetSchemaVersion(want)
	}

	var (
		worker     *outbox.Worker
		stopWorker context.CancelFunc = func() {}
	)

	if cfg.Federation.Enabled {
		key, err := signing.LoadKeyFile(cfg.Federation.SigningKeyFile)
		if err != nil {
			return fmt.Errorf("federation: %w", err)
		}

		srv.SetFederationKey(&key)

		addressPolicy, err := transport.NewAddressPolicy(cfg.Federation.IPRangeBlacklist, cfg.Federation.IPRangeWhitelist)
		if err != nil {
			return fmt.Errorf("federation: %w", err)
		}

		client := transport.NewClient(transport.Options{
			Key:                &key,
			Origin:             cfg.Matrix.ServerName,
			Scheme:             cfg.Federation.Scheme,
			InsecureSkipVerify: cfg.Federation.InsecureSkipVerify,
			AddressPolicy:      addressPolicy,
			Store:              db,
			AllowHosts:         cfg.Federation.AllowHosts,
			DenyHosts:          cfg.Federation.DenyHosts,
			MaxResponseBytes:   cfg.Federation.MaxTransactionBytes,
			ConnectTimeout:     cfg.Federation.ConnectTimeout.Std(),
			RequestTimeout:     cfg.Federation.RequestTimeout.Std(),
			MinBackoff:         cfg.Federation.RetryMin.Std(),
			MaxBackoff:         cfg.Federation.RetryMax.Std(),
		})
		srv.SetFederationTransport(client)
		// Отладочный лог joiner пишет тела PDU: не подключается.
		joiner := join.New(client, db, &key, cfg.Matrix.ServerName)
		srv.SetRemoteJoiner(joiner)

		// Исходящая очередь федерации: один воркер на процесс. Инстансов
		// может быть несколько: пачку захватывает один (аренда в БД).
		// Таймаут отправки - запрос транспорта плюс сборка и фиксация пачки.
		worker = outbox.New(db, client, outbox.Options{
			Origin:         cfg.Matrix.ServerName,
			MinBackoff:     cfg.Federation.RetryMin.Std(),
			MaxBackoff:     cfg.Federation.RetryMax.Std(),
			DeliverTimeout: max(cfg.Federation.RequestTimeout.Std(), 30*time.Second) + 30*time.Second,
			PDUs:           srv.FederationPDUs,
			Logger:         log,
		})

		// Получатели вне allow_hosts/в deny_hosts в outbox не ставятся.
		if db != nil {
			db.SetOutboxDestinationFilter(client.Allowed)
		}

		workerCtx, cancelWorker := context.WithCancel(context.Background())
		stopWorker = cancelWorker

		go worker.Run(workerCtx, 5*time.Second)

		// Очередь видна в /metrics и /health/ready.
		srv.SetOutboxStats(db.OutboxMetrics)

		log.Info("federation: transport configured",
			"key_id", key.KeyID, "server_name", cfg.Matrix.ServerName,
			"allow_hosts", len(cfg.Federation.AllowHosts), "deny_hosts", len(cfg.Federation.DenyHosts),
			"max_transaction_bytes", cfg.Federation.MaxTransactionBytes)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Служебный листенер: /metrics, подробные пробы, pprof.
	var opsSrv *http.Server

	if cfg.Server.OpsAddr != "" {
		opsSrv = &http.Server{
			Addr:              cfg.Server.OpsAddr,
			Handler:           srv.OpsHandler(),
			ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
			IdleTimeout:       cfg.Server.IdleTimeout.Std(),
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}

		go func() {
			log.Info("ops server listening", "addr", opsSrv.Addr, "pprof", cfg.Server.Pprof)

			if err := opsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("ops listen: %w", err)
			}
		}()
	} else if !cfg.IsDev() {
		log.Warn("server.ops_addr is not set: /metrics is disabled (set " + config.EnvOpsAddr + ")")
	}

	go func() {
		log.Info("http server listening", "addr", httpSrv.Addr)

		listen := httpSrv.ListenAndServe
		if cfg.Server.TLSCertFile != "" {
			listen = func() error {
				return httpSrv.ListenAndServeTLS(cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
			}
		}

		if err := listen(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancelShutdown()

	// Сначала перестаём ждать в long-poll /sync: клиенты получают текущую позицию.
	srv.BeginDrain()

	// Дренаж исходящей очереди федерации: не обрываем транзакции на середине.
	if worker != nil {
		stopWorker()

		stats, err := worker.Drain(shutdownCtx, 200*time.Millisecond)
		log.Info("federation outbox drained", "depth", stats.Depth, "error", err)
	}

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	if opsSrv != nil {
		_ = opsSrv.Shutdown(shutdownCtx)
	}

	// Очереди push: новые не принимаются, текущие запросы прерываются.
	if err := srv.ClosePush(shutdownCtx); err != nil {
		log.Warn("push dispatcher stop", "error", err)
	}

	log.Info("stopped gracefully")

	return nil
}

// activityCloseTimeout - сколько ждать финальный сброс отметок активности.
const activityCloseTimeout = 5 * time.Second

// closeActivity останавливает фоновый сброс отметок активности и пишет накопленное.
func closeActivity(activity *store.Activity, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), activityCloseTimeout)
	defer cancel()

	if err := activity.Close(ctx); err != nil {
		log.Warn("activity: final flush failed", "error", err)

		return
	}

	stats := activity.Stats()
	log.Info("activity: flushed", "statements", stats.Statements, "dropped", stats.Dropped)
}

// instanceLockWait - сколько ждать блокировку экземпляра при старте: перекрытие
// с завершающимся старым процессом при рестарте (shutdown_timeout 15 с).
// instanceLockCheckInterval - как часто проверять соединение с блокировкой.
const (
	instanceLockWait          = 30 * time.Second
	instanceLockCheckInterval = 5 * time.Second
)

// acquireInstanceLock берет блокировку экземпляра. Ошибка
// ErrInstanceLocked описывает держателя: хост, адрес клиента, время старта.
func acquireInstanceLock(ctx context.Context, dsn string, wait time.Duration, log *slog.Logger) (*store.InstanceLock, error) {
	ident := store.NewInstanceIdent()

	log.Info("instance lock: acquiring", "ident", ident, "wait", wait)

	lock, err := store.AcquireInstanceLock(ctx, dsn, ident, wait)
	if err != nil {
		return nil, fmt.Errorf("startup: %w", err)
	}

	log.Info("instance lock: acquired", "ident", ident, "application_name", lock.ApplicationName())

	return lock, nil
}

// redactionPurgeInterval - как часто стирать оригиналы удаленных событий с
// истекшим сроком (в Synapse _censor_redactions раз в 5 минут).
const redactionPurgeInterval = 5 * time.Minute

// prepareRedactions - до приема запросов вырезает content событий, удаленных до
// миграции 0029 (тогда вырезание было только при чтении), и запускает стирание
// оригиналов по redaction.retention_period.
func prepareRedactions(ctx context.Context, db *store.DB, retention time.Duration, log *slog.Logger) error {
	db.SetRedactionRetention(retention)

	pruned, err := db.PruneRedactedContent(ctx)
	if err != nil {
		return fmt.Errorf("startup: prune redacted content: %w", err)
	}

	if pruned > 0 {
		log.Info("redactions: content of redacted events pruned", "events", pruned)
	}

	purge := func() {
		n, err := db.PurgeRedactedOriginals(ctx, retention)
		if err != nil {
			log.Warn("redactions: purge originals failed", "error", err)

			return
		}

		if n > 0 {
			log.Info("redactions: originals purged", "events", n)
		}
	}

	purge()

	go func() {
		ticker := time.NewTicker(redactionPurgeInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				purge()
			}
		}
	}()

	return nil
}

// checkStartup - проверки до приема запросов: схема БД
// ровно той версии, что встроена в бинарь, и доступное на запись хранилище медиа.
// Без них сервис стартовал и отвечал 500 (старая схема, том медиа root:root).
func checkStartup(ctx context.Context, cfg config.Config, db *store.DB) error {
	if cfg.Media.StorageBackend == "local" {
		if _, err := media.NewLocalStore(cfg.Media.Directory); err != nil {
			return fmt.Errorf("startup: media.directory %q: %w", cfg.Media.Directory, err)
		}
	}

	if db == nil {
		if !cfg.IsDev() {
			return errors.New("startup: database is required outside dev/test (set " + config.EnvDatabaseDSN + ")")
		}

		return nil
	}

	want, err := migrations.Latest(migrations.FS)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	if err := db.CheckSchema(ctx, want); err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	return nil
}

// newLogger собирает slog-логгер по настройкам конфигурации. Секреты в
// атрибутах маскируются.
func newLogger(cfg config.Log, out io.Writer) *slog.Logger {
	level := slog.LevelInfo

	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "info":
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: api.RedactLogAttr}

	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(out, opts))
	}

	return slog.New(slog.NewJSONHandler(out, opts))
}
