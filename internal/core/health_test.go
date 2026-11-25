package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// fakeHealth - хранилище с заданными исходами проверок.
type fakeHealth struct {
	pingErr, schemaErr, sizeErr error
	schemaChecked               uint
}

func (f *fakeHealth) Ping(context.Context) error { return f.pingErr }

func (f *fakeHealth) CheckSchema(_ context.Context, want uint) error {
	f.schemaChecked = want

	return f.schemaErr
}

func (f *fakeHealth) PoolStats() (core.DBPoolStats, bool) {
	return core.DBPoolStats{Acquired: 1, Max: 8}, true
}

func (f *fakeHealth) DatabaseSize(context.Context) (int64, error) { return 4096, f.sizeErr }

// Readiness и метрики хранилища: без БД, недоступная база, чужая схема.
func TestHealthService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	noDB := core.NewHealthService(nil)
	if dbErr, schemaErr := noDB.CheckDatabase(ctx, 5); !errors.Is(dbErr, core.ErrStorageNotConfigured) || schemaErr != nil {
		t.Fatalf("без БД: %v %v", dbErr, schemaErr)
	}

	if _, ok := noDB.PoolStats(); ok {
		t.Fatal("без БД есть пул")
	}

	if _, err := noDB.DatabaseSize(ctx); !errors.Is(err, core.ErrStorageNotConfigured) {
		t.Fatalf("без БД размер: %v", err)
	}

	store := &fakeHealth{}
	svc := core.NewHealthService(store)

	if dbErr, schemaErr := svc.CheckDatabase(ctx, 0); dbErr != nil || schemaErr != nil || store.schemaChecked != 0 {
		t.Fatalf("без версии схемы: %v %v checked=%d", dbErr, schemaErr, store.schemaChecked)
	}

	store.schemaErr = errors.New("schema mismatch")

	if dbErr, schemaErr := svc.CheckDatabase(ctx, 7); dbErr != nil || !errors.Is(schemaErr, store.schemaErr) ||
		store.schemaChecked != 7 {
		t.Fatalf("чужая схема: %v %v", dbErr, schemaErr)
	}

	store.pingErr = errors.New("db down")
	store.schemaChecked = 0

	if dbErr, schemaErr := svc.CheckDatabase(ctx, 7); !errors.Is(dbErr, store.pingErr) || schemaErr != nil ||
		store.schemaChecked != 0 {
		t.Fatalf("база недоступна: %v %v", dbErr, schemaErr)
	}

	if st, ok := svc.PoolStats(); !ok || st.Max != 8 {
		t.Fatalf("пул: %+v %v", st, ok)
	}

	if size, err := svc.DatabaseSize(ctx); err != nil || size != 4096 {
		t.Fatalf("размер: %d %v", size, err)
	}
}
