package auth_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

// stateWith собирает состояние из baseState и дополнительных событий.
func stateWith(t *testing.T, extra ...auth.Event) auth.State {
	t.Helper()

	events := make([]auth.Event, 0, 4+len(extra))
	events = append(events,
		ev("m.room.create", "@alice:test.local", ptr(""), `{"creator":"@alice:test.local"}`, nil),
		ev("m.room.power_levels", "@alice:test.local", ptr(""),
			`{"users":{"@alice:test.local":100,"@carol:test.local":0},"state_default":50,"invite":0,"kick":50,"ban":50}`, nil),
		ev("m.room.member", "@alice:test.local", ptr("@alice:test.local"), `{"membership":"join"}`, nil),
		ev("m.room.member", "@alice:test.local", ptr("@carol:test.local"), `{"membership":"join"}`, nil))
	events = append(events, extra...)

	state, err := auth.BuildState(events)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}

	return state
}

// TestMembershipRulesV10 — правила членства, которых не было: бан блокирует вход,
// забаненный не снимает бан выходом, invite проверяется, kick требует порога,
// knock — по join rules.
func TestMembershipRulesV10(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		state     auth.State
		event     auth.Event
		wantError bool
	}{
		{
			name: "забаненный не входит в публичную комнату",
			state: func() auth.State {
				s := stateWith(t)
				s.JoinRules = "public"
				s.Members["@bob:remote.test"] = "ban"
				return s
			}(),
			event:     ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"join"}`, nil),
			wantError: true,
		},
		{
			name:      "забаненный не снимает бан своим leave",
			state:     func() auth.State { s := stateWith(t); s.Members["@bob:remote.test"] = "ban"; return s }(),
			event:     ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"leave"}`, nil),
			wantError: true,
		},
		{
			name: "участник с PL 0 не приглашает при invite=50",
			state: stateWith(t, ev("m.room.power_levels", "@alice:test.local", ptr(""),
				`{"users":{"@alice:test.local":100,"@carol:test.local":0},"state_default":50,"invite":50}`, nil)),
			event:     ev("m.room.member", "@carol:test.local", ptr("@mallory:remote.test"), `{"membership":"invite"}`, nil),
			wantError: true,
		},
		{
			name:      "пригласить забаненного нельзя",
			state:     func() auth.State { s := stateWith(t); s.Members["@bob:remote.test"] = "ban"; return s }(),
			event:     ev("m.room.member", "@alice:test.local", ptr("@bob:remote.test"), `{"membership":"invite"}`, nil),
			wantError: true,
		},
		{
			name:      "пригласить уже вошедшего нельзя",
			state:     stateWith(t),
			event:     ev("m.room.member", "@alice:test.local", ptr("@carol:test.local"), `{"membership":"invite"}`, nil),
			wantError: true,
		},
		{
			name:      "кик без порога отклоняется",
			state:     stateWith(t),
			event:     ev("m.room.member", "@carol:test.local", ptr("@alice:test.local"), `{"membership":"leave"}`, nil),
			wantError: true,
		},
		{
			name:  "unban забаненного с порогом ban разрешён",
			state: func() auth.State { s := stateWith(t); s.Members["@bob:remote.test"] = "ban"; return s }(),
			event: ev("m.room.member", "@alice:test.local", ptr("@bob:remote.test"), `{"membership":"leave"}`, nil),
		},
		{
			name:      "knock при join_rules=invite отклоняется",
			state:     func() auth.State { s := stateWith(t); s.JoinRules = "invite"; return s }(),
			event:     ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"knock"}`, nil),
			wantError: true,
		},
		{
			name:  "knock при join_rules=knock разрешён",
			state: func() auth.State { s := stateWith(t); s.JoinRules = "knock"; return s }(),
			event: ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"knock"}`, nil),
		},
	}

	for _, tc := range cases {
		err := auth.Check(tc.event, tc.state)
		if tc.wantError && err == nil {
			t.Errorf("%s: ожидали отказ, событие принято", tc.name)
		}

		if !tc.wantError && err != nil {
			t.Errorf("%s: неожиданный отказ: %v", tc.name, err)
		}
	}
}

// TestCreateContentChecks — create.creator обязан совпадать с отправителем,
// m.federate=false запрещает события с чужих серверов.
func TestCreateContentChecks(t *testing.T) {
	t.Parallel()

	state := stateWith(t)

	badCreate := auth.Event{
		EventID: "$c", RoomID: "!r:test.local", Type: "m.room.create", Sender: "@alice:test.local",
		StateKey: ptr(""), Content: json.RawMessage(`{"creator":"@mallory:remote.test"}`),
	}

	if err := auth.Check(badCreate, state); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("create с чужим creator принят: %v", err)
	}

	// m.federate=false: событие с чужого домена отклоняется.
	fedOff := stateWith(t)
	fedOff.Create = &auth.Event{
		EventID: "$c", RoomID: "!r:test.local", Type: "m.room.create", Sender: "@alice:test.local",
		StateKey: ptr(""), Content: json.RawMessage(`{"creator":"@alice:test.local","m.federate":false}`),
	}
	fedOff.Members["@bob:remote.test"] = "join"

	foreign := ev("m.room.message", "@bob:remote.test", nil, `{"body":"x"}`, nil)
	if err := auth.Check(foreign, fedOff); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("событие с чужого домена при m.federate=false принято: %v", err)
	}

	local := ev("m.room.message", "@alice:test.local", nil, `{"body":"x"}`, nil)
	if err := auth.Check(local, fedOff); err != nil {
		t.Fatalf("локальное событие при m.federate=false отклонено: %v", err)
	}
}

// TestRestrictedJoin — вход в restricted-комнату требует авторизующего участника
// с достаточным уровнем; без него restricted-комната открыта всем.
func TestRestrictedJoin(t *testing.T) {
	t.Parallel()

	restricted := func() auth.State {
		s := stateWith(t)
		s.JoinRules = "restricted"
		s.JoinRulesAllow = []string{"!allowed:test.local"}

		return s
	}

	cases := []struct {
		name      string
		content   string
		allow     map[string]string
		wantError bool
	}{
		{
			name:      "без авторизующего отклонён",
			content:   `{"membership":"join"}`,
			wantError: true,
		},
		{
			name:      "авторизующий не в комнате — отказ",
			content:   `{"membership":"join","join_authorised_via_users_server":"@nobody:remote.test"}`,
			wantError: true,
		},
		{
			name:      "сам себя авторизовать не может",
			content:   `{"membership":"join","join_authorised_via_users_server":"@bob:remote.test"}`,
			wantError: true,
		},
		{
			name:    "авторизующий участник — вход разрешён",
			content: `{"membership":"join","join_authorised_via_users_server":"@alice:test.local"}`,
		},
		{
			name:    "членство в allow-комнате разрешает вход",
			content: `{"membership":"join"}`,
			allow:   map[string]string{"!allowed:test.local": "join"},
		},
		{
			name:      "leave в allow-комнате не разрешает вход",
			content:   `{"membership":"join"}`,
			allow:     map[string]string{"!allowed:test.local": "leave"},
			wantError: true,
		},
	}

	for _, tc := range cases {
		event := ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), tc.content, nil)

		st := restricted()
		st.AllowMemberships = tc.allow

		err := auth.Check(event, st)
		if tc.wantError && err == nil {
			t.Errorf("%s: ожидали отказ, вход принят", tc.name)
		}

		if !tc.wantError && err != nil {
			t.Errorf("%s: неожиданный отказ: %v", tc.name, err)
		}
	}
}

// TestKickBanRequireHigherLevel — равный по уровню участник не может кикнуть или
// забанить другого, даже если пороги kick/ban соблюдены (спека v10).
func TestKickBanRequireHigherLevel(t *testing.T) {
	t.Parallel()

	// alice и carol оба 50: порог kick/ban (50) соблюдён, но уровень не выше.
	equal := stateWith(t, ev("m.room.power_levels", "@alice:test.local", ptr(""),
		`{"users":{"@alice:test.local":50,"@carol:test.local":50},"state_default":50,"kick":50,"ban":50}`, nil))

	cases := []struct {
		name      string
		event     auth.Event
		state     auth.State
		wantError bool
	}{
		{
			name:      "равный по уровню кикает — отказ",
			event:     ev("m.room.member", "@carol:test.local", ptr("@alice:test.local"), `{"membership":"leave"}`, nil),
			state:     equal,
			wantError: true,
		},
		{
			name:      "равный по уровню банит — отказ",
			event:     ev("m.room.member", "@carol:test.local", ptr("@alice:test.local"), `{"membership":"ban"}`, nil),
			state:     equal,
			wantError: true,
		},
		{
			name:  "старший по уровню банит — можно",
			event: ev("m.room.member", "@alice:test.local", ptr("@carol:test.local"), `{"membership":"ban"}`, nil),
			state: stateWith(t),
		},
	}

	for _, tc := range cases {
		err := auth.Check(tc.event, tc.state)
		if tc.wantError && err == nil {
			t.Errorf("%s: ожидали отказ, действие принято", tc.name)
		}

		if !tc.wantError && err != nil {
			t.Errorf("%s: неожиданный отказ: %v", tc.name, err)
		}
	}
}

// TestCreatorWithoutContentCreator - в v11 creator убран из content
// m.room.create, создатель - отправитель create. Он вступает первым и до
// power_levels имеет уровень 100; чужой - нет.
func TestCreatorWithoutContentCreator(t *testing.T) {
	t.Parallel()

	create := ev("m.room.create", "@alice:test.local", ptr(""), `{"room_version":"11"}`, nil)

	state, err := auth.BuildState([]auth.Event{create})
	if err != nil {
		t.Fatalf("build state: %v", err)
	}

	if level := auth.Level(state, "@alice:test.local"); level != 100 {
		t.Fatalf("уровень создателя до power_levels = %d, want 100", level)
	}

	if level := auth.Level(state, "@bob:test.local"); level != 0 {
		t.Fatalf("уровень чужого = %d, want 0", level)
	}

	join := ev("m.room.member", "@alice:test.local", ptr("@alice:test.local"), `{"membership":"join"}`, nil)
	if err := auth.Check(join, state); err != nil {
		t.Fatalf("первый join создателя v11 отклонен: %v", err)
	}

	foreign := ev("m.room.member", "@bob:test.local", ptr("@bob:test.local"), `{"membership":"join"}`, nil)
	if err := auth.Check(foreign, state); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("join чужого в комнату без join_rules принят: %v", err)
	}
}
