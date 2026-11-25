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
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/state"
)

// Differential-эталоны из Synapse: tests/state/test_v2.py (pin 40f63e0), все 7
// кейсов StateTestCase без изменений. Ожидаемые состояния заданы
// разработчиками Synapse, а не нашей реализацией. Граф событий строится так же,
// как build_event_graph: topological order по node id, состояние перед событием с
// несколькими prev_events - разрешение наборов состояний после каждого prev,
// auth_events - из состояния перед событием, origin_server_ts - счетчик в порядке
// построения. Проверка - как do_check: состояние в END, отфильтрованное до ключей,
// изменившихся с START или названных в expected, должно быть РАВНО expected.

const (
	uAlice   = "@alice:example.com"
	uBob     = "@bob:example.com"
	uCharlie = "@charlie:example.com"
	uEvelyn  = "@evelyn:example.com"
	uZara    = "@zara:example.com"
)

type node struct {
	ID       string
	Sender   string
	Type     string
	StateKey *string
	Content  map[string]any
}

func sk(s string) *string { return &s }

type synapseCase struct {
	name     string
	events   []node
	edges    [][]string
	expected []string
}

func initialEvents() []node {
	join := map[string]any{"membership": "join"}

	return []node{
		{"CREATE", uAlice, "m.room.create", sk(""), map[string]any{"creator": uAlice}},
		{"IMA", uAlice, "m.room.member", sk(uAlice), join},
		{"IPOWER", uAlice, "m.room.power_levels", sk(""), map[string]any{"users": map[string]any{uAlice: 100}}},
		{"IJR", uAlice, "m.room.join_rules", sk(""), map[string]any{"join_rule": "public"}},
		{"IMB", uBob, "m.room.member", sk(uBob), join},
		{"IMC", uCharlie, "m.room.member", sk(uCharlie), join},
		{"IMZ", uZara, "m.room.member", sk(uZara), join},
		{"START", uZara, "m.room.message", nil, map[string]any{}},
		{"END", uZara, "m.room.message", nil, map[string]any{}},
	}
}

var initialEdges = []string{"START", "IMZ", "IMC", "IMB", "IJR", "IPOWER", "IMA", "CREATE"}

func m(id, sender, stateKey string, content map[string]any) node {
	return node{ID: id, Sender: sender, Type: "m.room.member", StateKey: sk(stateKey), Content: content}
}

// synapseCases - кейсы StateTestCase: test_ban_vs_pl, test_join_rule_evasion,
// test_offtopic_pl, test_topic_basic, test_topic_reset, test_topic, test_mainline_sort.
func synapseCases() []synapseCase {
	pl := func(users map[string]any) map[string]any { return map[string]any{"users": users} }
	topic := func(id, sender string) node {
		return node{ID: id, Sender: sender, Type: "m.room.topic", StateKey: sk(""), Content: map[string]any{}}
	}
	plEv := func(id, sender string, users map[string]any) node {
		return node{ID: id, Sender: sender, Type: "m.room.power_levels", StateKey: sk(""), Content: pl(users)}
	}

	return []synapseCase{
		{
			name: "ban_vs_pl",
			events: []node{
				plEv("PA", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				m("MA", uAlice, uAlice, map[string]any{"membership": "join"}),
				m("MB", uAlice, uBob, map[string]any{"membership": "ban"}),
				plEv("PB", uBob, map[string]any{uAlice: 100, uBob: 50}),
			},
			edges:    [][]string{{"END", "MB", "MA", "PA", "START"}, {"END", "PB", "PA"}},
			expected: []string{"PA", "MA", "MB"},
		},
		{
			name: "join_rule_evasion",
			events: []node{
				// Ключ именно "join_rules", как в оригинале теста Synapse.
				{"JR", uAlice, "m.room.join_rules", sk(""), map[string]any{"join_rules": "private"}},
				m("ME", uEvelyn, uEvelyn, map[string]any{"membership": "join"}),
			},
			edges:    [][]string{{"END", "JR", "START"}, {"END", "ME", "START"}},
			expected: []string{"JR"},
		},
		{
			name: "offtopic_pl",
			events: []node{
				plEv("PA", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				plEv("PB", uBob, map[string]any{uAlice: 100, uBob: 50, uCharlie: 50}),
				plEv("PC", uCharlie, map[string]any{uAlice: 100, uBob: 50, uCharlie: 0}),
			},
			edges:    [][]string{{"END", "PC", "PB", "PA", "START"}, {"END", "PA"}},
			expected: []string{"PC"},
		},
		{
			name: "topic_basic",
			events: []node{
				topic("T1", uAlice),
				plEv("PA1", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				topic("T2", uAlice),
				plEv("PA2", uAlice, map[string]any{uAlice: 100, uBob: 0}),
				plEv("PB", uBob, map[string]any{uAlice: 100, uBob: 50}),
				topic("T3", uBob),
			},
			edges:    [][]string{{"END", "PA2", "T2", "PA1", "T1", "START"}, {"END", "T3", "PB", "PA1"}},
			expected: []string{"PA2", "T2"},
		},
		{
			name: "topic_reset",
			events: []node{
				topic("T1", uAlice),
				plEv("PA", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				topic("T2", uBob),
				m("MB", uAlice, uBob, map[string]any{"membership": "ban"}),
			},
			edges:    [][]string{{"END", "MB", "T2", "PA", "T1", "START"}, {"END", "T1"}},
			expected: []string{"T1", "MB", "PA"},
		},
		{
			name: "topic",
			events: []node{
				topic("T1", uAlice),
				plEv("PA1", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				topic("T2", uAlice),
				plEv("PA2", uAlice, map[string]any{uAlice: 100, uBob: 0}),
				plEv("PB", uBob, map[string]any{uAlice: 100, uBob: 50}),
				topic("T3", uBob),
				{"MZ1", uZara, "m.room.message", nil, map[string]any{}},
				topic("T4", uAlice),
			},
			edges: [][]string{
				{"END", "T4", "MZ1", "PA2", "T2", "PA1", "T1", "START"},
				{"END", "MZ1", "T3", "PB", "PA1"},
			},
			expected: []string{"T4", "PA2"},
		},
		{
			name: "mainline_sort",
			events: []node{
				topic("T1", uAlice),
				plEv("PA1", uAlice, map[string]any{uAlice: 100, uBob: 50}),
				topic("T2", uAlice),
				{"PA2", uAlice, "m.room.power_levels", sk(""), map[string]any{
					"users":  map[string]any{uAlice: 100, uBob: 50},
					"events": map[string]any{"m.room.power_levels": 100},
				}},
				plEv("PB", uBob, map[string]any{uAlice: 100, uBob: 50}),
				topic("T3", uBob),
				topic("T4", uAlice),
			},
			edges: [][]string{
				{"END", "T3", "PA2", "T2", "PA1", "T1", "START"},
				{"END", "T4", "PB", "PA1"},
			},
			expected: []string{"T3", "PA2"},
		},
	}
}

// buildGraph повторяет build_event_graph из Synapse: topological order по node id,
// состояние перед событием с несколькими prev - разрешение наборов состояний после
// каждого prev, auth_events - из состояния перед событием, ts - счетчик.
func buildGraph(extra []node, edges [][]string) (events map[string]state.Event, stateAt map[string]state.StateMap) {
	nodes := map[string]node{}
	for _, ev := range initialEvents() {
		nodes[ev.ID] = ev
	}

	for _, ev := range extra {
		nodes[ev.ID] = ev
	}

	graph := map[string]map[string]struct{}{}
	for id := range nodes {
		graph[id] = map[string]struct{}{}
	}

	addEdges := func(chain []string) {
		for i := 0; i+1 < len(chain); i++ {
			graph[chain[i]][chain[i+1]] = struct{}{}
		}
	}

	addEdges(initialEdges)
	for _, chain := range edges {
		addEdges(chain)
	}

	byEventID := map[string]state.Event{}
	stateAt = map[string]state.StateMap{}

	ts := int64(0)

	remaining := map[string]struct{}{}
	for id := range nodes {
		remaining[id] = struct{}{}
	}

	built := map[string]struct{}{}

	for len(remaining) > 0 {
		// Кандидаты - узлы, все prev которых уже построены.
		var ready []string

		for id := range remaining {
			ok := true

			for prev := range graph[id] {
				if _, done := built[prev]; !done {
					ok = false

					break
				}
			}

			if ok {
				ready = append(ready, id)
			}
		}

		if len(ready) == 0 {
			panic("cycle in event graph")
		}

		sort.Strings(ready)
		id := ready[0]
		delete(remaining, id)
		built[id] = struct{}{}

		prevNodes := make([]string, 0, len(graph[id]))
		for prev := range graph[id] {
			prevNodes = append(prevNodes, prev)
		}

		sort.Strings(prevNodes)

		prevIDs := make([]string, 0, len(prevNodes))
		sets := make([]state.StateMap, 0, len(prevNodes))

		for _, prev := range prevNodes {
			prevIDs = append(prevIDs, eventID(prev))
			sets = append(sets, stateAt[prev])
		}

		before := state.StateMap{}

		switch len(sets) {
		case 0:
		case 1:
			for k, v := range sets[0] {
				before[k] = v
			}
		default:
			resolved, err := state.New().Resolve(sets, byEventID)
			if err != nil {
				panic(err)
			}

			for k, v := range resolved {
				before[k] = v
			}
		}

		content, err := json.Marshal(nodes[id].Content)
		if err != nil {
			panic(err)
		}

		// auth_events - по auth-типам события из состояния перед ним.
		authIDs := []string{}
		for key := range authTypesOf(nodes[id]) {
			if evID, ok := before[key]; ok {
				authIDs = append(authIDs, evID)
			}
		}

		sort.Strings(authIDs)

		ev := state.Event{
			ID: eventID(id), Type: nodes[id].Type, StateKey: nodes[id].StateKey, Sender: nodes[id].Sender,
			Content: content, Prev: prevIDs, Auth: authIDs, OriginTS: ts,
		}
		ts++

		byEventID[ev.ID] = ev
		stateAt[id] = state.StateAfter(before, ev)
	}

	return byEventID, stateAt
}

// eventID - event_id узла (localpart - node id, домен example.com, как EventID в тестах).
func eventID(nodeID string) string { return "$" + nodeID + ":example.com" }

// authTypesOf - auth-типы события, как auth_types_for_event(RoomVersions.V6) в
// тесте Synapse: join_rules нужен для join, invite и knock.
func authTypesOf(ev node) map[state.Key]struct{} {
	out := map[state.Key]struct{}{
		{Type: "m.room.create", StateKey: ""}:        {},
		{Type: "m.room.power_levels", StateKey: ""}:  {},
		{Type: "m.room.member", StateKey: ev.Sender}: {},
	}

	if ev.Type == "m.room.create" {
		return map[state.Key]struct{}{}
	}

	if ev.Type == "m.room.member" && ev.StateKey != nil {
		out[state.Key{Type: "m.room.member", StateKey: *ev.StateKey}] = struct{}{}

		switch membership, _ := ev.Content["membership"].(string); membership {
		case "join", "invite", "knock":
			out[state.Key{Type: "m.room.join_rules", StateKey: ""}] = struct{}{}
		}
	}

	return out
}

// fmtState - состояние в виде отсортированных строк type|state_key=node.
func fmtState(st map[state.Key]string) string {
	rows := make([]string, 0, len(st))
	for k, v := range st {
		rows = append(rows, k.Type+"|"+k.StateKey+"="+shortID(v))
	}

	sort.Strings(rows)

	return strings.Join(rows, " ")
}

// TestSynapseDifferential - do_check из test_v2.py 1:1 для всех 7 кейсов.
//
// (a) состояние в END, посчитанное графом, как в Synapse;
// (b) то же явно: разрешение наборов состояний после prev-событий END.
// Сравнение строгое: лишний или недостающий ключ - ошибка. Раньше тест
// сравнивал только ключи из expected, и join_rule_evasion, offtopic_pl и
// настоящий topic_reset не проходили незаметно.
func TestSynapseDifferential(t *testing.T) {
	t.Parallel()

	for _, tc := range synapseCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			events, stateAt := buildGraph(tc.events, tc.edges)

			want := map[state.Key]string{}
			for _, nodeID := range tc.expected {
				ev := events[eventID(nodeID)]
				want[state.Key{Type: ev.Type, StateKey: *ev.StateKey}] = ev.ID
			}

			start := stateAt["START"]

			filter := func(end map[state.Key]string) map[state.Key]string {
				out := map[state.Key]string{}

				for k, v := range end {
					if _, ok := want[k]; ok || start[k] != v {
						out[k] = v
					}
				}

				return out
			}

			if got := filter(stateAt["END"]); fmtState(got) != fmtState(want) {
				t.Errorf("[state@END] got %s, ожидали %s (эталон Synapse)", fmtState(got), fmtState(want))
			}

			end := events[eventID("END")]
			sets := make([]state.StateMap, 0, len(end.Prev))

			for _, prev := range end.Prev {
				sets = append(sets, stateAt[shortID(prev)])
			}

			resolved, err := state.New().Resolve(sets, events)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			if got := filter(resolved); fmtState(got) != fmtState(want) {
				t.Errorf("[Resolve(prev END)] got %s, ожидали %s (эталон Synapse)", fmtState(got), fmtState(want))
			}
		})
	}
}

// shortID - node id из event_id для сообщений.
func shortID(id string) string { return strings.Split(strings.TrimPrefix(id, "$"), ":")[0] }
