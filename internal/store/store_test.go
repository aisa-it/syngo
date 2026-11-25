package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/store"
)

func TestOpenWithoutDSN(t *testing.T) {
	t.Parallel()

	db, err := store.Open(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}

	if db != nil {
		t.Fatal("Open() without DSN must return nil database")
	}

	// Ping и Close на nil-получателе должны быть безопасны.
	if err := db.Ping(context.Background()); !errors.Is(err, store.ErrNotConfigured) {
		t.Fatalf("Ping() = %v, want ErrNotConfigured", err)
	}

	db.Close()
}

func TestOpenInvalidDSN(t *testing.T) {
	t.Parallel()

	if _, err := store.Open(context.Background(), "not-a-dsn", 1, 0); err == nil {
		t.Fatal("Open() must fail on invalid DSN")
	}
}
