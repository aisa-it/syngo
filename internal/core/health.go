package core

import (
	"context"
	"errors"
	"time"
)

// Состояние хранилища для readiness и метрик. Проверки
// перенесены из HTTP-слоя.

// ErrStorageNotConfigured - сервер запущен без БД (DSN не задан).
var ErrStorageNotConfigured = errors.New("store: database is not configured")

// DBPoolStats - состояние пула соединений БД.
type DBPoolStats struct {
	Acquired, Idle, Constructing, Max int32
	// Acquires - выдачи соединений, EmptyAcquires - выдачи с ожиданием.
	Acquires, EmptyAcquires int64
	// EmptyAcquireWait - суммарное ожидание свободного соединения.
	EmptyAcquireWait time.Duration
}

// HealthStore - живость и метрики хранилища.
type HealthStore interface {
	Ping(ctx context.Context) error
	// CheckSchema сверяет версию схемы с ожидаемой.
	CheckSchema(ctx context.Context, want uint) error
	// PoolStats - состояние пула; ok=false - пула нет.
	PoolStats() (stats DBPoolStats, ok bool)
	DatabaseSize(ctx context.Context) (int64, error)
}

// HealthService - проверки хранилища для readiness и метрик.
type HealthService struct {
	store HealthStore
}

// NewHealthService создает сервис; store nil - сервер без БД (readiness не готов).
func NewHealthService(store HealthStore) *HealthService {
	return &HealthService{store: store}
}

// CheckDatabase - проверка базы и схемы: dbErr - база недоступна (схема тогда
// не проверяется), schemaErr - схема не совпала; schemaVersion 0 - схему не
// сверять.
func (s *HealthService) CheckDatabase(ctx context.Context, schemaVersion uint) (dbErr, schemaErr error) {
	if s.store == nil {
		return ErrStorageNotConfigured, nil
	}

	if err := s.store.Ping(ctx); err != nil {
		return err, nil
	}

	if schemaVersion > 0 {
		schemaErr = s.store.CheckSchema(ctx, schemaVersion)
	}

	return nil, schemaErr
}

// PoolStats - состояние пула; ok=false - БД нет.
func (s *HealthService) PoolStats() (DBPoolStats, bool) {
	if s.store == nil {
		return DBPoolStats{}, false
	}

	return s.store.PoolStats()
}

// DatabaseSize - размер базы в байтах.
func (s *HealthService) DatabaseSize(ctx context.Context) (int64, error) {
	if s.store == nil {
		return 0, ErrStorageNotConfigured
	}

	return s.store.DatabaseSize(ctx)
}
