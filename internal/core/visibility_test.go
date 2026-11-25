package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// Границы видимости: свои события членства видны всегда, остальное - по позиции.
func TestVisibilityFilter(t *testing.T) {
	t.Parallel()

	self := alice
	vis := core.Visibility{Before: 3, From: 10}
	events := []core.Event{
		{EventID: "$1", Type: "m.room.message", StreamOrdering: 1},
		{EventID: "$5", Type: "m.room.message", StreamOrdering: 5},
		{EventID: "$6", Type: "m.room.member", StateKey: &self, StreamOrdering: 6},
		{EventID: "$12", Type: "m.room.message", StreamOrdering: 12},
	}

	kept, hidden := vis.FilterVisible(alice, events)
	if !hidden || len(kept) != 3 || kept[1].EventID != "$6" {
		t.Fatalf("для alice: %v hidden=%v", kept, hidden)
	}

	kept, hidden = vis.FilterVisible(bob, events)
	if !hidden || len(kept) != 2 {
		t.Fatalf("для bob: %v hidden=%v", kept, hidden)
	}

	if kept, hidden := (core.Visibility{}).FilterVisible(bob, events); hidden || len(kept) != len(events) {
		t.Fatalf("без ограничений: %v hidden=%v", kept, hidden)
	}
}

// Сервис видимости: без комнат - nil, сбой хранилища закрывает историю.
func TestVisibilityServiceCutoffs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := coretest.NewMemVisibilityStore()
	store.Cutoffs["!a:test.local"] = core.Visibility{From: 7}

	var warned error

	svc := core.NewVisibilityService(store, func(_ context.Context, err error) { warned = err })

	if got := svc.Cutoffs(ctx, alice, nil); got != nil {
		t.Fatalf("без комнат: %v", got)
	}

	got := svc.Cutoffs(ctx, alice, []string{"!a:test.local", "!b:test.local"})
	if got["!a:test.local"].From != 7 || got["!b:test.local"].From != 0 {
		t.Fatalf("границы: %v", got)
	}

	store.Err = errors.New("db down")

	got = svc.Cutoffs(ctx, alice, []string{"!a:test.local"})
	if !errors.Is(warned, store.Err) || got["!a:test.local"].Visible(1<<61) {
		t.Fatalf("сбой: %v warned=%v", got, warned)
	}

	// Без warn сбой не роняет вызов.
	if got := core.NewVisibilityService(store, nil).Cutoffs(ctx, alice, []string{"!a:test.local"}); len(got) != 1 {
		t.Fatalf("сбой без warn: %v", got)
	}
}
