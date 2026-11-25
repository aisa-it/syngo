package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// To-device: сообщения по устройствам, пустой запрос ничего не пишет.
func TestToDeviceServiceSend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := coretest.NewMemToDeviceStore()
	svc := core.NewToDeviceService(store)

	queued, err := svc.Send(ctx, alice, "A", "m.test", "t0", nil)
	if err != nil || queued {
		t.Fatalf("пустой запрос: queued=%v err=%v", queued, err)
	}

	queued, err = svc.Send(ctx, alice, "A", "m.test", "t0", map[string]map[string]json.RawMessage{
		bob:   {"B1": json.RawMessage(`{"n":1}`), "B2": json.RawMessage(`{"n":2}`)},
		alice: {},
	})
	if err != nil || !queued {
		t.Fatalf("отправка: queued=%v err=%v", queued, err)
	}

	if got := len(store.Queued); got != 2 {
		t.Fatalf("устройств в очереди %d, ждали 2", got)
	}

	if got := string(store.Queued[core.DeviceTarget{UserID: bob, DeviceID: "B2"}][0]); got != `{"n":2}` {
		t.Fatalf("сообщение B2: %s", got)
	}

	store.Err = errors.New("db down")

	if _, err := svc.Send(ctx, alice, "A", "m.test", "t1", map[string]map[string]json.RawMessage{
		bob: {"B1": json.RawMessage(`{}`)},
	}); !errors.Is(err, store.Err) {
		t.Fatalf("сбой хранилища: %v", err)
	}
}

// /keys/changes: пустой список вместо nil, ошибка хранилища наружу.
func TestToDeviceServiceKeyChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := coretest.NewMemToDeviceStore()
	svc := core.NewToDeviceService(store)

	changed, err := svc.KeyChanges(ctx, alice, 0, 10)
	if err != nil || changed == nil || len(changed) != 0 {
		t.Fatalf("без изменений: %v %v", changed, err)
	}

	store.Changed = []string{bob, alice}

	changed, err = svc.KeyChanges(ctx, alice, 0, 10)
	if err != nil || len(changed) != 2 || changed[0] != alice {
		t.Fatalf("изменения: %v %v", changed, err)
	}

	store.Err = errors.New("db down")

	if _, err := svc.KeyChanges(ctx, alice, 0, 10); !errors.Is(err, store.Err) {
		t.Fatalf("сбой хранилища: %v", err)
	}
}

// Фильтры: сохранение и чтение, без хранилища - id без записи.
func TestFilterService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := coretest.NewMemFilterStore()
	svc := core.NewFilterService(store)

	id, err := svc.Create(ctx, alice, json.RawMessage(`{"room":{}}`))
	if err != nil || len(id) != 16 {
		t.Fatalf("создание: %q %v", id, err)
	}

	raw, err := svc.Get(ctx, alice, id)
	if err != nil || string(raw) != `{"room":{}}` {
		t.Fatalf("чтение: %s %v", raw, err)
	}

	if _, err := svc.Get(ctx, bob, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("чужой фильтр: %v", err)
	}

	store.Err = errors.New("db down")

	if _, err := svc.Create(ctx, alice, json.RawMessage(`{}`)); !errors.Is(err, store.Err) {
		t.Fatalf("сбой хранилища: %v", err)
	}

	noStore := core.NewFilterService(nil)

	id, err = noStore.Create(ctx, alice, json.RawMessage(`{}`))
	if err != nil || id == "" {
		t.Fatalf("без хранилища: %q %v", id, err)
	}

	if _, err := noStore.Get(ctx, alice, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("без хранилища чтение: %v", err)
	}
}
