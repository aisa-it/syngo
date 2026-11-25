package auth

import "testing"

// TestThirdPartyInviteAndRedactionLevels — спека v10/v11: событие
// m.room.third_party_invite требует invite-уровня, m.room.redaction — redact.
func TestThirdPartyInviteAndRedactionLevels(t *testing.T) {
	t.Parallel()

	invite25, redact50 := 25, 50

	state := State{
		Create:  &Event{Type: "m.room.create", Sender: "@admin:test", Content: []byte(`{"creator":"@admin:test","m.federate":true}`)},
		Members: map[string]string{"@low:test": "join", "@admin:test": "join"},
		PowerLevels: &powerLevels{
			Users:  map[string]int{"@admin:test": 100},
			Invite: &invite25, Redact: &redact50,
		},
	}

	tpi := Event{
		Type: "m.room.third_party_invite", RoomID: "!r:test", Sender: "@low:test",
		StateKey: strPtr("token"), Content: []byte(`{"display_name":"x"}`),
	}

	if err := Check(tpi, state); err == nil {
		t.Fatal("third_party_invite с уровнем 0 не должен проходить при invite=25")
	}

	redaction := Event{
		Type: "m.room.redaction", RoomID: "!r:test", Sender: "@low:test",
		Content: []byte(`{"redacts":"$e"}`),
	}

	if err := Check(redaction, state); err == nil {
		t.Fatal("redaction с уровнем 0 не должен проходить при redact=50")
	}
}
