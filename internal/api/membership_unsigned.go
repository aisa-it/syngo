package api

import "encoding/json"

// attachSyncMembership кладет unsigned.membership (MSC4115, спека v1.11) в события
// timeline /sync: членство запрашивающего в момент события - leave до его входа,
// join после (свое m.room.member - уже с новым членством), как в Synapse
// (Complement TestMembershipOnEvents).
//
// Членство до первого события timeline выводится из первого своего m.room.member
// в timeline: перед join (invite, knock) пользователь не состоял (leave), перед
// leave или ban - состоял (join). Своего m.room.member в timeline нет - членство
// не менялось и равно секции ответа (join или leave).
func attachSyncMembership(userID string, resp *syncResponse) {
	for roomID := range resp.Rooms.Join {
		fillMembership(userID, "join", resp.Rooms.Join[roomID].Timeline.Events)
	}

	for roomID := range resp.Rooms.Leave {
		fillMembership(userID, "leave", resp.Rooms.Leave[roomID].Timeline.Events)
	}
}

func fillMembership(userID, current string, timeline []eventView) {
	membership := current

	for i := range timeline {
		if first := ownMembership(userID, timeline[i]); first != "" {
			membership = "leave"
			if first == "leave" || first == "ban" {
				membership = "join"
			}

			break
		}
	}

	for i := range timeline {
		if m := ownMembership(userID, timeline[i]); m != "" {
			membership = m
		}

		if timeline[i].Unsigned == nil {
			timeline[i].Unsigned = map[string]any{}
		}

		timeline[i].Unsigned["membership"] = membership
	}
}

// ownMembership - membership из m.room.member самого пользователя, иначе "".
func ownMembership(userID string, event eventView) string {
	if event.Type != "m.room.member" || event.StateKey == nil || *event.StateKey != userID {
		return ""
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if json.Unmarshal(event.Content, &content) != nil {
		return ""
	}

	return content.Membership
}
