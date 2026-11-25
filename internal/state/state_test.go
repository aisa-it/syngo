package state_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/state"
)

func ptr(s string) *string { return &s }

var keyTopic = state.Key{Type: "m.room.topic", StateKey: ""}

const (
	rAlice   = "@alice:hs1"
	rBob     = "@bob:hs2"
	rMallory = "@mallory:evil"
)

// roomState - create, join alice, power_levels и join_rules.
func roomState(joinRule string) []state.Event {
	return []state.Event{
		{
			ID: "$create", Type: "m.room.create", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"creator":"` + rAlice + `","room_version":"10"}`), OriginTS: 1,
		},
		{
			ID: "$alice", Type: "m.room.member", StateKey: ptr(rAlice), Sender: rAlice,
			Content: []byte(`{"membership":"join"}`), Auth: []string{"$create"}, OriginTS: 2,
		},
		{
			ID: "$pl", Type: "m.room.power_levels", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"users":{"` + rAlice + `":100},"users_default":0,"state_default":50,"ban":50,"kick":50,"invite":0}`),
			Auth:    []string{"$create", "$alice"}, OriginTS: 3,
		},
		{
			ID: "$jr", Type: "m.room.join_rules", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"join_rule":"` + joinRule + `"}`), Auth: []string{"$create", "$alice", "$pl"}, OriginTS: 4,
		},
	}
}

func member(id, sender, target, membership string, ts int64, auth ...string) state.Event {
	return state.Event{
		ID: id, Type: "m.room.member", StateKey: ptr(target), Sender: sender,
		Content: []byte(`{"membership":"` + membership + `"}`), Auth: auth, OriginTS: ts,
	}
}

// graph - события комнаты по event_id.
type graph map[string]state.Event

func newGraph(events ...state.Event) graph {
	g := graph{}
	g.add(events...)

	return g
}

func (g graph) add(events ...state.Event) graph {
	for i := range events {
		g[events[i].ID] = events[i]
	}

	return g
}

// set - набор состояния из событий (позднее событие перекрывает ключ).
func (g graph) set(ids ...string) state.StateMap {
	out := state.StateMap{}

	for _, id := range ids {
		ev := g[id]
		out[state.Key{Type: ev.Type, StateKey: *ev.StateKey}] = id
	}

	return out
}

func ids(events []state.Event) []string {
	out := make([]string, 0, len(events))
	for i := range events {
		out = append(out, events[i].ID)
	}

	return out
}

func resolve(t *testing.T, g graph, sets ...state.StateMap) state.Result {
	t.Helper()

	res, err := state.New().Resolve(sets, g)
	if err != nil {
		t.Fatal(err)
	}

	return res
}

var keyBob = state.Key{Type: "m.room.member", StateKey: rBob}

// Забаненный шлет новый join из ветки до бана: join не побеждает ban.
func TestBannedRejoinPublicRoom(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...).add(
		member("$bobban", rAlice, rBob, "ban", 10, "$create", "$pl", "$alice"),
		member("$bobjoin2", rBob, rBob, "join", 20, "$create", "$pl", "$jr"),
	)

	res := resolve(t, g, g.set(append(ids(base), "$bobban")...), g.set(append(ids(base), "$bobjoin2")...))
	if got := res[keyBob]; got != "$bobban" {
		t.Errorf("member bob = %s, ожидали $bobban", got)
	}
}

// Ban - потомок join с отстающими часами: побеждает ban.
func TestBanWithSkewedClockPublicRoom(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...).add(
		member("$bobjoin", rBob, rBob, "join", 1000, "$create", "$pl", "$jr"),
		member("$bobban", rAlice, rBob, "ban", 500, "$create", "$pl", "$alice", "$bobjoin"),
	)

	res := resolve(t, g, g.set(append(ids(base), "$bobjoin")...), g.set(append(ids(base), "$bobban")...))
	if got := res[keyBob]; got != "$bobban" {
		t.Errorf("member bob = %s, ожидали $bobban", got)
	}
}

// Забаненный сам себе шлет leave: бан не снимается (правило 4.5.1).
func TestBannedSelfLeaveUnbans(t *testing.T) {
	t.Parallel()

	base := roomState("invite")
	g := newGraph(base...).add(
		member("$bobban", rAlice, rBob, "ban", 10, "$create", "$pl", "$alice"),
		member("$bobleave", rBob, rBob, "leave", 20, "$create", "$pl", "$bobban"),
	)

	res := resolve(t, g, g.set(append(ids(base), "$bobban")...), g.set(append(ids(base), "$bobleave")...))
	if got := res[keyBob]; got != "$bobban" {
		t.Errorf("member bob = %s, ожидали $bobban", got)
	}
}

// Invite от не-участника поверх ban не принимается.
func TestInviteFromNonMemberOverBan(t *testing.T) {
	t.Parallel()

	base := roomState("invite")
	g := newGraph(base...).add(
		member("$bobban", rAlice, rBob, "ban", 10, "$create", "$pl", "$alice"),
		member("$inv", rMallory, rBob, "invite", 20, "$create", "$pl", "$bobban"),
	)

	res := resolve(t, g, g.set(append(ids(base), "$bobban")...), g.set(append(ids(base), "$inv")...))
	if got := res[keyBob]; got != "$bobban" {
		t.Errorf("member bob = %s, ожидали $bobban", got)
	}
}

// Ключ, которого нет в одном из наборов, - конфликтный и проходит auth: join без
// приглашения в invite-only комнату в состояние не попадает.
func TestUnconflictedNotAuthChecked(t *testing.T) {
	t.Parallel()

	base := roomState("invite")
	g := newGraph(base...).add(member("$evil", rMallory, rMallory, "join", 20, "$otherRoomCreate", "$otherRoomMember"))

	res := resolve(t, g, g.set(ids(base)...), g.set(append(ids(base), "$evil")...))
	if got, ok := res[state.Key{Type: "m.room.member", StateKey: rMallory}]; ok {
		t.Errorf("join без invite попал в состояние: %s", got)
	}
}

// Модератор (50) кикает админа (100): kick отклоняется (правило 4.5.2.3).
func TestKickedByLowerPowerLevel(t *testing.T) {
	t.Parallel()

	base := roomState("invite")
	base[2].Content = []byte(`{"users":{"` + rAlice + `":100,"` + rBob + `":50},"kick":50,"ban":50}`)
	g := newGraph(base...).add(
		member("$bobjoin", rBob, rBob, "join", 10, "$create", "$pl"),
		member("$kick", rBob, rAlice, "leave", 20, "$create", "$pl", "$bobjoin", "$alice"),
	)

	res := resolve(t, g, g.set(append(ids(base), "$bobjoin")...), g.set(append(ids(base), "$bobjoin", "$kick")...))
	if got := res[state.Key{Type: "m.room.member", StateKey: rAlice}]; got != "$alice" {
		t.Errorf("member alice = %s, ожидали $alice", got)
	}
}

// TestMainlineOrdering - побеждает topic с большей mainline-глубиной, а не с
// большим origin_server_ts.
func TestMainlineOrdering(t *testing.T) {
	t.Parallel()

	base := roomState("invite")
	g := newGraph(base...).add(
		state.Event{
			ID: "$pl2", Type: "m.room.power_levels", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"users":{"` + rAlice + `":100}}`), Auth: []string{"$create", "$alice", "$pl"}, OriginTS: 5,
		},
		state.Event{
			ID: "$tnew", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"topic":"new"}`), Auth: []string{"$create", "$alice", "$pl2"}, OriginTS: 6,
		},
		state.Event{
			ID: "$told", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{"topic":"old"}`), Auth: []string{"$create", "$alice", "$pl"}, OriginTS: 100,
		},
	)

	res := resolve(t, g, g.set(append(ids(base), "$pl2", "$tnew")...), g.set(append(ids(base), "$pl2", "$told")...))
	if got := res[keyTopic]; got != "$tnew" {
		t.Fatalf("topic = %s, ожидали $tnew (mainline-глубина 2 против 1)", got)
	}
}

// TestDeterminism - перестановка наборов не меняет результат.
func TestDeterminism(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...)

	sets := make([]state.StateMap, 0, 20)

	for i := range 20 {
		id := fmt.Sprintf("$t%02d", i)
		g.add(state.Event{
			ID: id, Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{}`), Auth: []string{"$create", "$alice", "$pl"}, OriginTS: int64(100 - i%7),
		})

		joinID := fmt.Sprintf("$j%02d", i)
		g.add(member(joinID, rBob, rBob, "join", int64(50+i), "$create", "$pl", "$jr"))

		sets = append(sets, g.set(append(ids(base), id, joinID)...))
	}

	first := resolve(t, g, sets...)
	rnd := rand.New(rand.NewSource(7))

	for i := range 200 {
		rnd.Shuffle(len(sets), func(a, b int) { sets[a], sets[b] = sets[b], sets[a] })

		got := resolve(t, g, sets...)
		if fmt.Sprint(got) != fmt.Sprint(first) {
			t.Fatalf("перестановка %d: %v, ожидали %v", i, got, first)
		}
	}
}

// TestChangedStat - Stats.Changed считается относительно WithCurrent.
func TestChangedStat(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...).add(state.Event{
		ID: "$t", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
		Content: []byte(`{}`), Auth: []string{"$create", "$alice", "$pl"}, OriginTS: 5,
	})

	resolver := state.New().WithCurrent(g.set(ids(base)...))
	if _, err := resolver.Resolve([]state.StateMap{g.set(ids(base)...), g.set(append(ids(base), "$t")...)}, g); err != nil {
		t.Fatal(err)
	}

	if resolver.Stats.Changed != 1 || resolver.Stats.Conflicted != 1 {
		t.Fatalf("Stats = %+v, ожидали Changed=1 Conflicted=1", resolver.Stats)
	}
}

// TestRoomMismatch - событие другой комнаты в full conflicted set - ошибка, как в Synapse.
func TestRoomMismatch(t *testing.T) {
	t.Parallel()

	g := newGraph(state.Event{ID: "$a", RoomID: "!a:hs", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice},
		state.Event{ID: "$b", RoomID: "!b:hs", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice})

	if _, err := state.New().WithRoom("!a:hs").Resolve([]state.StateMap{g.set("$a"), g.set("$b")}, g); err == nil {
		t.Fatal("ожидали ошибку для события другой комнаты")
	}
}

// TestPowerSortScaling - 2000 конфликтующих ключей (2 набора: self-leave против
// ban) разрешаются меньше чем за 2 с и ban побеждает везде (раньше было
// 119 с CPU из-за полной сортировки с json.Unmarshal в компараторе).
//
// Без t.Parallel: замер времени не должен делить CPU с соседними тестами.
func TestPowerSortScaling(t *testing.T) {
	const n = 2000

	base := roomState("public")
	g := newGraph(base...)
	leaves := append([]string{}, ids(base)...)
	bans := append([]string{}, ids(base)...)

	for i := range n {
		user := fmt.Sprintf("@u%d:hs2", i)
		g.add(member(fmt.Sprintf("$l%d", i), user, user, "leave", int64(100+i), "$create", "$pl"),
			member(fmt.Sprintf("$b%d", i), rAlice, user, "ban", int64(10000+i), "$create", "$pl", "$alice"))
		leaves = append(leaves, fmt.Sprintf("$l%d", i))
		bans = append(bans, fmt.Sprintf("$b%d", i))
	}

	started := time.Now()
	res := resolve(t, g, g.set(leaves...), g.set(bans...))
	elapsed := time.Since(started)

	if limit := 2 * time.Second * raceSlowdown; elapsed > limit {
		t.Errorf("%d конфликтующих ключей: %v, ожидали < %v", n, elapsed, limit)
	}

	t.Logf("%d конфликтующих ключей: %v", n, elapsed)

	for i := range n {
		if got := res[state.Key{Type: "m.room.member", StateKey: fmt.Sprintf("@u%d:hs2", i)}]; got != fmt.Sprintf("$b%d", i) {
			t.Fatalf("u%d = %s, ожидали ban", i, got)
		}
	}
}
