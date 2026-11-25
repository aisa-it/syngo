package auth_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

func ev(typ, sender string, stateKey *string, content string, prev []string) auth.Event {
	return auth.Event{
		EventID: "$e", RoomID: "!r:test.local", Type: typ, Sender: sender,
		StateKey: stateKey, Content: json.RawMessage(content), Prev: prev, Depth: 1,
	}
}

func ptr(s string) *string { return &s }

// baseState — create + power_levels(100 у alice) + invite bob.
func baseState(t *testing.T) auth.State {
	t.Helper()

	state, err := auth.BuildState([]auth.Event{
		ev("m.room.create", "@alice:test.local", ptr(""), `{"creator":"@alice:test.local"}`, nil),
		ev("m.room.power_levels", "@alice:test.local", ptr(""), `{"users":{"@alice:test.local":100},"state_default":50}`, nil),
		ev("m.room.member", "@alice:test.local", ptr("@alice:test.local"), `{"membership":"join"}`, nil),
		ev("m.room.member", "@alice:test.local", ptr("@bob:remote.test"), `{"membership":"invite"}`, nil),
	})
	if err != nil {
		t.Fatalf("build state: %v", err)
	}

	return state
}

// TestAuthRules — минимальные правила v10/v11: create, membership, power levels.
func TestAuthRules(t *testing.T) {
	t.Parallel()

	state := baseState(t)

	tests := []struct {
		name    string
		event   auth.Event
		wantErr bool
	}{
		{
			name:  "invite -> join",
			event: ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"join"}`, nil),
		},
		{
			name:    "join без инвайта в приватной комнате",
			event:   ev("m.room.member", "@eve:remote.test", ptr("@eve:remote.test"), `{"membership":"join"}`, nil),
			wantErr: true,
		},
		{
			name:  "leave собой",
			event: ev("m.room.member", "@bob:remote.test", ptr("@bob:remote.test"), `{"membership":"leave"}`, nil),
		},
		{
			name:    "kick без power",
			event:   ev("m.room.member", "@bob:remote.test", ptr("@alice:test.local"), `{"membership":"leave"}`, nil),
			wantErr: true,
		},
		{
			name:    "сообщение от не-участника",
			event:   ev("m.room.message", "@bob:remote.test", nil, `{"body":"x"}`, nil),
			wantErr: true,
		},
		{
			name:    "power_levels от не-админа",
			event:   ev("m.room.power_levels", "@bob:remote.test", ptr(""), `{"users":{}}`, nil),
			wantErr: true,
		},
		{
			name:  "power_levels от админа",
			event: ev("m.room.power_levels", "@alice:test.local", ptr(""), `{"users":{"@alice:test.local":100}}`, nil),
		},
		{
			name:    "create с prev_events",
			event:   ev("m.room.create", "@alice:test.local", ptr(""), `{}`, []string{"$prev"}),
			wantErr: true,
		},
		{
			name:  "create без prev_events",
			event: ev("m.room.create", "@alice:test.local", ptr(""), `{}`, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := auth.Check(tt.event, state)

			if tt.wantErr && err == nil {
				t.Fatalf("ожидали отказ, событие прошло")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("ожидали разрешение, получили %v", err)
			}

			if tt.wantErr && !errors.Is(err, auth.ErrUnauthorized) {
				t.Fatalf("ошибка должна быть ErrUnauthorized, получили %v", err)
			}
		})
	}
}

// TestAuthPublicJoin — в публичной комнате join разрешён без инвайта.
func TestAuthPublicJoin(t *testing.T) {
	t.Parallel()

	state, err := auth.BuildState([]auth.Event{
		ev("m.room.create", "@alice:test.local", ptr(""), `{"creator":"@alice:test.local"}`, nil),
		ev("m.room.join_rules", "@alice:test.local", ptr(""), `{"join_rule":"public"}`, nil),
		ev("m.room.power_levels", "@alice:test.local", ptr(""), `{"users":{"@alice:test.local":100}}`, nil),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if err := auth.Check(ev("m.room.member", "@eve:remote.test", ptr("@eve:remote.test"), `{"membership":"join"}`, nil), state); err != nil {
		t.Fatalf("публичный join должен проходить: %v", err)
	}
}
