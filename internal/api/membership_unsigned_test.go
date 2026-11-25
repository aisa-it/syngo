package api

import (
	"encoding/json"
	"testing"
)

// TestAttachSyncMembership - unsigned.membership в timeline /sync: leave до своего
// join и join после, join до своего leave (MSC4115).
func TestAttachSyncMembership(t *testing.T) {
	t.Parallel()

	const bob = "@bob:test.local"

	member := func(membership string) eventView {
		key := bob

		return eventView{Type: "m.room.member", StateKey: &key, Content: json.RawMessage(`{"membership":"` + membership + `"}`)}
	}

	message := eventView{Type: "m.room.message", Content: json.RawMessage(`{}`)}

	resp := syncResponse{Rooms: emptySyncRooms()}
	resp.Rooms.Join["!join"] = syncJoinedRoom{Timeline: syncTimeline{Events: []eventView{message, member("join"), message}}}
	resp.Rooms.Join["!quiet"] = syncJoinedRoom{Timeline: syncTimeline{Events: []eventView{message}}}
	resp.Rooms.Leave["!left"] = syncLeftRoom{Timeline: syncTimeline{Events: []eventView{message, member("leave")}}}

	attachSyncMembership(bob, &resp)

	check := func(room string, events []eventView, want ...string) {
		t.Helper()

		for i := range events {
			if got := events[i].Unsigned["membership"]; got != want[i] {
				t.Fatalf("%s событие %d: membership %v, ожидали %s", room, i, got, want[i])
			}
		}
	}

	check("!join", resp.Rooms.Join["!join"].Timeline.Events, "leave", "join", "join")
	check("!quiet", resp.Rooms.Join["!quiet"].Timeline.Events, "join")
	check("!left", resp.Rooms.Leave["!left"].Timeline.Events, "join", "leave")
}
