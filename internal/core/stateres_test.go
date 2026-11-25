package core_test

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/state"
)

func strPtr(s string) *string { return &s }

// memRoomState - состояние одной комнаты в памяти: события, состояние после
// события, forward extremities и room_state (core.RoomStateTx).
type memRoomState struct {
	version     string
	events      map[string]state.Event
	after       map[string]state.StateMap
	rejected    map[string]bool
	extremities []string
	current     state.StateMap
	written     []core.StateRow
	err         error
}

func (m *memRoomState) WithRoomState(_ context.Context, _ string, fn func(core.RoomStateTx) error) error {
	if m.err != nil {
		return m.err
	}

	return fn(m)
}

func (m *memRoomState) RoomVersion(context.Context) (string, error) { return m.version, nil }

func (m *memRoomState) CurrentState(context.Context) (state.StateMap, error) {
	out := state.StateMap{}
	for k, v := range m.current {
		out[k] = v
	}

	return out, nil
}

func (m *memRoomState) ForwardExtremities(context.Context, []string) ([]string, error) {
	return append([]string(nil), m.extremities...), nil
}

func (m *memRoomState) SetForwardExtremities(_ context.Context, ids []string) error {
	m.extremities = append([]string(nil), ids...)

	return nil
}

func (m *memRoomState) StateAfter(_ context.Context, ids []string) (map[string]state.StateMap, error) {
	out := map[string]state.StateMap{}

	for _, id := range ids {
		if st, ok := m.after[id]; ok {
			out[id] = st
		}
	}

	return out, nil
}

func (m *memRoomState) Events(_ context.Context, ids []string) (map[string]state.Event, error) {
	out := map[string]state.Event{}

	for _, id := range ids {
		if ev, ok := m.events[id]; ok {
			ev.Rejected = m.rejected[id]
			out[id] = ev
		}
	}

	return out, nil
}

func (m *memRoomState) AuthChains(_ context.Context, ids []string) (map[string]state.Event, error) {
	out := map[string]state.Event{}
	stack := append([]string(nil), ids...)

	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if _, seen := out[id]; seen {
			continue
		}

		ev, ok := m.events[id]
		if !ok {
			continue
		}

		ev.Rejected = m.rejected[id]
		out[id] = ev
		stack = append(stack, ev.Auth...)
	}

	return out, nil
}

func (m *memRoomState) SaveStateAfter(_ context.Context, id string, after state.StateMap, _ string, rejected bool) error {
	m.after[id] = after
	m.rejected[id] = rejected

	return nil
}

func (m *memRoomState) ApplyCurrentState(_ context.Context, st state.StateMap, _ string) ([]core.StateRow, error) {
	var changed []core.StateRow

	for k, v := range st {
		if m.current[k] != v {
			changed = append(changed, core.StateRow{Type: k.Type, StateKey: k.StateKey, EventID: v})
		}
	}

	for k := range m.current {
		if _, ok := st[k]; !ok {
			changed = append(changed, core.StateRow{Type: k.Type, StateKey: k.StateKey})
		}
	}

	m.current = st
	m.written = append(m.written, changed...)

	return changed, nil
}

const (
	sAlice   = "@alice:hs1"
	sBob     = "@bob:hs2"
	sMallory = "@mallory:evil"
)

// roomEvents - create, alice, power_levels и join_rules.
func roomEvents(joinRule string) []state.Event {
	return []state.Event{
		{
			ID: "$create", Type: "m.room.create", StateKey: strPtr(""), Sender: sAlice,
			Content: []byte(`{"creator":"` + sAlice + `","room_version":"10"}`), Depth: 1, OriginTS: 1,
		},
		{
			ID: "$alice", Type: "m.room.member", StateKey: strPtr(sAlice), Sender: sAlice,
			Content: []byte(`{"membership":"join"}`), Auth: []string{"$create"}, Prev: []string{"$create"}, Depth: 2, OriginTS: 2,
		},
		{
			ID: "$pl", Type: "m.room.power_levels", StateKey: strPtr(""), Sender: sAlice,
			Content: []byte(`{"users":{"` + sAlice + `":100,"` + sBob + `":50},"state_default":50,"ban":50,"kick":50}`),
			Auth:    []string{"$create", "$alice"}, Prev: []string{"$alice"}, Depth: 3, OriginTS: 3,
		},
		{
			ID: "$jr", Type: "m.room.join_rules", StateKey: strPtr(""), Sender: sAlice,
			Content: []byte(`{"join_rule":"` + joinRule + `"}`), Auth: []string{"$create", "$alice", "$pl"},
			Prev: []string{"$pl"}, Depth: 4, OriginTS: 4,
		},
	}
}

// newRoom - комната из roomEvents и extra, записанная линейно (как локальные
// события): состояние после каждого события, extremity - последнее.
func newRoom(joinRule string, extra ...state.Event) *memRoomState {
	room := &memRoomState{
		version: "10", events: map[string]state.Event{}, after: map[string]state.StateMap{},
		rejected: map[string]bool{}, current: state.StateMap{},
	}

	all := append(roomEvents(joinRule), extra...)

	for i := range all {
		ev := all[i]
		room.events[ev.ID] = ev
		room.current = state.StateAfter(room.current, ev)
		room.after[ev.ID] = room.current
		room.extremities = []string{ev.ID}
	}

	return room
}

// deliver записывает входящие события (как ApplyFederationEvents) и применяет состояние.
func deliver(t *testing.T, room *memRoomState, events ...state.Event) {
	t.Helper()

	for i := range events {
		room.events[events[i].ID] = events[i]
	}

	if err := core.NewStateResolutionService(room, nil).Materialize(context.Background(), "!room:hs1", events); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
}

func member(id, sender, target, membership string, depth, ts int64, prev []string, auth ...string) state.Event {
	return state.Event{
		ID: id, Type: "m.room.member", StateKey: strPtr(target), Sender: sender,
		Content: []byte(`{"membership":"` + membership + `"}`), Auth: auth, Prev: prev, Depth: depth, OriginTS: ts,
	}
}

var (
	keyBob   = state.Key{Type: "m.room.member", StateKey: sBob}
	keyTopic = state.Key{Type: "m.room.topic"}
)

// Новое state-событие: записано состояние после него, extremity сдвинута,
// room_state обновлен; повтор ничего не меняет.
func TestStateResolutionService(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	room := newRoom("public")

	var applied int

	svc := core.NewStateResolutionService(room, func(_ context.Context, _ string, changed, _ int) { applied = changed })

	if err := svc.Materialize(ctx, "!room:hs1", nil); err != nil || len(room.written) != 0 {
		t.Fatalf("без новых событий: записано %v, err=%v", room.written, err)
	}

	topic := state.Event{
		ID: "$topic", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{"topic":"t"}`), Auth: []string{"$create", "$pl", "$alice"},
		Prev: []string{"$jr"}, Depth: 5, OriginTS: 5,
	}
	room.events[topic.ID] = topic

	if err := svc.Materialize(ctx, "!room:hs1", []state.Event{topic}); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	if len(room.written) != 1 || room.written[0].EventID != "$topic" || applied != 1 {
		t.Fatalf("записано %v, applied=%d", room.written, applied)
	}

	if len(room.extremities) != 1 || room.extremities[0] != "$topic" || room.after["$topic"][keyTopic] != "$topic" {
		t.Fatalf("extremities %v, состояние после topic %v", room.extremities, room.after["$topic"])
	}

	applied = 0

	if err := svc.Materialize(ctx, "!room:hs1", []state.Event{topic}); err != nil || applied != 0 || len(room.written) != 1 {
		t.Fatalf("повтор: err=%v applied=%d записано %v", err, applied, room.written)
	}

	room.err = errors.New("db down")

	if err := svc.Materialize(ctx, "!room:hs1", []state.Event{topic}); !errors.Is(err, room.err) {
		t.Fatalf("сбой: %v", err)
	}
}

// Потомок topic с отстающими часами (prev = T1) побеждает предка: T1 перестает быть
// extremity, разрешения нет .
func TestClockSkewNewerStateWins(t *testing.T) {
	t.Parallel()

	t1 := state.Event{
		ID: "$t1", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{"topic":"old"}`), Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$jr"},
		Depth: 5, OriginTS: 1000,
	}
	room := newRoom("invite", t1)

	t2 := state.Event{
		ID: "$t2", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{"topic":"new"}`), Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$t1"},
		Depth: 6, OriginTS: 500,
	}
	deliver(t, room, t2)

	if got := room.current[keyTopic]; got != "$t2" {
		t.Fatalf("topic = %s, ожидали $t2", got)
	}
}

// Ban с отстающими часами - потомок join: побеждает ban.
func TestBanWithSkewedClock(t *testing.T) {
	t.Parallel()

	room := newRoom("public", member("$bobjoin", sBob, sBob, "join", 5, 1000, []string{"$jr"}, "$create", "$pl", "$jr"))
	deliver(t, room, member("$bobban", sAlice, sBob, "ban", 6, 500, []string{"$bobjoin"}, "$create", "$pl", "$alice", "$bobjoin"))

	if got := room.current[keyBob]; got != "$bobban" {
		t.Fatalf("member bob = %s, ожидали $bobban", got)
	}
}

// Забаненный шлет join из ветки до бана: событие проходит проверку по своему
// состоянию, но разрешение двух extremities оставляет ban.
func TestBannedRejoinFork(t *testing.T) {
	t.Parallel()

	room := newRoom("public",
		member("$bobjoin", sBob, sBob, "join", 5, 10, []string{"$jr"}, "$create", "$pl", "$jr"),
		member("$bobban", sAlice, sBob, "ban", 6, 20, []string{"$bobjoin"}, "$create", "$pl", "$alice", "$bobjoin"))

	// Форк от $bobjoin: leave и новый join bob, ban не виден.
	deliver(t, room,
		member("$bobleave", sBob, sBob, "leave", 6, 30, []string{"$bobjoin"}, "$create", "$pl", "$bobjoin"),
		member("$bobjoin2", sBob, sBob, "join", 7, 40, []string{"$bobleave"}, "$create", "$pl", "$jr", "$bobleave"))

	if room.rejected["$bobjoin2"] {
		t.Fatal("join в своей ветке отклонен: по состоянию до него бана нет")
	}

	if len(room.extremities) != 2 {
		t.Fatalf("extremities %v, ожидали две ветки", room.extremities)
	}

	if got := room.current[keyBob]; got != "$bobban" {
		t.Fatalf("member bob = %s, ожидали $bobban", got)
	}
}

// Join забаненного поверх бана, self-leave забаненного, invite от не-участника,
// kick админа модератором: событие не проходит auth по состоянию до него и в
// состояние и extremities не попадает.
func TestRejectedByStateBefore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		event state.Event
		key   state.Key
		want  string
	}{
		{"rejoin", member("$x", sBob, sBob, "join", 6, 20, []string{"$bobban"}, "$create", "$pl", "$jr"), keyBob, "$bobban"},
		{"self_leave", member("$x", sBob, sBob, "leave", 6, 20, []string{"$bobban"}, "$create", "$pl", "$bobban"), keyBob, "$bobban"},
		{"invite", member("$x", sMallory, sBob, "invite", 6, 20, []string{"$bobban"}, "$create", "$pl", "$bobban"), keyBob, "$bobban"},
		{
			"unconflicted", member("$x", sMallory, sMallory, "join", 6, 20, nil, "$otherCreate"),
			state.Key{Type: "m.room.member", StateKey: sMallory},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			room := newRoom("invite", member("$bobban", sAlice, sBob, "ban", 5, 10, []string{"$jr"}, "$create", "$pl", "$alice"))
			deliver(t, room, tc.event)

			if got := room.current[tc.key]; got != tc.want || !room.rejected["$x"] {
				t.Fatalf("%v = %q (rejected=%v), ожидали %q", tc.key, got, room.rejected["$x"], tc.want)
			}

			if len(room.extremities) != 1 || room.extremities[0] != "$bobban" {
				t.Fatalf("extremities %v, ожидали [$bobban]", room.extremities)
			}
		})
	}

	// Модератор (50) кикает админа (100).
	room := newRoom("invite", member("$bobjoin", sBob, sBob, "join", 5, 10, []string{"$jr"}, "$create", "$pl", "$jr"))
	deliver(t, room, member("$kick", sBob, sAlice, "leave", 6, 20, []string{"$bobjoin"}, "$create", "$pl", "$bobjoin", "$alice"))

	if got := room.current[state.Key{Type: "m.room.member", StateKey: sAlice}]; got != "$alice" {
		t.Fatalf("member alice = %s, ожидали $alice", got)
	}
}

// Две ветки сходятся событием с двумя prev: состояние до него - разрешение
// наборов, extremity одна (topic с большей mainline-глубиной побеждает).
func TestMergeOfTwoPrevs(t *testing.T) {
	t.Parallel()

	room := newRoom("public")
	topicA := state.Event{
		ID: "$ta", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{}`), Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$jr"}, Depth: 5, OriginTS: 50,
	}
	topicB := state.Event{
		ID: "$tb", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{}`), Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$jr"}, Depth: 5, OriginTS: 60,
	}
	msg := state.Event{
		ID: "$m", Type: "m.room.message", Sender: sAlice, Content: []byte(`{}`),
		Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$ta", "$tb"}, Depth: 6, OriginTS: 70,
	}

	deliver(t, room, topicA, topicB)

	exts := append([]string(nil), room.extremities...)
	sort.Strings(exts)

	if len(exts) != 2 || room.current[keyTopic] != "$tb" {
		t.Fatalf("extremities %v, topic %s; ожидали две ветки и $tb (больший ts при равной глубине)", exts, room.current[keyTopic])
	}

	deliver(t, room, msg)

	if len(room.extremities) != 1 || room.extremities[0] != "$m" || room.after["$m"][keyTopic] != "$tb" {
		t.Fatalf("после слияния extremities %v, состояние %v", room.extremities, room.after["$m"])
	}
}

// fakeEvents - события по id для redacted_because.
type fakeEvents struct {
	events []core.Event
	err    error
}

func (f fakeEvents) EventsByIDs(context.Context, []string) ([]core.Event, error) {
	return f.events, f.err
}

// События удаления по id; сбой - пустая map и ошибка.
func TestRedactionLookup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	byID, err := core.NewRedactionLookup(fakeEvents{events: []core.Event{{EventID: "$r1"}, {EventID: "$r2"}}}).
		ByID(ctx, []string{"$r1", "$r2", "$r3"})
	if err != nil || len(byID) != 2 || byID["$r2"].EventID != "$r2" {
		t.Fatalf("ByID: %v %v", byID, err)
	}

	failure := errors.New("db down")

	byID, err = core.NewRedactionLookup(fakeEvents{err: failure}).ByID(ctx, []string{"$r1"})
	if !errors.Is(err, failure) || byID == nil || len(byID) != 0 {
		t.Fatalf("сбой: %v %v", byID, err)
	}
}

// Блокировка комнаты до join: хранилище с блокировками и без.
func TestRoomServiceRoomBlocked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rooms, store := newRoomService(t)
	roomID := createRoom(t, rooms)

	if blocked, err := rooms.RoomBlocked(ctx, roomID); err != nil || blocked {
		t.Fatalf("без блокировки: %v %v", blocked, err)
	}

	store.SetBlocked(roomID, true)

	if blocked, err := rooms.RoomBlocked(ctx, roomID); err != nil || !blocked {
		t.Fatalf("с блокировкой: %v %v", blocked, err)
	}

	plain := core.NewRoomService(struct{ core.RoomStore }{store}, "test.local")

	if blocked, err := plain.RoomBlocked(ctx, roomID); err != nil || blocked {
		t.Fatalf("хранилище без блокировок: %v %v", blocked, err)
	}
}
