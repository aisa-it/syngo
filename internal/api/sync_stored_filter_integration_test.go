//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestSyncStoredFilterAndLeaveState (Complement TestSync,
// TestLeaveEventVisibility): /sync принимает сохраненный фильтр по id и
// применяет timeline/state types; в incremental покинутая комната несет в state
// только то, чего клиент еще не видел.
func TestSyncStoredFilterAndLeaveState(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "sfalice", "ADEV")
	bob := registerHTTP(t, server, "sfbob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/user/@sfbob:test.local/filter",
		`{"room":{"timeline":{"types":["m.room.message"]},"state":{"types":[]}}}`, bob)

	var created struct {
		FilterID string `json:"filter_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.FilterID == "" {
		t.Fatalf("filter: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var room struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &room)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+room.RoomID, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room.RoomID+"/send/m.room.message/m1",
		`{"msgtype":"m.text","body":"hi"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	type roomView struct {
		Timeline struct {
			Events []struct {
				Type string `json:"type"`
			} `json:"events"`
		} `json:"timeline"`
		State struct {
			Events []json.RawMessage `json:"events"`
		} `json:"state"`
	}

	sync := func(token, query string) (join, leave map[string]roomView, next string) {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0"+query, "", token)

		var resp struct {
			NextBatch string `json:"next_batch"`
			Rooms     struct {
				Join  map[string]roomView `json:"join"`
				Leave map[string]roomView `json:"leave"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		return resp.Rooms.Join, resp.Rooms.Leave, resp.NextBatch
	}

	join, _, _ := sync(bob, "&filter="+url.QueryEscape(created.FilterID))

	got := join[room.RoomID]
	if len(got.Timeline.Events) != 1 || got.Timeline.Events[0].Type != "m.room.message" || len(got.State.Events) != 0 {
		t.Fatalf("сохраненный фильтр не применен: %+v", got)
	}

	_, _, aliceSince := sync(alice, "")

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room.RoomID+"/leave", `{}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	_, leave, _ := sync(alice, "&since="+aliceSince)

	left, ok := leave[room.RoomID]
	if !ok || len(left.Timeline.Events) != 1 || len(left.State.Events) != 0 {
		t.Fatalf("incremental leave: timeline %d, state %d (ожидали 1 и 0)", len(left.Timeline.Events), len(left.State.Events))
	}
}
