// Derived from Synapse tests/state/test_v2.py at commit
// 40f63e0fb1813161c04a0e2c930ec611466e81f7.
// Copyright (C) 2023 New Vector, Ltd.
// Modified and ported to Go by SYNGO contributors on 2026-11-27.
//
// This file is licensed under the GNU Affero General Public License, version 3
// or (at your option) any later version. See LICENSE and NOTICE.
// The upstream file was originally licensed under Apache License 2.0 and
// includes modifications made by New Vector Limited.
// SPDX-License-Identifier: AGPL-3.0-or-later

package state_test

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/aisa-it/syngo/internal/state"
)

// Юнит-тесты из tests/state/test_v2.py (pin 40f63e0): LexicographicalTestCase,
// SimpleParamStateTestCase, AuthChainDifferenceTestCase и
// test_get_power_level_for_sender.

// TestLexicographicalSort - LexicographicalTestCase.test_simple: ключ - event_id.
func TestLexicographicalSort(t *testing.T) {
	t.Parallel()

	edges := map[string][]string{"l": {"o"}, "m": {"n", "o"}, "n": {"o"}, "o": nil, "p": {"o"}}
	events := map[string]state.Event{}

	for id, auth := range edges {
		events[id] = state.Event{ID: id, Type: "m.room.power_levels", StateKey: ptr(""), Auth: auth}
	}

	got := state.PowerSort([]string{"p", "m", "o", "n", "l"}, events)
	if fmt.Sprint(got) != fmt.Sprint([]string{"o", "l", "n", "m", "p"}) {
		t.Fatalf("порядок %v, ожидали [o l n m p]", got)
	}
}

// TestSimpleParamState - SimpleParamStateTestCase: bob и charlie входят одновременно,
// разрешение двух наборов дает оба join.
func TestSimpleParamState(t *testing.T) {
	t.Parallel()

	g := newGraph(
		state.Event{
			ID: "$CREATE", Type: "m.room.create", StateKey: ptr(""), Sender: uAlice,
			Content: []byte(`{"creator":"` + uAlice + `"}`),
		},
		state.Event{
			ID: "$IMA", Type: "m.room.member", StateKey: ptr(uAlice), Sender: uAlice,
			Content: []byte(`{"membership":"join"}`), Auth: []string{"$CREATE"}, OriginTS: 1,
		},
		state.Event{
			ID: "$IJR", Type: "m.room.join_rules", StateKey: ptr(""), Sender: uAlice,
			Content: []byte(`{"join_rule":"public"}`), Auth: []string{"$CREATE", "$IMA"}, OriginTS: 2,
		},
		member("$IMB", uBob, uBob, "join", 3, "$CREATE", "$IJR"),
		member("$IMC", uCharlie, uCharlie, "join", 4, "$CREATE", "$IJR"),
	)

	res := resolve(t, g, g.set("$CREATE", "$IMA", "$IJR", "$IMB"), g.set("$CREATE", "$IMA", "$IJR", "$IMC"))

	want := g.set("$CREATE", "$IMA", "$IJR", "$IMB", "$IMC")
	if fmtState(res) != fmtState(want) {
		t.Fatalf("got %s, ожидали %s", fmtState(res), fmtState(want))
	}
}

// chainEvent - событие A..E цепочки AuthChainDifferenceTestCase.
func chainEvent(id string, auth ...string) state.Event {
	return state.Event{ID: id, Type: "m.room.member", StateKey: ptr(""), Sender: uAlice, Content: []byte(`{}`), Auth: auth}
}

func sortedIDs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

// TestAuthChainDifference - AuthChainDifferenceTestCase: test_simple,
// test_multiple_unpersisted_chain, test_unpersisted_events_different_sets.
func TestAuthChainDifference(t *testing.T) {
	t.Parallel()

	events := newGraph(chainEvent("A"), chainEvent("B", "A"), chainEvent("C", "B"), chainEvent("D", "C"),
		chainEvent("E", "C", "B"))

	set := func(pairs ...string) state.StateMap {
		out := state.StateMap{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out[state.Key{Type: pairs[i]}] = pairs[i+1]
		}

		return out
	}

	cases := []struct {
		name string
		sets []state.StateMap
		want []string
	}{
		{"simple", []state.StateMap{set("a", "A", "b", "B"), set("c", "C")}, []string{"C"}},
		{"multiple_chain", []state.StateMap{set("a", "A", "b", "B"), set("c", "C", "d", "D")}, []string{"C", "D"}},
		{"different_sets", []state.StateMap{set("a", "A", "b", "B", "e", "E"), set("c", "C", "d", "D")}, []string{"D", "E"}},
	}

	for _, tc := range cases {
		got := sortedIDs(state.AuthDifference(tc.sets, events))
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: auth difference %v, ожидали %v", tc.name, got, tc.want)
		}
	}
}

// TestConflictedSubgraph - v2.1: события auth DAG между конфликтующими A и D
// (B и C) входят в conflicted subgraph, E (потомок, не предок конфликтующего) - нет.
func TestConflictedSubgraph(t *testing.T) {
	t.Parallel()

	events := newGraph(chainEvent("A"), chainEvent("B", "A"), chainEvent("C", "B"), chainEvent("D", "C"),
		chainEvent("E", "D"), chainEvent("X"))

	got := sortedIDs(state.ConflictedSubgraph(map[string]struct{}{"A": {}, "D": {}}, events))
	if fmt.Sprint(got) != fmt.Sprint([]string{"A", "B", "C", "D"}) {
		t.Fatalf("conflicted subgraph %v, ожидали [A B C D]", got)
	}
}

// TestPowerLevelForSender - test_get_power_level_for_sender для v10 и v11 и
// создатели v12 с бесконечным уровнем.
func TestPowerLevelForSender(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"10", "11"} {
		createContent := `{"room_version":"` + version + `","creator":"` + uAlice + `"}`
		if version == "11" {
			createContent = `{"room_version":"11"}`
		}

		g := newGraph(
			state.Event{ID: "$create", Type: "m.room.create", StateKey: ptr(""), Sender: uAlice, Content: []byte(createContent)},
			state.Event{
				ID: "$ma", Type: "m.room.member", StateKey: ptr(uAlice), Sender: uAlice,
				Content: []byte(`{"membership":"join"}`), Auth: []string{"$create"},
			},
			state.Event{
				ID: "$pl", Type: "m.room.power_levels", StateKey: ptr(""), Sender: uAlice,
				Content: []byte(`{"users":{"` + uAlice + `":100,"` + uBob + `":50},"users_default":10}`),
				Auth:    []string{"$create", "$ma"},
			},
		)

		for user, want := range map[string]int{uAlice: 100, uBob: 50, uCharlie: 10} {
			ev := state.Event{ID: "$t", Type: "m.room.topic", StateKey: ptr(""), Sender: user, Auth: []string{"$create", "$ma", "$pl"}}
			if got := state.SenderPowerLevel(ev, g); got != want {
				t.Errorf("v%s: pl %s = %d, ожидали %d", version, user, got, want)
			}
		}

		// Без power_levels: создатель - 100, остальные - 0.
		onlyCreate := newGraph(g["$create"])
		for user, want := range map[string]int{uAlice: 100, uBob: 0, uCharlie: 0} {
			ev := state.Event{ID: "$t", Type: "m.room.topic", StateKey: ptr(""), Sender: user, Auth: []string{"$create", "$ma", "$pl"}}
			if got := state.SenderPowerLevel(ev, onlyCreate); got != want {
				t.Errorf("v%s без PL: pl %s = %d, ожидали %d", version, user, got, want)
			}
		}
	}

	// v12: создатель выше любого power_levels, даже если его там нет.
	v12 := newGraph(state.Event{
		ID: "$c12", RoomID: "!c12", Type: "m.room.create", StateKey: ptr(""), Sender: uAlice,
		Content: []byte(`{"room_version":"12","additional_creators":["` + uBob + `"]}`),
	})
	ev := state.Event{ID: "$t", RoomID: "!c12", Type: "m.room.topic", StateKey: ptr(""), Sender: uBob}

	if got := state.SenderPowerLevel(ev, v12); got != math.MaxInt {
		t.Errorf("v12 additional creator: pl = %d, ожидали бесконечность", got)
	}
}

// TestAlgorithmV21BaseState - v2.1 начинает итеративные проверки с пустого
// состояния: topic bob из ветки до бана проверяется по своим auth_events, а не по
// бесконфликтному бану (в v2 - отклоняется).
func TestAlgorithmV21BaseState(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	base[2].Content = []byte(`{"users":{"` + rAlice + `":100,"` + rBob + `":50},"state_default":50}`)
	g := newGraph(base...).add(
		member("$bobjoin", rBob, rBob, "join", 10, "$create", "$pl", "$jr"),
		member("$bobban", rAlice, rBob, "ban", 20, "$create", "$pl", "$alice", "$bobjoin"),
		state.Event{
			ID: "$topic", Type: "m.room.topic", StateKey: ptr(""), Sender: rBob,
			Content: []byte(`{}`), Auth: []string{"$create", "$pl", "$bobjoin"}, OriginTS: 15,
		},
	)

	sets := []state.StateMap{g.set(append(ids(base), "$bobban", "$topic")...), g.set(append(ids(base), "$bobban")...)}

	v2, err := state.New().Resolve(sets, g)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := v2[keyTopic]; ok {
		t.Errorf("v2: topic = %s, ожидали отсутствие (bob забанен в базовом состоянии)", got)
	}

	v21, err := state.New().WithAlgorithm(state.V21).Resolve(sets, g)
	if err != nil {
		t.Fatal(err)
	}

	if got := v21[keyTopic]; got != "$topic" {
		t.Errorf("v2.1: topic = %q, ожидали $topic", got)
	}

	if v21[keyBob] != "$bobban" {
		t.Errorf("v2.1: member bob = %q, ожидали $bobban (бесконфликтное)", v21[keyBob])
	}

	if state.AlgorithmFor("12") != state.V21 || state.AlgorithmFor("11") != state.V2 {
		t.Error("AlgorithmFor: v12 - v2.1, v11 - v2")
	}
}

// TestAuthorized - проверка по состоянию до события: забаненный не входит, участник
// пишет topic, v12 - create неявно.
func TestAuthorized(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...).add(member("$bobban", rAlice, rBob, "ban", 10, "$create", "$pl", "$alice"))
	before := g.set(append(ids(base), "$bobban")...)

	if state.Authorized(member("$rejoin", rBob, rBob, "join", 20, "$create", "$pl", "$jr"), before, g) {
		t.Error("join забаненного авторизован")
	}

	topic := state.Event{ID: "$t", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice, Content: []byte(`{}`)}
	if !state.Authorized(topic, before, g) {
		t.Error("topic админа не авторизован")
	}

	msg := state.Event{ID: "$m", Type: "m.room.message", Sender: rBob, Content: []byte(`{}`)}
	if state.Authorized(msg, before, g) {
		t.Error("сообщение забаненного авторизовано")
	}

	after := state.StateAfter(before, topic)
	if after[keyTopic] != "$t" || before[keyTopic] != "" {
		t.Error("StateAfter должен копировать состояние и добавлять событие")
	}
}
