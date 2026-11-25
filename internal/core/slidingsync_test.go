package core_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// Юнит-тесты модели simplified sliding sync: сортировка и окна,
// required_state, фильтры, состояние соединения, pos.

func ptr[T any](v T) *T { return &v }

func TestSortSlidingRoomsByBumpStamp(t *testing.T) {
	t.Parallel()

	rooms := []core.SlidingRoom{
		{RoomID: "!old", Membership: "join", MembershipPos: 1, LastBump: 10},
		{RoomID: "!invite", Membership: "invite", MembershipPos: 30, LastBump: 99},
		{RoomID: "!new", Membership: "join", MembershipPos: 2, LastBump: 50},
		{RoomID: "!nobump", Membership: "join", MembershipPos: 20},
		{RoomID: "!b-tie", Membership: "join", LastBump: 10},
		{RoomID: "!neg", Membership: "leave", MembershipPos: -5},
	}

	core.SortSlidingRooms(rooms)

	got := make([]string, 0, len(rooms))
	for i := range rooms {
		got = append(got, rooms[i].RoomID)
	}

	// invite - по позиции приглашения (30), а не по событиям комнаты (99); без
	// bump-события - по входу; равные - по room_id; отрицательная - ноль.
	want := []string{"!new", "!invite", "!nobump", "!b-tie", "!old", "!neg"}
	if !slices.Equal(got, want) {
		t.Fatalf("порядок %v, ожидали %v", got, want)
	}

	if stamp := rooms[len(rooms)-1].BumpStamp(); stamp != 0 {
		t.Fatalf("отрицательный bump_stamp: %d", stamp)
	}
}

func TestSlidingWindows(t *testing.T) {
	t.Parallel()

	rooms := make([]core.SlidingRoom, 0, 5)
	for _, id := range []string{"!a", "!b", "!c", "!d", "!e"} {
		rooms = append(rooms, core.SlidingRoom{RoomID: id})
	}

	ops := core.SlidingWindows(rooms, [][2]int{{0, 1}, {3, 10}, {7, 9}, {2, 1}})
	if len(ops) != 4 {
		t.Fatalf("операций %d", len(ops))
	}

	cases := [][]string{{"!a", "!b"}, {"!d", "!e"}, {}, {}}
	for i, want := range cases {
		if !slices.Equal(ops[i].RoomIDs, want) || ops[i].RoomIDs == nil {
			t.Fatalf("окно %v: %v, ожидали %v", ops[i].Range, ops[i].RoomIDs, want)
		}
	}

	if got := core.SlidingWindows(nil, [][2]int{{0, 5}}); len(got) != 1 || len(got[0].RoomIDs) != 0 {
		t.Fatalf("пустой список: %+v", got)
	}
}

func TestRequiredState(t *testing.T) {
	t.Parallel()

	rs := core.NewRequiredState([][2]string{
		{"m.room.member", "$LAZY"},
		{"m.room.member", "$ME"},
		{"m.room.name", ""},
		{"m.space.child", "*"},
		{"m.room.member", "@x:s"},
		{"m.room.name", ""},
	})

	if !rs.Lazy() || rs.All() {
		t.Fatal("ожидали lazy без all")
	}

	if len(rs["m.room.name"]) != 1 {
		t.Fatalf("повтор не схлопнут: %v", rs)
	}

	for _, tc := range []struct {
		typ, key string
		want     bool
	}{
		{"m.room.name", "", true},
		{"m.room.topic", "", false},
		{"m.space.child", "!any", true},
		{"m.room.member", "@me:s", true},
		{"m.room.member", "@x:s", true},
		{"m.room.member", "@other:s", false},
	} {
		if got := rs.Includes(tc.typ, tc.key, "@me:s"); got != tc.want {
			t.Fatalf("Includes(%s,%s) = %v", tc.typ, tc.key, got)
		}
	}

	if got := rs.ExplicitMembers("@me:s"); !slices.Equal(got, []string{"@me:s", "@x:s"}) {
		t.Fatalf("явные участники: %v", got)
	}

	all := core.NewRequiredState([][2]string{{"*", "*"}, {"m.room.member", "$LAZY"}})
	if !all.All() || all.Lazy() || !all.Includes("anything", "k", "@me:s") {
		t.Fatal("[*,*] - все состояние, без lazy")
	}

	combined := rs.Combine(core.NewRequiredState([][2]string{{"m.room.topic", ""}}))
	if !combined.Includes("m.room.topic", "", "") || !combined.Includes("m.room.name", "", "") || rs.Includes("m.room.topic", "", "") {
		t.Fatalf("Combine: %v (исходный изменен: %v)", combined, rs)
	}

	if rs.Equal(combined) || !rs.Equal(rs.Combine(core.RequiredState{})) || rs.Equal(core.RequiredState{"m.room.name": {""}}) {
		t.Fatal("Equal")
	}
}

func TestRequiredStateAddedSince(t *testing.T) {
	t.Parallel()

	prev := core.NewRequiredState([][2]string{{"m.room.name", ""}, {"m.room.member", "$ME"}, {"m.space.child", "*"}})

	added := core.NewRequiredState([][2]string{
		{"m.room.name", ""},
		{"m.room.topic", ""},
		{"m.room.member", "$ME"},
		{"m.room.member", "$LAZY"},
		{"m.room.member", "@me:s"},
		{"m.room.member", "@bob:s"},
		{"m.space.child", "!x"},
		{"m.room.pinned", "*"},
		{"m.custom", "$ME"},
	}).AddedSince(prev, "@me:s")

	wantPairs := [][2]string{{"m.custom", "@me:s"}, {"m.room.member", "@bob:s"}, {"m.room.topic", ""}}
	if added.All || !slices.Equal(added.Types, []string{"m.room.pinned"}) || !slices.Equal(added.Pairs, wantPairs) {
		t.Fatalf("добавлено: %+v", added)
	}

	if !core.NewRequiredState([][2]string{{"*", "*"}}).AddedSince(prev, "@me:s").All {
		t.Fatal("добавление [*,*] - читать все")
	}

	if got := prev.AddedSince(core.NewRequiredState([][2]string{{"*", "*"}}), "@me:s"); !got.Empty() {
		t.Fatalf("сужение со всего - ничего не читать: %+v", got)
	}

	if got := prev.AddedSince(prev, "@me:s"); !got.Empty() {
		t.Fatalf("без изменений: %+v", got)
	}
}

func TestSlidingRoomConfigCombine(t *testing.T) {
	t.Parallel()

	a := core.SlidingRoomConfig{TimelineLimit: 1, RequiredState: core.NewRequiredState([][2]string{{"m.room.name", ""}})}
	b := core.SlidingRoomConfig{TimelineLimit: 20, RequiredState: core.NewRequiredState([][2]string{{"m.room.topic", ""}})}

	c := a.Combine(b)
	if c.TimelineLimit != 20 || !c.RequiredState.Includes("m.room.topic", "", "") || !c.RequiredState.Includes("m.room.name", "", "") {
		t.Fatalf("Combine: %+v", c)
	}

	if a.Equal(c) || !c.Equal(b.Combine(a)) {
		t.Fatal("Equal")
	}
}

func TestSlidingFiltersMatch(t *testing.T) {
	t.Parallel()

	space := "m.space"
	room := &core.SlidingRoom{
		RoomID: "!r", Membership: "join", Known: true, Encrypted: true, DM: true,
		Tags: map[string]bool{"m.favourite": true}, Parents: map[string]bool{"!space": true},
	}
	spaceRoom := &core.SlidingRoom{RoomID: "!s", Membership: "invite", Known: true, RoomType: &space}
	unknown := &core.SlidingRoom{RoomID: "!u", Membership: "invite"}

	var none *core.SlidingFilters
	if !none.Match(room) || none.NeedsRoomInfo() || none.NeedsTags() {
		t.Fatal("без фильтров - все")
	}

	for name, tc := range map[string]struct {
		f    core.SlidingFilters
		want [3]bool // room, spaceRoom, unknown
	}{
		"is_dm":          {core.SlidingFilters{IsDM: ptr(true)}, [3]bool{true, false, false}},
		"not dm":         {core.SlidingFilters{IsDM: ptr(false)}, [3]bool{false, true, true}},
		"is_invite":      {core.SlidingFilters{IsInvite: ptr(true)}, [3]bool{false, true, true}},
		"is_encrypted":   {core.SlidingFilters{IsEncrypted: ptr(true)}, [3]bool{true, false, false}},
		"not encrypted":  {core.SlidingFilters{IsEncrypted: ptr(false)}, [3]bool{false, true, false}},
		"room_types":     {core.SlidingFilters{RoomTypes: []*string{&space}, HasRoomTypes: true}, [3]bool{false, true, false}},
		"room_types nil": {core.SlidingFilters{RoomTypes: []*string{nil}, HasRoomTypes: true}, [3]bool{true, false, false}},
		"not_room_types": {core.SlidingFilters{NotRoomTypes: []*string{&space}, HasNotRoomTypes: true}, [3]bool{true, false, false}},
		"empty types":    {core.SlidingFilters{RoomTypes: []*string{}, HasRoomTypes: true}, [3]bool{false, false, false}},
		"tags":           {core.SlidingFilters{Tags: []string{"m.favourite"}, HasTags: true}, [3]bool{true, false, false}},
		"not_tags":       {core.SlidingFilters{NotTags: []string{"m.favourite"}, HasNotTags: true}, [3]bool{false, true, true}},
		"spaces":         {core.SlidingFilters{Spaces: []string{"!space"}}, [3]bool{true, false, false}},
	} {
		got := [3]bool{tc.f.Match(room), tc.f.Match(spaceRoom), tc.f.Match(unknown)}
		if got != tc.want {
			t.Fatalf("%s: %v, ожидали %v", name, got, tc.want)
		}
	}

	if f := (&core.SlidingFilters{IsEncrypted: ptr(true)}); !f.NeedsRoomInfo() || f.NeedsTags() {
		t.Fatal("NeedsRoomInfo")
	}

	if f := (&core.SlidingFilters{NotTags: []string{"x"}, HasNotTags: true}); f.NeedsRoomInfo() || !f.NeedsTags() {
		t.Fatal("NeedsTags")
	}
}

func TestSlidingConnStateTransitions(t *testing.T) {
	t.Parallel()

	st := core.NewSlidingConnState()
	if st.Dirty() {
		t.Fatal("новое состояние не изменено")
	}

	st.RecordUnsent(core.SlidingStreamRooms, 5, "!never")

	if _, ok := st.Status(core.SlidingStreamRooms, "!never"); ok || st.Dirty() {
		t.Fatal("NEVER остается NEVER")
	}

	st.RecordSent(core.SlidingStreamRooms, "!a", "!b")
	st.RecordSent(core.SlidingStreamReceipts, "!a")
	st.RecordUnsent(core.SlidingStreamRooms, 7, "!a")
	st.RecordUnsent(core.SlidingStreamRooms, 9, "!a")

	if s, ok := st.Status(core.SlidingStreamRooms, "!a"); !ok || s.Live || s.Since != 7 {
		t.Fatalf("LIVE -> PREVIOUSLY(7) и позиция не сдвигается: %+v", s)
	}

	if s, _ := st.Status(core.SlidingStreamReceipts, "!a"); !s.Live {
		t.Fatal("потоки независимы")
	}

	clone := st.Clone()
	clone.RecordSent(core.SlidingStreamRooms, "!a")

	if s, _ := st.Status(core.SlidingStreamRooms, "!a"); s.Live {
		t.Fatal("копия меняет исходное")
	}

	cfg := core.SlidingRoomConfig{TimelineLimit: 3, RequiredState: core.NewRequiredState([][2]string{{"m.room.name", ""}})}
	st.SetConfig("!a", cfg)
	st.SetConfig("!b", cfg)
	st.SetLazyMembers("!a", []string{"@x:s"})

	many := make([]string, 0, 150)
	for i := range 150 {
		many = append(many, "@u"+string(rune('a'+i%26))+string(rune('a'+i/26))+":s")
	}

	st.SetLazyMembers("!b", many)

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}

	var states []core.RequiredState
	if err := json.Unmarshal(stored["states"], &states); err != nil || len(states) != 1 {
		t.Fatalf("одинаковый required_state хранится один раз: %s", stored["states"])
	}

	back := core.NewSlidingConnState()
	if err := json.Unmarshal(raw, back); err != nil {
		t.Fatal(err)
	}

	if got, ok := back.Config("!b"); !ok || !got.Equal(cfg) {
		t.Fatalf("config после чтения: %+v", got)
	}

	if s, _ := back.Status(core.SlidingStreamRooms, "!a"); s.Live || s.Since != 7 {
		t.Fatalf("статус после чтения: %+v", s)
	}

	if len(back.LazyMembers["!b"]) != 100 || back.LazyMembers["!a"][0] != "@x:s" {
		t.Fatalf("lazy после чтения: %d", len(back.LazyMembers["!b"]))
	}

	if back.Dirty() {
		t.Fatal("прочитанное состояние не изменено")
	}

	back.SetLazyMembers("!a", nil)
	back.SetConfig("!b", cfg)

	if _, ok := back.LazyMembers["!a"]; !ok && !back.Dirty() {
		t.Fatal("удаление lazy - изменение")
	}

	var broken core.SlidingConnState
	if err := json.Unmarshal([]byte(`{"configs":{"!x":[5,7]}}`), &broken); err != nil {
		t.Fatal(err)
	}

	if cfg, _ := broken.Config("!x"); cfg.TimelineLimit != 5 || len(cfg.RequiredState) != 0 {
		t.Fatalf("ссылка на несуществующий required_state: %+v", cfg)
	}

	if err := json.Unmarshal([]byte(`[`), &broken); err == nil {
		t.Fatal("битый JSON")
	}
}

func TestSlidingPosRoundTrip(t *testing.T) {
	t.Parallel()

	codec := core.NewSyncTokenCodec("test.local", []byte("secret"))
	pos := core.SlidingPos{Conn: 12, Stream: 345, Ephemeral: 6789}

	token := codec.EncodeSliding(pos)

	got, err := codec.DecodeSliding(token)
	if err != nil || got != pos {
		t.Fatalf("DecodeSliding(%s) = %+v, %v", token, got, err)
	}

	if _, err := codec.Decode(token); err == nil {
		t.Fatal("pos не должен приниматься как since /sync")
	}

	other := core.NewSyncTokenCodec("other.local", []byte("secret"))
	for _, bad := range []string{
		"", "garbage", "s1.c1.e2.r3", other.EncodeSliding(pos), codec.Encode(5), token + "x",
		"s1.c1.e2.x3." + "mac", "5/s58_224_0_13_10_1_1_16_0_1",
	} {
		if _, err := codec.DecodeSliding(bad); !errors.Is(err, core.ErrUnknownPos) {
			t.Fatalf("DecodeSliding(%q) = %v, ожидали ErrUnknownPos", bad, err)
		}
	}

	if got, _ := codec.DecodeSliding(codec.EncodeSliding(core.SlidingPos{Conn: -1, Stream: -2})); got != (core.SlidingPos{}) {
		t.Fatalf("отрицательные позиции: %+v", got)
	}
}
