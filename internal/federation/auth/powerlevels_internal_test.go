package auth

import "testing"

// TestPowerLevelsContentRules — правила спеки для m.room.power_levels:
// значения целые, users — валидные user_id, нельзя выдать уровень выше своего
// и изменить уровень участника не ниже своего.
func TestPowerLevelsContentRules(t *testing.T) {
	t.Parallel()

	fifty := 50

	state := State{
		Create:      &Event{Type: "m.room.create", Sender: "@mod:test", Content: []byte(`{"creator":"@mod:test","m.federate":true}`)},
		Members:     map[string]string{"@mod:test": "join", "@other:test": "join"},
		PowerLevels: &powerLevels{Users: map[string]int{"@mod:test": 50, "@other:test": 100}, Ban: &fifty},
	}

	ev := func(content string) Event {
		return Event{
			Type: "m.room.power_levels", RoomID: "!r:test", Sender: "@mod:test", StateKey: strPtr(""),
			Content: []byte(content),
		}
	}

	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"свои уровни менять можно", `{"ban":50,"users":{"@mod:test":50,"@other:test":100}}`, false},
		{"notifications - объект уровней", `{"users":{"@mod:test":50,"@other:test":100},"notifications":{"room":50}}`, false},
		{"notifications с нецелым значением", `{"users":{"@mod:test":50,"@other:test":100},"notifications":{"room":"50"}}`, true},
		{"нельзя убрать участника выше себя", `{"users":{"@mod:test":50}}`, true},
		{"нельзя выдать уровень выше своего", `{"users":{"@mod:test":100}}`, true},
		{"нельзя понизить участника выше себя", `{"users":{"@other:test":0}}`, true},
		{"нецелое значение", `{"ban":"50"}`, true},
		{"кривой user_id", `{"users":{"not-a-user":0}}`, true},
		{"порог события выше своего", `{"events":{"m.room.name":100}}`, true},
	}

	for _, tc := range cases {
		err := Check(ev(tc.content), state)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: ожидали отказ", tc.name)
		}

		if !tc.wantErr && err != nil {
			t.Fatalf("%s: неожидали отказ: %v", tc.name, err)
		}
	}
}

// TestPowerLevelsDemoteUsersDefault - участник с уровнем 100 меняет users_default
// 100 -> 40 при notifications в content, но не меняет уровень другого участника,
// равный своему (спека, Complement TestDemotingUsersViaUsersDefault).
func TestPowerLevelsDemoteUsersDefault(t *testing.T) {
	t.Parallel()

	hundred := 100

	state := State{
		Create:  &Event{Type: "m.room.create", Sender: "@alice:test", Content: []byte(`{"creator":"@alice:test"}`)},
		Members: map[string]string{"@alice:test": "join", "@bob:test": "join"},
		PowerLevels: &powerLevels{
			Users: map[string]int{"@alice:test": 100, "@bob:test": 100}, UsersDefault: &hundred,
		},
	}

	ev := Event{
		Type: "m.room.power_levels", RoomID: "!r:test", Sender: "@bob:test", StateKey: strPtr(""),
		Content: []byte(`{"users_default":40,"users":{"@alice:test":100,"@bob:test":100},"events":{},"notifications":{}}`),
	}

	if err := Check(ev, state); err != nil {
		t.Fatalf("понижение users_default: %v", err)
	}

	ev.Content = []byte(`{"users_default":100,"users":{"@alice:test":50,"@bob:test":100}}`)

	if err := Check(ev, state); err == nil {
		t.Fatal("изменение уровня другого участника, равного своему, должно отклоняться")
	}
}
