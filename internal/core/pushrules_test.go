package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/pushers"
	"github.com/aisa-it/syngo/internal/pushrules"
)

func ruleRef(kind, ruleID string) core.PushRuleRef {
	return core.PushRuleRef{UserID: alice, Scope: pushrules.ScopeGlobal, Kind: kind, RuleID: ruleID}
}

func TestCheckPushRuleRef(t *testing.T) {
	t.Parallel()

	requireKind(t, core.CheckPushRuleRef(core.PushRuleRef{Scope: "device", Kind: "room", RuleID: "x"}),
		core.ErrUnrecognized, "Unknown push rule scope")
	requireKind(t, core.CheckPushRuleRef(ruleRef("bogus", "x")), core.ErrUnrecognized, "Unknown push rule kind")
	requireKind(t, core.CheckPushRuleRef(ruleRef("room", "a/b")), core.ErrInvalidParam, "")
	requireKind(t, core.CheckPushRuleRef(ruleRef("room", "")), core.ErrInvalidParam, "")

	if err := core.CheckPushRuleRef(ruleRef("room", "!r:test.local")); err != nil {
		t.Fatal(err)
	}

	requireKind(t, core.CheckPushRulePut(ruleRef("override", ".m.rule.master"), "", ""), core.ErrInvalidParam,
		"Cannot add new rule_ids that start with '.'")
	requireKind(t, core.CheckPushRulePut(ruleRef("override", "mine"), "", ".m.rule.master"), core.ErrInvalidParam,
		"before/after cannot be a server-default rule")

	// before важнее after: серверный after при before не проверяется.
	if err := core.CheckPushRulePut(ruleRef("override", "mine"), "other", ".m.rule.master"); err != nil {
		t.Fatal(err)
	}
}

// Push-правила: создание, порядок, атрибуты, удаление и снимок m.push_rules.
func TestPushRulesService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := coretest.NewMemPushRulesStore()

	var invalidated []string

	svc := core.NewPushRulesService(st, core.PushRulesHooks{
		Invalidate: func(userID string) { invalidated = append(invalidated, userID) },
	})

	actions := json.RawMessage(`["notify"]`)

	requireKind(t, svc.Put(ctx, ruleRef("room", "!r:test.local"), "", "", core.PushRuleBody{}),
		core.ErrInvalidParam, "Missing 'actions'")
	requireKind(t, svc.Put(ctx, ruleRef("room", "!r:test.local"), "", "", core.PushRuleBody{Actions: json.RawMessage(`[1]`)}),
		core.ErrInvalidParam, "Invalid 'actions'")
	requireKind(t, svc.Put(ctx, ruleRef("content", "c"), "", "", core.PushRuleBody{Actions: actions}),
		core.ErrInvalidParam, "Missing 'pattern'")
	requireKind(t, svc.Put(ctx, ruleRef("override", "o"), "", "", core.PushRuleBody{
		Actions: actions, Conditions: json.RawMessage(`{}`),
	}), core.ErrInvalidParam, "Invalid 'conditions'")
	requireKind(t, svc.Put(ctx, ruleRef("override", "o"), "", "", core.PushRuleBody{
		Actions: actions, Conditions: json.RawMessage(`[{"key":"type"}]`),
	}), core.ErrInvalidParam, "Condition without 'kind'")

	if err := svc.Put(ctx, ruleRef("override", "first"), "", "", core.PushRuleBody{
		Actions: actions, Conditions: json.RawMessage(`[{"kind":"event_match","key":"type","pattern":"m.room.message"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	pattern := "cake"
	if err := svc.Put(ctx, ruleRef("content", "cake"), "", "", core.PushRuleBody{Actions: actions, Pattern: &pattern}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Put(ctx, ruleRef("override", "second"), "", "first", core.PushRuleBody{Actions: actions}); err != nil {
		t.Fatal(err)
	}

	requireKind(t, svc.Put(ctx, ruleRef("override", "third"), "missing", "", core.PushRuleBody{Actions: actions}),
		core.ErrPushRuleAnchorNotFound, "before/after rule not found")

	rule, err := svc.Rule(ctx, ruleRef("override", "second"))
	if err != nil || !rule.Enabled {
		t.Fatalf("новое правило включено: %+v %v", rule, err)
	}

	requireKind(t, svc.SetEnabled(ctx, ruleRef("override", "second"), nil), core.ErrMissingParam, "Missing boolean 'enabled'")
	requireKind(t, svc.SetActions(ctx, ruleRef("override", "second"), json.RawMessage(`{}`)), core.ErrInvalidParam, "Invalid 'actions'")

	off := false
	if err := svc.SetEnabled(ctx, ruleRef("override", "second"), &off); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetActions(ctx, ruleRef("override", "second"), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}

	requireKind(t, svc.SetEnabled(ctx, ruleRef("override", "nope"), &off), core.ErrNotFound, "Push rule not found")

	// Серверное правило: включить можно, неизвестное серверное - 404, удалить нельзя.
	on := true
	if err := svc.SetEnabled(ctx, ruleRef("override", pushrules.RuleMaster), &on); err != nil {
		t.Fatal(err)
	}

	master, err := svc.Rule(ctx, ruleRef("override", pushrules.RuleMaster))
	if err != nil || !master.Enabled {
		t.Fatalf("серверное правило: %+v %v", master, err)
	}

	requireKind(t, svc.SetEnabled(ctx, ruleRef("override", ".m.rule.nope"), &on), core.ErrNotFound, "")
	requireKind(t, svc.Delete(ctx, ruleRef("override", pushrules.RuleMaster)), core.ErrInvalidParam,
		"Cannot delete a server-default rule")
	requireKind(t, svc.Delete(ctx, ruleRef("override", ".m.rule.nope")), core.ErrNotFound, "Push rule not found")

	if err := svc.Delete(ctx, ruleRef("override", "first")); err != nil {
		t.Fatal(err)
	}

	requireKind(t, svc.Delete(ctx, ruleRef("override", "first")), core.ErrNotFound, "Push rule not found")
	requireKind(t, svc.Delete(ctx, ruleRef("override", "first")), core.ErrNotFound, "")

	if _, err := svc.Rule(ctx, ruleRef("override", "first")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("удаленное правило: %v", err)
	}

	// После каждого изменения - сброс кэша и снимок m.push_rules в account data.
	if len(invalidated) != 7 {
		t.Fatalf("сбросов кэша: %d", len(invalidated))
	}

	snapshot := string(st.AccountData[alice+"||"+core.AccountDataPushRules])
	if !strings.HasPrefix(snapshot, `{"global":`) || !strings.Contains(snapshot, `"rule_id":"second"`) {
		t.Fatalf("снимок m.push_rules: %s", snapshot)
	}

	// RefreshSnapshot: без снимка и не initial - не добавляет, иначе - актуальный.
	global := map[string][]byte{}
	svc.RefreshSnapshot(ctx, alice, global, false)

	if len(global) != 0 {
		t.Fatalf("снимок добавлен без нужды: %v", global)
	}

	svc.RefreshSnapshot(ctx, alice, global, true)

	if len(global[core.AccountDataPushRules]) == 0 {
		t.Fatal("нет снимка при initial sync")
	}
}

func TestPushRulesServiceStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Без хранилища: чтение - серверные правила, изменение - ErrNoStorage.
	none := core.NewPushRulesService(nil, core.PushRulesHooks{})

	if _, err := none.Rule(ctx, ruleRef("override", pushrules.RuleMaster)); err != nil {
		t.Fatal(err)
	}

	on := true
	if err := none.SetEnabled(ctx, ruleRef("override", "x"), &on); !errors.Is(err, core.ErrNoStorage) {
		t.Fatalf("без хранилища: %v", err)
	}

	if err := none.Put(ctx, ruleRef("room", "!r:test.local"), "", "", core.PushRuleBody{
		Actions: json.RawMessage(`[]`),
	}); !errors.Is(err, core.ErrNoStorage) {
		t.Fatalf("без хранилища: %v", err)
	}

	if err := none.Delete(ctx, ruleRef("room", "!r:test.local")); !errors.Is(err, core.ErrNoStorage) {
		t.Fatalf("без хранилища: %v", err)
	}

	// Сбой хранилища: ошибка наверх, снимок при initial sync не пишется, а уходит в Warn.
	st := coretest.NewMemPushRulesStore()
	st.Err = errors.New("boom")

	var warns []string

	svc := core.NewPushRulesService(st, core.PushRulesHooks{
		Warn: func(_ context.Context, msg string, _ error) { warns = append(warns, msg) },
	})

	if _, err := svc.Rules(ctx, alice); !errors.Is(err, st.Err) {
		t.Fatalf("сбой чтения: %v", err)
	}

	if _, err := svc.Rule(ctx, ruleRef("override", pushrules.RuleMaster)); !errors.Is(err, st.Err) {
		t.Fatalf("сбой чтения: %v", err)
	}

	global := map[string][]byte{}
	svc.RefreshSnapshot(ctx, alice, global, true)

	if len(global) != 0 || len(warns) != 1 {
		t.Fatalf("сбой снимка: %v %v", global, warns)
	}

	for _, err := range []error{
		svc.Put(ctx, ruleRef("room", "!r:test.local"), "", "", core.PushRuleBody{Actions: json.RawMessage(`[]`)}),
		svc.SetEnabled(ctx, ruleRef("room", "!r:test.local"), &on),
		svc.SetEnabled(ctx, ruleRef("override", pushrules.RuleMaster), &on),
		svc.Delete(ctx, ruleRef("room", "!r:test.local")),
	} {
		if !errors.Is(err, st.Err) {
			t.Fatalf("сбой записи: %v", err)
		}
	}
}

// Pushers: проверка тела, SSRF-проверка URL, лимит и удаление.
func TestPushersService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := coretest.NewMemPushersStore()
	unsafe := errors.New("private address")

	svc := core.NewPushersService(st, func(rawURL string) error {
		if rawURL != "https://push.example/_matrix/push/v1/notify" {
			return unsafe
		}

		return nil
	})

	list, err := svc.List(ctx, alice)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("пустой список: %v %v", list, err)
	}

	err = svc.Set(ctx, alice, "DEV", []byte(`{}`))
	requireKind(t, err, core.ErrInvalidParam, "")

	pusher := func(url string) []byte {
		return []byte(`{"app_id":"app","pushkey":"key","kind":"http","app_display_name":"A",` +
			`"device_display_name":"D","lang":"en","data":{"url":"` + url + `"}}`)
	}

	err = svc.Set(ctx, alice, "DEV", pusher("http://10.0.0.1/_matrix/push/v1/notify"))
	requireKind(t, err, core.ErrInvalidParam, "Unsafe push URL: private address")

	if err := svc.Set(ctx, alice, "DEV", pusher("https://push.example/_matrix/push/v1/notify")); err != nil {
		t.Fatal(err)
	}

	list, err = svc.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].DeviceID != "DEV" || list[0].UserID != alice {
		t.Fatalf("pusher: %+v %v", list, err)
	}

	for i := range pushers.MaxPerUser - 1 {
		if err := st.UpsertPusher(ctx, pushers.Pusher{UserID: alice, AppID: "app", Pushkey: string(rune('a' + i))}, 0); err != nil {
			t.Fatal(err)
		}
	}

	extra := []byte(`{"app_id":"app2","pushkey":"k","kind":"email","app_display_name":"A",` +
		`"device_display_name":"D","lang":"en","data":{}}`)
	requireKind(t, svc.Set(ctx, alice, "DEV", extra), core.ErrResourceLimit, "Too many pushers")

	if err := svc.Set(ctx, alice, "DEV", []byte(`{"app_id":"app","pushkey":"key","kind":null}`)); err != nil {
		t.Fatal(err)
	}

	list, _ = svc.List(ctx, alice)
	if len(list) != pushers.MaxPerUser-1 {
		t.Fatalf("после удаления: %d", len(list))
	}

	if err := svc.Delete(ctx, alice, list[0].AppID, list[0].Pushkey); err != nil {
		t.Fatal(err)
	}

	if list, _ = svc.List(ctx, alice); len(list) != pushers.MaxPerUser-2 {
		t.Fatalf("после Delete: %d", len(list))
	}
}
