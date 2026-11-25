// Derived from Synapse synapse/state/v2.py at commit
// 40f63e0fb1813161c04a0e2c930ec611466e81f7.
// Copyright (C) 2023 New Vector, Ltd.
// Modified and ported to Go by SYNGO contributors on 2026-11-27.
//
// This file is licensed under the GNU Affero General Public License, version 3
// or (at your option) any later version. See LICENSE and NOTICE.
// The upstream file was originally licensed under Apache License 2.0 and
// includes modifications made by New Vector Limited.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package state реализует state resolution v2 и v2.1 по спецификации Matrix.
//
// Порт synapse/state/v2.py (pin 40f63e0): вход - наборы состояний
// (состояние после каждого forward extremity или каждого prev_event), а не плоский
// список событий. Ключ бесконфликтный, только если он есть во всех наборах с одним
// и тем же событием; full conflicted set - конфликтующие события плюс auth
// difference (события, входящие в полные auth chains не всех наборов), для v2.1 -
// еще conflicted subgraph. Power-события сортируются reverse topological power
// ordering (куча с предвычисленным ключом), остальные - mainline ordering по
// цепочке m.room.power_levels разрешенного power-события; оба прохода - iterative
// auth checks. Поверх результата кладется бесконфликтное состояние.
package state

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

// Event - событие, участвующее в разрешении (state-событие или его auth-предок).
type Event struct {
	ID       string
	RoomID   string
	Type     string
	StateKey *string
	Sender   string
	Content  []byte
	Prev     []string
	Auth     []string
	Depth    int64
	OriginTS int64
	// Rejected - событие ранее отклонено: в состояние не допускается и не
	// используется как auth-событие (как rejected_reason в Synapse).
	Rejected bool
}

// Key - ключ состояния.
type Key struct {
	Type     string
	StateKey string
}

// StateMap - набор состояния: ключ -> event_id.
type StateMap = map[Key]string

// Result - итоговое состояние: ключ -> событие-победитель.
type Result map[Key]string

// Algorithm - вариант алгоритма разрешения.
type Algorithm int

const (
	// V2 - state resolution v2 (комнаты v2-v11): базовое состояние - бесконфликтное.
	V2 Algorithm = iota
	// V21 - state resolution v2.1 (MSC4297, комнаты v12): итеративные проверки
	// начинаются с пустого состояния, в full conflicted set входит conflicted subgraph.
	V21
)

// AlgorithmFor - алгоритм для версии комнаты: v2.1 - там же, где правила
// создателей v12 (v12 и org.matrix.hydra.11), как StateResolutionVersions в Synapse.
func AlgorithmFor(roomVersion string) Algorithm {
	if auth.CreatorsPrivileged(roomVersion) {
		return V21
	}

	return V2
}

// Stats - счетчики разрешения (информационные, не влияют на результат).
type Stats struct {
	// Conflicted - размер full conflicted set (conflicted + auth difference).
	Conflicted int
	// Changed - сколько ключей изменилось относительно WithCurrent.
	Changed int
}

// Resolver - разрешение состояния.
type Resolver struct {
	Stats Stats

	roomID    string
	algorithm Algorithm
	current   map[Key]string
}

// New создает resolver (алгоритм v2).
func New() *Resolver { return &Resolver{} }

// WithRoom задает комнату: метрики по комнатам и проверка, что все события
// разрешения из этой комнаты.
func (r *Resolver) WithRoom(roomID string) *Resolver {
	r.roomID = roomID

	return r
}

// WithAlgorithm задает алгоритм (V2 или V21).
func (r *Resolver) WithAlgorithm(algorithm Algorithm) *Resolver {
	r.algorithm = algorithm

	return r
}

// WithCurrent задает материализованное состояние для счетчика Stats.Changed.
func (r *Resolver) WithCurrent(current map[Key]string) *Resolver {
	r.current = current

	return r
}

// Resolve разрешает наборы состояний.
//
// events - события наборов и их полные auth chains (что найдено; отсутствующие
// события в auth difference не попадают, как в Synapse). Порядок наборов и
// событий на результат не влияет.
func (r *Resolver) Resolve(sets []StateMap, events map[string]Event) (Result, error) {
	started := time.Now()
	r.Stats = Stats{}

	defer func() {
		DefaultMetrics.Observe(r.roomID, time.Since(started), r.Stats.Conflicted, r.Stats.Changed)
	}()

	unconflicted, conflicted := separate(sets)
	if len(conflicted) == 0 {
		return r.finish(Result(unconflicted)), nil
	}

	conflictedIDs := map[string]struct{}{}

	for _, ids := range conflicted {
		for _, id := range ids {
			conflictedIDs[id] = struct{}{}
		}
	}

	full := authDifference(sets, events)
	for id := range conflictedIDs {
		full[id] = struct{}{}
	}

	if r.algorithm == V21 {
		for id := range conflictedSubgraph(conflictedIDs, events) {
			full[id] = struct{}{}
		}
	}

	// В разрешении участвуют только известные события одной комнаты.
	for id := range full {
		ev, ok := events[id]
		if !ok {
			delete(full, id)

			continue
		}

		if r.roomID != "" && ev.RoomID != "" && ev.RoomID != r.roomID {
			return nil, fmt.Errorf("state: event %s is in room %s, resolving %s", id, ev.RoomID, r.roomID)
		}
	}

	r.Stats.Conflicted = len(full)

	base := StateMap{}
	if r.algorithm != V21 {
		for key, id := range unconflicted {
			base[key] = id
		}
	}

	res := newResolution(events)
	resolved := res.resolveConflicted(full, base)

	for key, id := range unconflicted {
		resolved[key] = id
	}

	return r.finish(Result(resolved)), nil
}

// finish считает статистику и возвращает результат.
func (r *Resolver) finish(res Result) Result {
	if r.current != nil {
		for key, id := range res {
			if r.current[key] != id {
				r.Stats.Changed++
			}
		}
	}

	return res
}

// separate делит ключи на бесконфликтные и конфликтные. В отличие от исходного
// алгоритма ключ конфликтный и тогда, когда его нет хотя бы в одном наборе
// (separate-шаг в synapse/state/v2.py).
func separate(sets []StateMap) (unconflicted StateMap, conflicted map[Key][]string) {
	unconflicted = StateMap{}
	conflicted = map[Key][]string{}

	keys := map[Key]struct{}{}

	for _, set := range sets {
		for key := range set {
			keys[key] = struct{}{}
		}
	}

	for key := range keys {
		ids := map[string]struct{}{}
		missing := false

		for _, set := range sets {
			id, ok := set[key]
			if !ok {
				missing = true

				continue
			}

			ids[id] = struct{}{}
		}

		if !missing && len(ids) == 1 {
			for id := range ids {
				unconflicted[key] = id
			}

			continue
		}

		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}

		sort.Strings(list)
		conflicted[key] = list
	}

	return unconflicted, conflicted
}

// authDifference - события, которые есть в полных auth chains одних наборов, но
// не всех (union - intersection). Auth chain набора включает сами события набора.
func authDifference(sets []StateMap, events map[string]Event) map[string]struct{} {
	out := map[string]struct{}{}
	if len(sets) == 0 {
		return out
	}

	counts := map[string]int{}

	for _, set := range sets {
		roots := make([]string, 0, len(set))
		for _, id := range set {
			roots = append(roots, id)
		}

		for id := range authChainOf(roots, events) {
			counts[id]++
		}
	}

	for id, n := range counts {
		if n < len(sets) {
			out[id] = struct{}{}
		}
	}

	return out
}

// authChainOf - полный auth chain событий roots, включая их самих. Неизвестные
// события входят в цепочку, но их предки - нет.
func authChainOf(roots []string, events map[string]Event) map[string]struct{} {
	out := make(map[string]struct{}, len(roots))
	stack := append([]string(nil), roots...)

	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if _, seen := out[id]; seen {
			continue
		}

		out[id] = struct{}{}

		ev, ok := events[id]
		if !ok {
			continue
		}

		for _, aid := range authIDs(ev) {
			if _, seen := out[aid]; !seen {
				stack = append(stack, aid)
			}
		}
	}

	return out
}

// conflictedSubgraph - события auth DAG между конфликтующими событиями: предки
// какого-либо конфликтующего события, у которых в auth chain тоже есть
// конфликтующее событие (MSC4297), включая сами конфликтующие события.
func conflictedSubgraph(conflicted map[string]struct{}, events map[string]Event) map[string]struct{} {
	roots := make([]string, 0, len(conflicted))
	for id := range conflicted {
		roots = append(roots, id)
	}

	backward := authChainOf(roots, events)

	// reaches[id] - есть ли конфликтующее событие в auth chain id (включая сам id).
	reaches := make(map[string]bool, len(backward))

	var visit func(id string) bool

	visit = func(id string) bool {
		if v, ok := reaches[id]; ok {
			return v
		}

		// Защита от циклов: пока обходим, считаем «не достигает».
		reaches[id] = false

		result := false
		if _, ok := conflicted[id]; ok {
			result = true
		}

		if ev, ok := events[id]; ok {
			for _, aid := range authIDs(ev) {
				if _, inBackward := backward[aid]; inBackward && visit(aid) {
					result = true
				}
			}
		}

		reaches[id] = result

		return result
	}

	out := map[string]struct{}{}

	for id := range backward {
		if visit(id) {
			out[id] = struct{}{}
		}
	}

	return out
}

// resolution - кэши одного разрешения (разобранные power_levels, mainline-глубины).
type resolution struct {
	events map[string]Event
	// levels - разобранный content m.room.power_levels по event_id.
	levels map[string]*plContent
}

func newResolution(events map[string]Event) *resolution {
	return &resolution{events: events, levels: map[string]*plContent{}}
}

// resolveConflicted применяет full conflicted set к базовому состоянию
// (_resolve_conflicted_set в Synapse).
func (res *resolution) resolveConflicted(full map[string]struct{}, base StateMap) StateMap {
	powerIDs := make([]string, 0, len(full))

	for id := range full {
		if isPowerEvent(res.events[id]) {
			powerIDs = append(powerIDs, id)
		}
	}

	sortedPower := res.reverseTopologicalPowerSort(powerIDs, full)
	resolved := res.iterativeAuthChecks(sortedPower, base)

	inPower := make(map[string]struct{}, len(sortedPower))
	for _, id := range sortedPower {
		inPower[id] = struct{}{}
	}

	leftover := make([]string, 0, len(full)-len(sortedPower))

	for id := range full {
		if _, ok := inPower[id]; !ok {
			leftover = append(leftover, id)
		}
	}

	leftover = res.mainlineSort(leftover, resolved[Key{Type: typePowerLevels}])

	return res.iterativeAuthChecks(leftover, resolved)
}

const (
	typeCreate      = "m.room.create"
	typePowerLevels = "m.room.power_levels"
	typeJoinRules   = "m.room.join_rules"
	typeMember      = "m.room.member"
	typeTPInvite    = "m.room.third_party_invite"
)

// isPowerEvent - power-событие v2: create, power_levels, join_rules и kick/ban
// (leave или ban чужого пользователя). Собственный leave и knock - не power.
func isPowerEvent(ev Event) bool {
	if ev.StateKey == nil {
		return false
	}

	switch ev.Type {
	case typeCreate, typePowerLevels, typeJoinRules:
		return *ev.StateKey == ""
	case typeMember:
		switch membershipOf(ev.Content) {
		case "leave", "ban":
			return ev.Sender != *ev.StateKey
		}
	}

	return false
}

// orderKey - ключ reverse topological power ordering: power ↓, ts ↑, event_id ↑.
type orderKey struct {
	negPower int
	ts       int64
	id       string
}

func (a orderKey) less(b orderKey) bool {
	if a.negPower != b.negPower {
		return a.negPower < b.negPower
	}

	if a.ts != b.ts {
		return a.ts < b.ts
	}

	return a.id < b.id
}

// keyHeap - min-куча по orderKey.
type keyHeap []orderKey

func (h keyHeap) Len() int           { return len(h) }
func (h keyHeap) Less(i, j int) bool { return h[i].less(h[j]) }
func (h keyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *keyHeap) Push(x any) {
	key, ok := x.(orderKey)
	if ok {
		*h = append(*h, key)
	}
}

func (h *keyHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]

	return last
}

// reverseTopologicalPowerSort сортирует power-события вместе с их auth-предками
// из full conflicted set: предки раньше потомков, при равенстве - больший power
// отправителя, меньший origin_server_ts, меньший event_id
// (_reverse_topological_power_sort + lexicographical_topological_sort).
func (res *resolution) reverseTopologicalPowerSort(ids []string, full map[string]struct{}) []string {
	graph := map[string]map[string]struct{}{}

	for _, start := range ids {
		stack := []string{start}

		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			edges, ok := graph[id]
			if !ok {
				edges = map[string]struct{}{}
				graph[id] = edges
			}

			for _, aid := range authIDs(res.events[id]) {
				if _, inFull := full[aid]; !inFull {
					continue
				}

				if _, seen := graph[aid]; !seen {
					stack = append(stack, aid)
				}

				edges[aid] = struct{}{}
			}
		}
	}

	keys := make(map[string]orderKey, len(graph))
	outdegree := make(map[string]int, len(graph))
	reverse := make(map[string][]string, len(graph))
	ready := make(keyHeap, 0, len(graph))

	for id, edges := range graph {
		key := orderKey{negPower: -res.senderPowerLevel(res.events[id]), ts: res.events[id].OriginTS, id: id}
		keys[id] = key
		outdegree[id] = len(edges)

		for aid := range edges {
			reverse[aid] = append(reverse[aid], id)
		}

		if len(edges) == 0 {
			ready = append(ready, key)
		}
	}

	heap.Init(&ready)

	out := make([]string, 0, len(graph))

	for ready.Len() > 0 {
		key, ok := heap.Pop(&ready).(orderKey)
		if !ok {
			break
		}

		out = append(out, key.id)

		for _, parent := range reverse[key.id] {
			outdegree[parent]--
			if outdegree[parent] == 0 {
				heap.Push(&ready, keys[parent])
			}
		}
	}

	return out
}

// iterativeAuthChecks последовательно проверяет события по auth-событиям из их
// auth_events, перекрытым разрешенным состоянием по auth-типам события, и кладет
// прошедшие в состояние (_iterative_auth_checks).
func (res *resolution) iterativeAuthChecks(ids []string, base StateMap) StateMap {
	resolved := make(StateMap, len(base)+len(ids))
	for key, id := range base {
		resolved[key] = id
	}

	for _, id := range ids {
		ev := res.events[id]

		authByKey := map[Key]Event{}

		for _, aid := range authIDs(ev) {
			aev, ok := res.events[aid]
			if !ok || aev.Rejected || aev.StateKey == nil {
				continue
			}

			authByKey[Key{Type: aev.Type, StateKey: *aev.StateKey}] = aev
		}

		for key := range AuthTypesForEvent(ev) {
			rid, ok := resolved[key]
			if !ok {
				continue
			}

			if rev, ok := res.events[rid]; ok && !rev.Rejected {
				authByKey[key] = rev
			}
		}

		if ev.Rejected || ev.StateKey == nil {
			continue
		}

		if checkStateDependent(ev, authByKey) {
			resolved[Key{Type: ev.Type, StateKey: *ev.StateKey}] = id
		}
	}

	return resolved
}

// checkStateDependent - зависящие от состояния auth rules (check_state_dependent_auth_rules):
// create проверяется независимыми от состояния правилами при приеме и здесь проходит.
func checkStateDependent(ev Event, authByKey map[Key]Event) bool {
	if ev.Type == typeCreate && ev.StateKey != nil && *ev.StateKey == "" {
		return true
	}

	list := make([]auth.Event, 0, len(authByKey))
	for key := range authByKey {
		list = append(list, toAuthEvent(authByKey[key]))
	}

	st, err := auth.BuildState(list)
	if err != nil {
		return false
	}

	return auth.Check(toAuthEvent(ev), st) == nil
}

// mainlineSort сортирует события по mainline разрешенного power_levels: глубина
// ближайшего power_levels из цепочки auth_events, затем origin_server_ts, event_id.
func (res *resolution) mainlineSort(ids []string, resolvedPL string) []string {
	if len(ids) == 0 {
		return ids
	}

	mainline := make([]string, 0, 8)
	seen := map[string]struct{}{}

	for pl := resolvedPL; pl != ""; {
		if _, loop := seen[pl]; loop {
			break
		}

		seen[pl] = struct{}{}
		mainline = append(mainline, pl)

		plEv, ok := res.events[pl]
		if !ok {
			break
		}

		pl = res.powerLevelsAuth(plEv)
	}

	depthOf := make(map[string]int, len(mainline)+len(ids))
	for i, id := range mainline {
		depthOf[id] = len(mainline) - i
	}

	type sortKey struct {
		depth int
		ts    int64
	}

	keys := make(map[string]sortKey, len(ids))
	for _, id := range ids {
		keys[id] = sortKey{depth: res.mainlineDepth(id, depthOf), ts: res.events[id].OriginTS}
	}

	sort.Slice(ids, func(i, j int) bool {
		ki, kj := keys[ids[i]], keys[ids[j]]

		if ki.depth != kj.depth {
			return ki.depth < kj.depth
		}

		if ki.ts != kj.ts {
			return ki.ts < kj.ts
		}

		return ids[i] < ids[j]
	})

	return ids
}

// mainlineDepth - глубина события в mainline: идем по первому power_levels из
// auth_events до события mainline (0 - не дошли). Найденные глубины кэшируются
// в depthOf для всех пройденных событий цепочки.
func (res *resolution) mainlineDepth(id string, depthOf map[string]int) int {
	path := make([]string, 0, 8)
	depth := 0
	cur := id

	for cur != "" {
		if d, ok := depthOf[cur]; ok {
			depth = d

			break
		}

		path = append(path, cur)

		ev, ok := res.events[cur]
		if !ok || len(path) > len(res.events) {
			break
		}

		cur = res.powerLevelsAuth(ev)
	}

	for _, p := range path {
		depthOf[p] = depth
	}

	return depth
}

// powerLevelsAuth - первый m.room.power_levels среди известных auth_events события.
func (res *resolution) powerLevelsAuth(ev Event) string {
	for _, aid := range authIDs(ev) {
		if aev, ok := res.events[aid]; ok && aev.Type == typePowerLevels && aev.StateKey != nil && *aev.StateKey == "" {
			return aid
		}
	}

	return ""
}

// plContent - нужные для сортировки поля m.room.power_levels.
type plContent struct {
	users        map[string]json.RawMessage
	usersDefault json.RawMessage
}

func (res *resolution) powerLevels(ev Event) *plContent {
	if pl, ok := res.levels[ev.ID]; ok {
		return pl
	}

	var parsed struct {
		Users        map[string]json.RawMessage `json:"users"`
		UsersDefault json.RawMessage            `json:"users_default"`
	}

	_ = json.Unmarshal(ev.Content, &parsed)

	pl := &plContent{users: parsed.Users, usersDefault: parsed.UsersDefault}
	res.levels[ev.ID] = pl

	return pl
}

// senderPowerLevel - power level отправителя по auth_events события
// (_get_power_level_for_sender): создатели v12 - бесконечный уровень; без
// power_levels создатель - 100, остальные - 0.
func (res *resolution) senderPowerLevel(ev Event) int {
	var pl, create *Event

	for _, aid := range authIDs(ev) {
		aev, ok := res.events[aid]
		if !ok || aev.StateKey == nil || *aev.StateKey != "" {
			continue
		}

		switch aev.Type {
		case typePowerLevels:
			pl = &aev
		case typeCreate:
			create = &aev
		}
	}

	if create != nil {
		authCreate := toAuthEvent(*create)
		if _, ok := auth.Creators(auth.State{Create: &authCreate})[ev.Sender]; ok {
			return math.MaxInt
		}
	}

	if pl == nil {
		if create != nil && creatorOf(*create) == ev.Sender {
			return 100
		}

		return 0
	}

	content := res.powerLevels(*pl)

	if raw, ok := content.users[ev.Sender]; ok {
		return levelValue(raw)
	}

	return levelValue(content.usersDefault)
}

// levelValue - целое из значения power level (число или строка с числом, как
// int() в Synapse); иначе 0.
func levelValue(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}

	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return int(number)
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if n, err := strconv.Atoi(text); err == nil {
			return n
		}
	}

	return 0
}

// creatorOf - создатель комнаты: creator из content m.room.create (до v11), иначе
// отправитель create (v11 убрал поле из content).
func creatorOf(create Event) string {
	var parsed struct {
		Creator string `json:"creator"`
	}

	if err := json.Unmarshal(create.Content, &parsed); err == nil && parsed.Creator != "" {
		return parsed.Creator
	}

	return create.Sender
}

// AuthTypesForEvent - ключи состояния, нужные для авторизации события
// (auth_types_for_event в Synapse): power_levels, членство отправителя, create
// (кроме комнат v12), для членства - цель, join_rules для join/invite/knock,
// third_party_invite по токену и авторизующий пользователь restricted join.
func AuthTypesForEvent(ev Event) map[Key]struct{} {
	if ev.Type == typeCreate {
		return map[Key]struct{}{}
	}

	out := map[Key]struct{}{
		{Type: typePowerLevels}:                 {},
		{Type: typeMember, StateKey: ev.Sender}: {},
	}

	if !auth.IsCreateHashRoomID(ev.RoomID) {
		out[Key{Type: typeCreate}] = struct{}{}
	}

	if ev.Type != typeMember || ev.StateKey == nil {
		return out
	}

	var content struct {
		Membership       string `json:"membership"`
		ThirdPartyInvite *struct {
			Signed struct {
				Token string `json:"token"`
			} `json:"signed"`
		} `json:"third_party_invite"`
		Authorising string `json:"join_authorised_via_users_server"`
	}

	_ = json.Unmarshal(ev.Content, &content)

	switch content.Membership {
	case "join", "invite", "knock":
		out[Key{Type: typeJoinRules}] = struct{}{}
	}

	out[Key{Type: typeMember, StateKey: *ev.StateKey}] = struct{}{}

	if content.Membership == "invite" && content.ThirdPartyInvite != nil {
		out[Key{Type: typeTPInvite, StateKey: content.ThirdPartyInvite.Signed.Token}] = struct{}{}
	}

	if content.Membership == "join" && content.Authorising != "" {
		out[Key{Type: typeMember, StateKey: content.Authorising}] = struct{}{}
	}

	return out
}

// authIDs - auth_events события; в комнатах v12 create в auth_events не пишется,
// но входит в них неявно (event_id create = room_id с "$" вместо "!", как
// auth_event_ids() в Synapse для MSC4291).
func authIDs(ev Event) []string {
	if ev.Type == typeCreate || !auth.IsCreateHashRoomID(ev.RoomID) {
		return ev.Auth
	}

	createID := "$" + ev.RoomID[1:]
	for _, id := range ev.Auth {
		if id == createID {
			return ev.Auth
		}
	}

	return append([]string{createID}, ev.Auth...)
}

// membershipOf - membership из content события member.
func membershipOf(content []byte) string {
	var parsed struct {
		Membership string `json:"membership"`
	}

	_ = json.Unmarshal(content, &parsed)

	return parsed.Membership
}

// resolveRoomID - room_id для auth-проверки, если у события его нет (тесты).
const resolveRoomID = "!resolve:resolve"

// toAuthEvent переводит событие в тип auth.
func toAuthEvent(ev Event) auth.Event {
	roomID := ev.RoomID
	if roomID == "" {
		roomID = resolveRoomID
	}

	return auth.Event{
		EventID: ev.ID, RoomID: roomID, Type: ev.Type, Sender: ev.Sender,
		StateKey: ev.StateKey, Content: ev.Content, Prev: ev.Prev, Depth: ev.Depth,
	}
}

// StateAfter - состояние после события: состояние до него плюс само событие, если
// это state-событие (не отклоненное).
func StateAfter(before StateMap, ev Event) StateMap {
	after := make(StateMap, len(before)+1)
	for key, id := range before {
		after[key] = id
	}

	if ev.StateKey != nil && !ev.Rejected {
		after[Key{Type: ev.Type, StateKey: *ev.StateKey}] = ev.ID
	}

	return after
}

// Authorized проверяет событие по состоянию до него (state before): auth-события -
// ключи AuthTypesForEvent из before (проверка «по состоянию на prev_events» при
// приеме PDU). Для комнат v12 create входит в auth-события неявно.
func Authorized(ev Event, before StateMap, events map[string]Event) bool {
	authByKey := map[Key]Event{}

	for key := range AuthTypesForEvent(ev) {
		id, ok := before[key]
		if !ok {
			continue
		}

		if aev, ok := events[id]; ok && !aev.Rejected {
			authByKey[key] = aev
		}
	}

	if auth.IsCreateHashRoomID(ev.RoomID) {
		if create, ok := events["$"+ev.RoomID[1:]]; ok {
			authByKey[Key{Type: typeCreate}] = create
		}
	}

	return checkStateDependent(ev, authByKey)
}
