//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/migrations"
)

// TestCheckSchema - проверка версии схемы на живой БД: пустая схема,
// актуальная, отставшая на одну миграцию и dirty.
func TestCheckSchema(t *testing.T) {
	m := newMigrator(t)

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down: %v", err)
	}

	ctx := context.Background()
	db := openDB(t)

	want, err := migrations.Latest(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.CheckSchema(ctx, want); !errors.Is(err, store.ErrSchemaMismatch) {
		t.Fatalf("пустая схема: %v, ожидали ErrSchemaMismatch", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	if err := db.CheckSchema(ctx, want); err != nil {
		t.Fatalf("актуальная схема: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 1: %v", err)
	}

	// Предыдущая версия - по мигратору, а не want-1: в нумерации бывают пропуски
	// (номера заняты параллельными ветками), и Force на несуществующую
	// версию ломал Down() всех следующих тестов пакета.
	prev, _, err := m.Version()
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	if err := db.CheckSchema(ctx, want); !errors.Is(err, store.ErrSchemaMismatch) {
		t.Fatalf("схема на версию старше: %v, ожидали ErrSchemaMismatch", err)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE schema_migrations SET version = $1, dirty = true`, want); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}

	if err := db.CheckSchema(ctx, want); !errors.Is(err, store.ErrSchemaMismatch) {
		t.Fatalf("dirty схема: %v, ожидали ErrSchemaMismatch", err)
	}

	// Возвращаем мигратору чистое состояние для Down() в Cleanup.
	if err := m.Force(int(prev)); err != nil {
		t.Fatalf("force: %v", err)
	}
}
