package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestStateFormatEventAndR0 - GET state с format=event отдает событие целиком, а
// префикс r0 обслуживается как v3 (Complement TestRoomState).
func TestStateFormatEventAndR0(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "1234567890")

	rec := do(t, server, http.MethodPost, "/_matrix/client/r0/createRoom", `{}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom через r0: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/rooms/"+created.RoomID+"/state/m.room.member/"+alice.UserID+"?format=event", "", alice.AccessToken)

	var event struct {
		Sender   string          `json:"sender"`
		RoomID   string          `json:"room_id"`
		EventID  string          `json:"event_id"`
		Content  json.RawMessage `json:"content"`
		StateKey *string         `json:"state_key"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &event); err != nil || event.Sender != alice.UserID ||
		event.RoomID != created.RoomID || event.EventID == "" || event.StateKey == nil || len(event.Content) == 0 {
		t.Fatalf("format=event: %d %s", rec.Code, rec.Body.String())
	}
}
