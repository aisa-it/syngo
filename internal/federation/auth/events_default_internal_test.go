package auth

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestCheckEnforcesEventsDefault — обычные события подчиняются events_default,
// иначе понижённый участник может писать в комнату.
func TestCheckEnforcesEventsDefault(t *testing.T) {
	t.Parallel()

	createContent := json.RawMessage(`{"creator":"@alice:test.local"}`)
	plContent := json.RawMessage(`{"users":{"@alice:test.local":100},"events_default":50}`)
	emptyKey := ""

	state, err := BuildState([]Event{
		{EventID: "$c", RoomID: "!r:test.local", Type: "m.room.create", Sender: "@alice:test.local", StateKey: &emptyKey, Content: createContent},
		{EventID: "$pl", RoomID: "!r:test.local", Type: "m.room.power_levels", Sender: "@alice:test.local", StateKey: &emptyKey, Content: plContent},
		{EventID: "$m", RoomID: "!r:test.local", Type: "m.room.member", Sender: "@bob:test.local", StateKey: strPtr("@bob:test.local"), Content: json.RawMessage(`{"membership":"join"}`)},
	})
	if err != nil {
		t.Fatalf("BuildState: %v", err)
	}

	err = Check(Event{
		RoomID: "!r:test.local", Type: "m.room.message", Sender: "@bob:test.local",
		Content: json.RawMessage(`{"msgtype":"m.text","body":"hi"}`),
	}, state)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ожидали ErrUnauthorized для events_default=50, получили %v", err)
	}
}

func strPtr(v string) *string { return &v }
