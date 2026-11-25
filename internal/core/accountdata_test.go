package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

func newAccountDataService(t *testing.T) (*core.AccountDataService, *coretest.MemAccountDataStore) {
	t.Helper()

	st := coretest.NewMemAccountDataStore()
	rules := core.NewPushRulesService(coretest.NewMemPushRulesStore(), core.PushRulesHooks{})

	return core.NewAccountDataService(st, rules), st
}

// Account data: запись, чтение, удаление, серверные типы и m.push_rules.
func TestAccountDataService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newAccountDataService(t)
	key := core.AccountDataKey{UserID: alice, EventType: "org.example.settings"}

	if _, err := svc.Get(ctx, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("нет записи: %v", err)
	}

	requireKind(t, svc.Set(ctx, key, json.RawMessage(`{"a":`)), core.ErrMalformed, "Malformed JSON")

	// Числа сохраняются без потери точности, JSON - в компактном виде.
	if err := svc.Set(ctx, key, json.RawMessage(`{ "big": 12345678901234567890 }`)); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, key)
	if err != nil || string(got) != `{"big":12345678901234567890}` {
		t.Fatalf("чтение: %s %v", got, err)
	}

	for _, eventType := range []string{core.ReceiptFullyRead, core.AccountDataPushRules} {
		serverKey := core.AccountDataKey{UserID: alice, EventType: eventType}
		requireKind(t, svc.Set(ctx, serverKey, json.RawMessage(`{}`)), core.ErrServerManaged, "")
		requireKind(t, svc.Delete(ctx, serverKey), core.ErrServerManaged, "")
	}

	// m.push_rules собирается из правил, даже если снимка нет.
	rules, err := svc.Get(ctx, core.AccountDataKey{UserID: alice, EventType: core.AccountDataPushRules})
	if err != nil || !strings.HasPrefix(string(rules), `{"global":`) {
		t.Fatalf("m.push_rules: %s %v", rules, err)
	}

	if err := svc.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("повторное удаление: %v", err)
	}

	if _, err := svc.Get(ctx, key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("удаленная запись: %v", err)
	}
}

// Теги: пустой список без записи, добавление и удаление по одному.
func TestAccountDataTags(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, st := newAccountDataService(t)
	room := "!r:test.local"

	tags, err := svc.Tags(ctx, alice, room)
	if err != nil || tags == nil || len(tags) != 0 {
		t.Fatalf("без тегов: %v %v", tags, err)
	}

	requireKind(t, svc.SetTag(ctx, alice, room, "u.work", json.RawMessage(`[1]`)), core.ErrMalformed, "Malformed JSON")

	if err := svc.SetTag(ctx, alice, room, "u.work", json.RawMessage(`{"order":0.5}`)); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetTag(ctx, alice, room, "m.favourite", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteTag(ctx, alice, room, "m.favourite"); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteTag(ctx, alice, room, "missing"); err != nil {
		t.Fatal(err)
	}

	tags, err = svc.Tags(ctx, alice, room)
	if err != nil || len(tags) != 1 || string(tags["u.work"]) != `{"order":0.5}` {
		t.Fatalf("теги: %v %v", tags, err)
	}

	// Испорченная запись m.tag - пустой список, сбой хранилища - ошибка.
	if err := st.SetAccountData(ctx, alice, room, core.AccountDataTag, []byte(`{"tags":[]}`)); err != nil {
		t.Fatal(err)
	}

	if tags, err := svc.Tags(ctx, alice, room); err != nil || len(tags) != 0 {
		t.Fatalf("испорченная запись: %v %v", tags, err)
	}

	st.Err = errors.New("boom")

	if _, err := svc.Tags(ctx, alice, room); !errors.Is(err, st.Err) {
		t.Fatalf("сбой хранилища: %v", err)
	}
}

// /sync: initial - все записи и m.push_rules, incremental - только изменения.
func TestAccountDataSyncChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, st := newAccountDataService(t)
	room := "!r:test.local"

	if err := svc.Set(ctx, core.AccountDataKey{UserID: alice, EventType: "a"}, json.RawMessage(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetTag(ctx, alice, room, "u.x", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	since := st.Position()

	global, perRoom, err := svc.SyncChanges(ctx, alice, 0, since, nil)
	if err != nil || global["a"] == nil || global[core.AccountDataPushRules] == nil || perRoom[room][core.AccountDataTag] == nil {
		t.Fatalf("initial: %v %v %v", global, perRoom, err)
	}

	if err := svc.Set(ctx, core.AccountDataKey{UserID: alice, EventType: "b"}, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	global, perRoom, err = svc.SyncChanges(ctx, alice, since, st.Position(), nil)
	if err != nil || len(global) != 1 || global["b"] == nil || len(perRoom) != 0 {
		t.Fatalf("incremental: %v %v %v", global, perRoom, err)
	}

	// Комната, куда вошли после since, - все ее записи.
	_, perRoom, err = svc.SyncChanges(ctx, alice, st.Position(), st.Position(), map[string]bool{room: true})
	if err != nil || perRoom[room][core.AccountDataTag] == nil {
		t.Fatalf("новая комната: %v %v", perRoom, err)
	}

	st.Err = errors.New("boom")

	if _, _, err := svc.SyncChanges(ctx, alice, 0, 1, nil); !errors.Is(err, st.Err) {
		t.Fatalf("сбой хранилища: %v", err)
	}
}
