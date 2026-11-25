//go:build integration

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // драйвер БД
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
	"github.com/aisa-it/syngo/migrations"
)

// TestCheckStartupSchema - syngod не стартует на пустой и отставшей
// схеме, стартует на актуальной.
func TestCheckStartupSchema(t *testing.T) {
	dsn := testdb.DSN(t)

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src,
		"pgx5://"+strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://"))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down: %v", err)
	}

	ctx := context.Background()

	db, err := store.Open(ctx, dsn, 2, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(db.Close)

	cfg := devConfig(t)

	if err := checkStartup(ctx, cfg, db); !errors.Is(err, store.ErrSchemaMismatch) {
		t.Fatalf("пустая схема: %v, ожидали отказ", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	if err := checkStartup(ctx, cfg, db); err != nil {
		t.Fatalf("актуальная схема: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 1: %v", err)
	}

	if err := checkStartup(ctx, cfg, db); !errors.Is(err, store.ErrSchemaMismatch) {
		t.Fatalf("отставшая схема: %v, ожидали отказ", err)
	}
}

// TestSecondInstanceRefused - второй syngod на той же базе получает
// понятную ошибку старта с описанием держателя, а не работает параллельно.
func TestSecondInstanceRefused(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := acquireInstanceLock(ctx, dsn, 0, log)
	if err != nil {
		t.Fatalf("первый экземпляр: %v", err)
	}

	t.Cleanup(func() { first.Release(ctx) })

	_, err = acquireInstanceLock(ctx, dsn, 200*time.Millisecond, log)
	if !errors.Is(err, store.ErrInstanceLocked) {
		t.Fatalf("второй экземпляр: %v, ожидали ErrInstanceLocked", err)
	}

	for _, want := range []string{"startup:", "another syngod instance", "application_name=" + first.ApplicationName() + " ", "ADR-0009"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в ошибке нет %q: %v", want, err)
		}
	}
}
