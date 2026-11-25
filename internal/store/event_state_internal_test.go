package store

import (
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/state"
)

// TestStateDelta - дельта группы: измененные и новые ключи; пропавший ключ -
// только снимок.
func TestStateDelta(t *testing.T) {
	t.Parallel()

	topic, pl, bob := state.Key{Type: "m.room.topic"}, state.Key{Type: "m.room.power_levels"},
		state.Key{Type: "m.room.member", StateKey: "@bob:hs"}
	parent := state.StateMap{topic: "$t1", pl: "$pl"}

	delta, ok := stateDelta(parent, state.StateMap{topic: "$t2", pl: "$pl", bob: "$bob"})
	if !ok || len(delta) != 2 || delta[topic] != "$t2" || delta[bob] != "$bob" {
		t.Fatalf("delta %v ok=%v, ожидали topic и bob", delta, ok)
	}

	if delta, ok := stateDelta(parent, state.StateMap{topic: "$t1", pl: "$pl"}); !ok || len(delta) != 0 {
		t.Fatalf("без изменений: delta %v ok=%v", delta, ok)
	}

	if _, ok := stateDelta(parent, state.StateMap{topic: "$t1"}); ok {
		t.Fatal("пропавший ключ должен давать снимок")
	}
}

// TestChangedStateRows - изменившиеся строки room_state по порядку ключей,
// удаленный ключ - с пустым EventID.
func TestChangedStateRows(t *testing.T) {
	t.Parallel()

	current := state.StateMap{
		{Type: "m.room.topic"}:                     "$t1",
		{Type: "m.room.member", StateKey: "@b:hs"}: "$b1",
		{Type: "m.room.member", StateKey: "@a:hs"}: "$a1",
		{Type: "m.room.join_rules"}:                "$jr",
	}
	next := state.StateMap{
		{Type: "m.room.topic"}:                     "$t2",
		{Type: "m.room.member", StateKey: "@a:hs"}: "$a1",
		{Type: "m.room.member", StateKey: "@b:hs"}: "$b2",
		{Type: "m.room.member", StateKey: "@c:hs"}: "$c1",
	}

	got := fmt.Sprint(changedStateRows(current, next))
	want := "[{m.room.join_rules  } {m.room.member @b:hs $b2} {m.room.member @c:hs $c1} {m.room.topic  $t2}]"

	if got != want {
		t.Fatalf("changed %s, ожидали %s", got, want)
	}

	if rows := changedStateRows(next, next); len(rows) != 0 {
		t.Fatalf("без изменений: %v", rows)
	}
}
