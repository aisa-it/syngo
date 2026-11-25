// Package store отвечает за доступ к PostgreSQL: пул соединений, транзакции и
// реализации репозиториев домена.
//
// Схема БД и миграции - в каталоге migrations (ADR-0003); здесь же подключение,
// проверка живости и корректное закрытие.
package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/core"
)

// ErrNotConfigured возвращается, если DSN не задан и хранилище отключено (та же
// ошибка, что core.ErrStorageNotConfigured).
var ErrNotConfigured = core.ErrStorageNotConfigured

var _ core.HealthStore = (*DB)(nil)

// DB — обёртка над пулом соединений pgx.
type DB struct {
	pool *pgxpool.Pool
	// pending — выданные, но ещё не закоммиченные stream_ordering: из них
	// считается безопасная позиция /sync.
	pendingMu sync.Mutex
	pending   map[int64]time.Time
	// keepRedactedOriginal - хранить оригинальный content удаленных событий в
	// redactions.original_content (redaction.retention_period > 0).
	keepRedactedOriginal bool
	// outboxAllowed - политика федерации для постановки в outbox (allow_hosts,
	// deny_hosts): получатели вне ее в очередь не ставятся.
	outboxAllowed atomic.Pointer[func(destination string) bool]
	// activity - отложенная запись отметок активности; nil - метки
	// пишутся синхронно в пути запроса.
	activity atomic.Pointer[Activity]
	// roomLocks - очередь записи локальных событий комнаты без соединения.
	roomLocks roomLocks
	// noFederationOutbox - федерация выключена: запись события не читает
	// серверы комнаты и не ставит outbox.
	noFederationOutbox atomic.Bool
}

// Options — параметры подключения к PostgreSQL и трейсинга запросов.
type Options struct {
	DSN      string
	MaxConns int32
	MinConns int32
	// Tracer подключается к пулу; стенд /sync считает по нему SQL-запросы.
	Tracer pgx.QueryTracer
}

// Open создаёт пул и проверяет соединение. Если DSN пуст, возвращает nil, nil —
// сервис может работать без БД (полезно для каркаса и локальной разработки).
func Open(ctx context.Context, dsn string, maxConns, minConns int32) (*DB, error) {
	return OpenWithOptions(ctx, Options{DSN: dsn, MaxConns: maxConns, MinConns: minConns})
}

// OpenWithOptions открывает пул с трейсером (или без).
func OpenWithOptions(ctx context.Context, opts Options) (*DB, error) {
	dsn := opts.DSN
	if dsn == "" {
		return nil, nil //nolint:nilnil // отсутствие БД — легальное состояние каркаса
	}

	maxConns, minConns := opts.MaxConns, opts.MinConns

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}

	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	if minConns >= 0 {
		cfg.MinConns = minConns
	}

	if opts.Tracer != nil {
		cfg.ConnConfig.Tracer = opts.Tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return &DB{pool: pool}, nil
}

// Pool возвращает пул pgx. Нужен репозиториям и интеграционным тестам; в домене не используется.
func (db *DB) Pool() *pgxpool.Pool {
	if db == nil {
		return nil
	}

	return db.pool
}

// Ping проверяет живость соединения (используется readiness-пробой).
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return ErrNotConfigured
	}

	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}

	return nil
}

// Close закрывает пул. Безопасен для nil-получателя.
func (db *DB) Close() {
	if db == nil || db.pool == nil {
		return
	}

	db.pool.Close()
}

// PoolStats - состояние пула соединений (метрики syngo_db_pool_*);
// ok=false - пула нет.
func (db *DB) PoolStats() (core.DBPoolStats, bool) {
	pool := db.Pool()
	if pool == nil {
		return core.DBPoolStats{}, false
	}

	st := pool.Stat()

	return core.DBPoolStats{
		Acquired: st.AcquiredConns(), Idle: st.IdleConns(), Constructing: st.ConstructingConns(), Max: st.MaxConns(),
		Acquires: st.AcquireCount(), EmptyAcquires: st.EmptyAcquireCount(), EmptyAcquireWait: st.EmptyAcquireWaitTime(),
	}, true
}

// DatabaseSize - размер текущей базы в байтах (метрика syngo_db_size_bytes).
func (db *DB) DatabaseSize(ctx context.Context) (int64, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	var size int64
	if err := db.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&size); err != nil {
		return 0, fmt.Errorf("store: database size: %w", err)
	}

	return size, nil
}
