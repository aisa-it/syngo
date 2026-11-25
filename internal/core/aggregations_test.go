package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// memAggregations - агрегации по родителю поверх in-memory границ видимости;
// запоминает запрошенных родителей и переданные границы.
type memAggregations struct {
	*coretest.MemVisibilityStore

	aggs    core.Aggregations
	errOn   int
	calls   [][]string
	gotVis  []map[string]core.Visibility
	callNum int
}

func (m *memAggregations) BundledAggregations(
	_ context.Context, _ string, parents []string, vis map[string]core.Visibility,
) (core.Aggregations, error) {
	m.callNum++
	m.calls = append(m.calls, parents)
	m.gotVis = append(m.gotVis, vis)

	if m.callNum == m.errOn {
		return core.Aggregations{}, errors.New("db down")
	}

	out := core.Aggregations{
		Threads: map[string]core.ThreadSummary{}, Edits: map[string]string{}, Events: map[string]core.Event{},
	}

	for _, id := range parents {
		if v, ok := m.aggs.Threads[id]; ok {
			out.Threads[id] = v
			out.Events[v.LatestEventID] = m.aggs.Events[v.LatestEventID]
		}

		if v, ok := m.aggs.Edits[id]; ok {
			out.Edits[id] = v
			out.Events[v] = m.aggs.Events[v]
		}
	}

	return out, nil
}

// Агрегации выдачи: видимость дочитывается только для
// недостающих комнат, правки последних ответов тредов - вторым запросом и
// только для целей-корней; сбой основного запроса - выдача без агрегаций,
// сбой видимости закрывает связи комнаты, сбой второго - без правок ответов.
func TestAggregationsServiceBundle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reply := core.Event{EventID: "$reply", RoomID: "!a:test"}
	newStore := func() *memAggregations {
		return &memAggregations{
			MemVisibilityStore: coretest.NewMemVisibilityStore(),
			aggs: core.Aggregations{
				Threads: map[string]core.ThreadSummary{
					"$root": {Count: 1, LatestEventID: "$reply"}, "$edit": {Count: 1, LatestEventID: "$reply"},
				},
				Edits:  map[string]string{"$reply": "$replyedit"},
				Events: map[string]core.Event{"$reply": reply, "$replyedit": {EventID: "$replyedit", RoomID: "!a:test"}},
			},
		}
	}

	targets := map[string]core.AggregationTarget{
		"$root": {RoomID: "!a:test", Threads: true},
		"$edit": {RoomID: "!b:test"},
	}
	ids := []string{"$root", "$edit"}
	known := map[string]core.Visibility{"!a:test": {Before: 5, From: 10}}

	var warned []string

	warn := func(_ context.Context, msg string, _ error) { warned = append(warned, msg) }

	st := newStore()
	st.Cutoffs["!b:test"] = core.Visibility{Before: 1, From: 2}

	bundle, ok := core.NewAggregationsService(st, warn).Bundle(ctx, "@u:test", targets, ids, known)
	if !ok || len(st.calls) != 2 || len(st.calls[1]) != 1 || st.calls[1][0] != "$reply" {
		t.Fatalf("запросы агрегаций: ok=%v %v", ok, st.calls)
	}

	if st.gotVis[0]["!a:test"] != known["!a:test"] || st.gotVis[0]["!b:test"] != st.Cutoffs["!b:test"] {
		t.Fatalf("границы видимости: %v", st.gotVis[0])
	}

	if bundle.Latest.Edits["$reply"] != "$replyedit" || !bundle.LatestTargets["$reply"].Edits {
		t.Fatalf("правка последнего ответа: %+v", bundle)
	}

	// Сбой видимости: связи недостающей комнаты закрыты.
	st = newStore()
	st.Err = errors.New("db down")

	if _, ok := core.NewAggregationsService(st, warn).Bundle(ctx, "@u:test", targets, ids, nil); !ok {
		t.Fatal("сбой видимости уронил агрегации")
	}

	if v := st.gotVis[0]["!b:test"]; v.Visible(1) || v.Visible(1<<40) {
		t.Fatalf("связи не закрыты: %+v", v)
	}

	// Сбой второго запроса: без правок ответов.
	st = newStore()
	st.errOn = 2

	bundle, ok = core.NewAggregationsService(st, nil).Bundle(ctx, "@u:test", targets, ids, known)
	if !ok || len(bundle.Latest.Edits) != 0 || len(bundle.Aggs.Threads) != 2 {
		t.Fatalf("сбой второго запроса: ok=%v %+v", ok, bundle)
	}

	// Сбой основного запроса: агрегаций нет.
	st = newStore()
	st.errOn = 1

	if _, ok := core.NewAggregationsService(st, warn).Bundle(ctx, "@u:test", targets, ids, known); ok {
		t.Fatal("сбой основного запроса не отменил агрегации")
	}

	if len(warned) != 2 || warned[0] != "history visibility for aggregations failed" ||
		warned[1] != "bundled aggregations failed" {
		t.Fatalf("журнал: %v", warned)
	}
}
