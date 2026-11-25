package core

import (
	"testing"
)

// TestFilterLeaveState — в state покинутой комнаты остаются name, avatar,
// canonical_alias, topic и member самого пользователя, прочее вырезается.
func TestFilterLeaveState(t *testing.T) {
	t.Parallel()

	key := func(s string) *string { return &s }

	state := []Event{
		{Type: "m.room.name", StateKey: key("")},
		{Type: "m.room.avatar", StateKey: key("")},
		{Type: "m.room.canonical_alias", StateKey: key("")},
		{Type: "m.room.topic", StateKey: key("")},
		{Type: "m.room.member", StateKey: key("@alice:test")},
		{Type: "m.room.member", StateKey: key("@bob:test")},
		{Type: "m.room.power_levels", StateKey: key("")},
	}

	kept := filterLeaveState(state, "@alice:test")

	got := map[string]int{}
	for i := range kept {
		got[kept[i].Type+"\x00"+*kept[i].StateKey]++
	}

	for _, want := range []string{
		"m.room.name\x00", "m.room.avatar\x00", "m.room.canonical_alias\x00",
		"m.room.topic\x00", "m.room.member\x00@alice:test",
	} {
		if got[want] != 1 {
			t.Fatalf("в state покинутой комнаты нет %q", want)
		}
	}

	if got["m.room.member\x00@bob:test"] != 0 {
		t.Fatal("в state попал member другого пользователя")
	}

	if got["m.room.power_levels\x00"] != 0 {
		t.Fatal("в state попал power_levels")
	}
}
