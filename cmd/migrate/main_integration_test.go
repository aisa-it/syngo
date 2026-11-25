//go:build integration

package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/testdb"
	"github.com/aisa-it/syngo/migrations"
)

// TestEmbeddedMigrations - migrate без -path применяет встроенные
// миграции до последней версии, -down 1 откатывает ровно одну.
func TestEmbeddedMigrations(t *testing.T) {
	dsn := testdb.DSN(t)

	want, err := migrations.Latest(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}

	m, err := newMigrate(options{dsn: dsn})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	var out bytes.Buffer
	if err := run([]string{"-dsn", dsn, "-up"}, &out); err != nil {
		t.Fatalf("up: %v", err)
	}

	if !strings.Contains(out.String(), "версия: "+strconv.Itoa(int(want))+", dirty: false") {
		t.Fatalf("после -up: %q, ожидали версию %d", out.String(), want)
	}

	out.Reset()

	if err := run([]string{"-dsn", dsn, "-down", "1"}, &out); err != nil {
		t.Fatalf("down 1: %v", err)
	}

	if !strings.Contains(out.String(), "версия: "+strconv.Itoa(int(want)-1)+",") {
		t.Fatalf("после -down 1: %q", out.String())
	}
}
