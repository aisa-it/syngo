package auth

import "testing"

// TestInviteAlreadyInvitedIsIdempotent — повторный invite уже приглашённого
// разрешён безусловно (как в Synapse), даже если уровень отправителя ниже
// invite.
func TestInviteAlreadyInvitedIsIdempotent(t *testing.T) {
	t.Parallel()

	invite50 := 50

	state := State{
		JoinRules: "invite",
		Members:   map[string]string{"@admin:test": "join", "@eve:test": "join", "@bob:test": "invite"},
	}

	levels := &powerLevels{Invite: &invite50, Users: map[string]int{"@eve:test": 0}}

	ev := Event{
		Type: "m.room.member", Sender: "@eve:test", StateKey: strPtr("@bob:test"),
		Content: []byte(`{"membership":"invite"}`),
	}

	if err := checkMembership(ev, state, levels, 0); err != nil {
		t.Fatalf("повторный invite уже приглашённого отклонён: %v", err)
	}
}
