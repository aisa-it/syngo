package auth_test

import (
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

// v12Room - room_id комнаты v12: хеш m.room.create без домена.
const v12Room = "!BGO0qy-BCFl9JY0_IBxh2iYSjWGT3yVx5xdDtzn5PLg"

// v12State - комната v12 из Synapse 1.165: создатель alice, дополнительный создатель
// bob, в power_levels их нет (users только carol c 50), tombstone требует 150.
func v12State(t *testing.T) auth.State {
	t.Helper()

	events := make([]auth.Event, 0, 7)
	events = append(events,
		auth.Event{
			EventID: "$" + v12Room[1:], RoomID: v12Room, Type: "m.room.create", Sender: "@alice:test", StateKey: ptr(""),
			Content: []byte(`{"room_version":"12","additional_creators":["@bob:test"],"m.federate":false}`),
		},
		auth.Event{
			EventID: "$pl", RoomID: v12Room, Type: "m.room.power_levels", Sender: "@alice:test", StateKey: ptr(""),
			Content: []byte(`{"users":{"@carol:test":50},"users_default":0,"state_default":50,"ban":50,"kick":50,` +
				`"events":{"m.room.tombstone":150,"m.room.power_levels":100}}`),
		},
		auth.Event{
			EventID: "$jr", RoomID: v12Room, Type: "m.room.join_rules", Sender: "@alice:test", StateKey: ptr(""),
			Content: []byte(`{"join_rule":"invite"}`),
		},
	)

	for _, user := range []string{"@alice:test", "@bob:test", "@carol:test", "@dave:test"} {
		events = append(events, auth.Event{
			EventID: "$m" + user, RoomID: v12Room, Type: "m.room.member", Sender: user,
			StateKey: ptr(user), Content: []byte(`{"membership":"join"}`),
		})
	}

	state, err := auth.BuildState(events)
	if err != nil {
		t.Fatal(err)
	}

	return state
}

func v12Event(typ, sender string, stateKey *string, content string) auth.Event {
	return auth.Event{EventID: "$x", RoomID: v12Room, Type: typ, Sender: sender, StateKey: stateKey, Content: []byte(content)}
}

// TestV12CreatorsRules - в комнате v12 создатели (отправитель create и
// additional_creators) имеют бесконечный уровень без записи в power_levels, их
// нельзя вписать в power_levels.users, выгнать или забанить. Раньше уровень
// создателя считался по users_default (0), и после переезда из Synapse он
// терял права в своей комнате.
func TestV12CreatorsRules(t *testing.T) {
	t.Parallel()

	state := v12State(t)

	cases := []struct {
		name  string
		event auth.Event
		allow bool
	}{
		{"создатель меняет название (state_default 50)", v12Event("m.room.name", "@alice:test", ptr(""), `{"name":"x"}`), true},
		{"дополнительный создатель - tombstone (150)", v12Event("m.room.tombstone", "@bob:test", ptr(""), `{"replacement_room":"!n"}`), true},
		{"модератор 50 - tombstone (150)", v12Event("m.room.tombstone", "@carol:test", ptr(""), `{"replacement_room":"!n"}`), false},
		{"создатель выгоняет модератора", v12Event("m.room.member", "@bob:test", ptr("@carol:test"), `{"membership":"leave"}`), true},
		{"модератор выгоняет создателя", v12Event("m.room.member", "@carol:test", ptr("@alice:test"), `{"membership":"ban"}`), false},
		{"создатель банит другого создателя", v12Event("m.room.member", "@alice:test", ptr("@bob:test"), `{"membership":"ban"}`), false},
		{"создатель выдает 100 участнику", v12Event("m.room.power_levels", "@alice:test", ptr(""),
			`{"users":{"@carol:test":50,"@dave:test":100},"events":{"m.room.tombstone":150,"m.room.power_levels":100}}`), true},
		{"создатель в power_levels.users", v12Event("m.room.power_levels", "@alice:test", ptr(""),
			`{"users":{"@carol:test":50,"@alice:test":100}}`), false},
		{"дополнительный создатель в power_levels.users", v12Event("m.room.power_levels", "@alice:test", ptr(""),
			`{"users":{"@bob:test":100}}`), false},
		{"обычный участник - название", v12Event("m.room.name", "@dave:test", ptr(""), `{"name":"x"}`), false},
	}

	for _, tc := range cases {
		err := auth.Check(tc.event, state)
		if tc.allow && err != nil {
			t.Fatalf("%s: отказ %v", tc.name, err)
		}

		if !tc.allow && !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("%s: ожидали ErrUnauthorized, получили %v", tc.name, err)
		}
	}

	if got := auth.Level(state, "@bob:test"); got != auth.CreatorLevel {
		t.Fatalf("Level(дополнительный создатель) = %d", got)
	}

	if got := auth.Level(state, "@carol:test"); got != 50 {
		t.Fatalf("Level(carol) = %d", got)
	}
}

// TestV12CreateEvent - create комнаты v12 не сверяет домен room_id (его
// нет), но требует корректных additional_creators; для v11 домен по-прежнему
// обязан совпадать.
func TestV12CreateEvent(t *testing.T) {
	t.Parallel()

	state := v12State(t)

	create := func(roomID, content string) auth.Event {
		return auth.Event{
			EventID: "$c", RoomID: roomID, Type: "m.room.create", Sender: "@alice:test",
			StateKey: ptr(""), Content: []byte(content),
		}
	}

	if err := auth.Check(create(v12Room, `{"room_version":"12","additional_creators":["@bob:test"]}`), state); err != nil {
		t.Fatalf("create v12: %v", err)
	}

	for _, content := range []string{
		`{"room_version":"12","additional_creators":"@bob:test"}`,
		`{"room_version":"12","additional_creators":["bob"]}`,
		`{"room_version":"12","additional_creators":[1]}`,
	} {
		if err := auth.Check(create(v12Room, content), state); err == nil {
			t.Fatalf("create v12 с %s принят", content)
		}
	}

	if err := auth.Check(create("!r:other", `{"room_version":"11"}`), state); err == nil {
		t.Fatal("create v11 с чужим доменом room_id принят")
	}
}

// TestV12AuthEventsAndFederate - в комнате v12 create в auth_events
// запрещен и не обязателен; m.federate=false сверяет сервер отправителя с
// сервером создателя (у room_id домена нет).
func TestV12AuthEventsAndFederate(t *testing.T) {
	t.Parallel()

	message := v12Event("m.room.message", "@alice:test", nil, `{"body":"x"}`)
	member := auth.Event{
		EventID: "$m", RoomID: v12Room, Type: "m.room.member", Sender: "@alice:test",
		StateKey: ptr("@alice:test"), Content: []byte(`{"membership":"join"}`),
	}
	create := auth.Event{
		EventID: "$" + v12Room[1:], RoomID: v12Room, Type: "m.room.create", Sender: "@alice:test",
		StateKey: ptr(""), Content: []byte(`{"room_version":"12"}`),
	}

	if err := auth.ValidateAuthEvents(message, []auth.Event{member}); err != nil {
		t.Fatalf("auth_events v12 без create: %v", err)
	}

	if err := auth.ValidateAuthEvents(message, []auth.Event{create, member}); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("auth_events v12 с create: %v", err)
	}

	state := v12State(t)
	state.Members["@eve:other"] = "join"

	if err := auth.Check(v12Event("m.room.message", "@alice:test", nil, `{"body":"x"}`), state); err != nil {
		t.Fatalf("m.federate=false, свой сервер: %v", err)
	}

	if err := auth.Check(v12Event("m.room.message", "@eve:other", nil, `{"body":"x"}`), state); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("m.federate=false, чужой сервер: %v", err)
	}
}

// TestCreatorsPrivileged - версии с правилами создателей v12.
func TestCreatorsPrivileged(t *testing.T) {
	t.Parallel()

	for version, want := range map[string]bool{
		"10": false, "11": false, "12": true, "13": true, "org.matrix.hydra.11": true, "": false, "x": false,
	} {
		if got := auth.CreatorsPrivileged(version); got != want {
			t.Fatalf("CreatorsPrivileged(%q) = %v", version, got)
		}
	}

	for roomID, want := range map[string]bool{v12Room: true, "!r:test": false, "!": false, "#a:test": false} {
		if got := auth.IsCreateHashRoomID(roomID); got != want {
			t.Fatalf("IsCreateHashRoomID(%q) = %v", roomID, got)
		}
	}
}

// TestV12CreatorJoinEmptyAuthEvents - у первого события создателя
// комнаты v12 (его join) auth_events пуст - create в них не входит, членства
// еще нет. Такой join Synapse присылает в auth_chain send_join и /state; раньше
// он отвергался («empty auth_events»). В комнате ранней версии пустой набор -
// по-прежнему отказ.
func TestV12CreatorJoinEmptyAuthEvents(t *testing.T) {
	t.Parallel()

	join := auth.Event{
		EventID: "$j", RoomID: v12Room, Type: "m.room.member", Sender: "@alice:test",
		StateKey: ptr("@alice:test"), Content: []byte(`{"membership":"join"}`),
	}

	if err := auth.ValidateAuthEvents(join, nil); err != nil {
		t.Fatalf("join создателя v12 без auth_events: %v", err)
	}

	message := v12Event("m.room.message", "@alice:test", nil, `{"body":"x"}`)
	if err := auth.ValidateAuthEvents(message, nil); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("сообщение v12 без членства отправителя: %v", err)
	}

	join.RoomID = "!old:test"
	if err := auth.ValidateAuthEvents(join, nil); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("пустые auth_events в комнате v11: %v", err)
	}
}
