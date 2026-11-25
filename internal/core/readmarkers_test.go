package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

const markerRoom = "!room:test.local"

// Отметки прочтения: receipt в памяти и в хранилище, m.fully_read - в account
// data, загрузка после рестарта.
func TestReadMarkersService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := coretest.NewMemReadMarkerStore()
	ephemeral := core.NewEphemeralStore()
	svc := core.NewReadMarkersService(ephemeral, store, nil)

	if err := svc.Set(ctx, alice, markerRoom, core.ReceiptRead, "", "$e1"); err != nil {
		t.Fatalf("m.read: %v", err)
	}

	if err := svc.Set(ctx, alice, markerRoom, core.ReceiptFullyRead, "", "$e2"); err != nil {
		t.Fatalf("m.fully_read: %v", err)
	}

	if len(store.Receipts) != 1 || store.Receipts[0].EventID != "$e1" {
		t.Fatalf("сохраненные receipts: %+v", store.Receipts)
	}

	if got := string(store.AccountData[alice+"|"+markerRoom+"|m.fully_read"]); got != `{"event_id":"$e2"}` {
		t.Fatalf("m.fully_read: %s", got)
	}

	if got := ephemeral.Receipts(markerRoom, bob); len(got) != 1 {
		t.Fatalf("receipt в памяти: %+v", got)
	}

	// Рестарт: новое эфемерное хранилище поднимает отметки из хранилища.
	restarted := core.NewEphemeralStore()
	if err := core.NewReadMarkersService(restarted, store, nil).Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := restarted.Receipts(markerRoom, bob); len(got) != 1 || got[0].EventID != "$e1" {
		t.Fatalf("после рестарта: %+v", got)
	}

	// Сбой хранилища: receipt остается в памяти с предупреждением, m.fully_read - ошибка.
	store.Err = errors.New("db down")

	var warned error

	failing := core.NewReadMarkersService(ephemeral, store, func(_ context.Context, err error) { warned = err })

	if err := failing.Set(ctx, bob, markerRoom, core.ReceiptRead, "", "$e3"); err != nil || !errors.Is(warned, store.Err) {
		t.Fatalf("сбой receipt: err=%v warned=%v", err, warned)
	}

	if err := failing.Set(ctx, bob, markerRoom, core.ReceiptFullyRead, "", "$e3"); !errors.Is(err, store.Err) {
		t.Fatalf("сбой m.fully_read: %v", err)
	}

	if err := failing.Load(ctx); !errors.Is(err, store.Err) {
		t.Fatalf("сбой Load: %v", err)
	}
}

// Без хранилища receipts только в памяти, m.fully_read не сохраняется.
func TestReadMarkersServiceWithoutStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ephemeral := core.NewEphemeralStore()
	svc := core.NewReadMarkersService(ephemeral, nil, nil)

	if err := svc.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := svc.Set(ctx, alice, markerRoom, core.ReceiptFullyRead, "", "$e1"); err != nil {
		t.Fatalf("m.fully_read: %v", err)
	}

	if err := svc.Set(ctx, alice, markerRoom, core.ReceiptReadPrivate, "", "$e1"); err != nil {
		t.Fatalf("m.read.private: %v", err)
	}

	if got := ephemeral.Receipts(markerRoom, alice); len(got) != 1 {
		t.Fatalf("receipt в памяти: %+v", got)
	}
}
