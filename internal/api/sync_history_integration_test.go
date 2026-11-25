//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

type histEvent struct {
	Type    string         `json:"type"`
	Content map[string]any `json:"content"`
}

func histBodies(events []histEvent) []string {
	var out []string

	for _, e := range events {
		if e.Type == "m.room.message" {
			out = append(out, fmt.Sprint(e.Content["body"]))
		}
	}

	return out
}

// TestSyncTimelineLatestAndBackfill — сценарий Element:
// initial sync с лимитом отдает ПОСЛЕДНИЕ события и limited=true, подгрузка назад
// по prev_batch через /messages отдает остальные без пропусков и дублей; то же
// для инкрементального sync при большем числе новых событий, чем лимит.
func TestSyncTimelineLatestAndBackfill(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ADEV")
	bob := registerHTTP(t, server, "bob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"h","invite":["@bob:test.local"]}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	room := url.PathEscape(created.RoomID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	send := func(from, to int) {
		for i := from; i < to; i++ {
			body := fmt.Sprintf(`{"msgtype":"m.text","body":"h%02d"}`, i)
			if rec := do(t, server, http.MethodPut, fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/t%d", room, i), body, alice); rec.Code != http.StatusOK {
				t.Fatalf("send %d: %d", i, rec.Code)
			}
		}
	}

	type syncResp struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events    []histEvent `json:"events"`
					Limited   bool        `json:"limited"`
					PrevBatch string      `json:"prev_batch"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}

	filter := url.QueryEscape(`{"room":{"timeline":{"limit":20}}}`)
	doSync := func(since string) syncResp {
		path := "/_matrix/client/v3/sync?filter=" + filter
		if since != "" {
			path += "&since=" + url.QueryEscape(since)
		}

		var resp syncResp

		rec := do(t, server, http.MethodGet, path, "", bob)
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v %s", err, rec.Body.String())
		}

		return resp
	}

	backfill := func(from, stopAt string) []string {
		var got []string

		for page := 0; page < 20 && from != ""; page++ {
			rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/messages?dir=b&limit=7&from="+url.QueryEscape(from), "", bob)

			var m struct {
				Chunk []histEvent `json:"chunk"`
				End   string      `json:"end"`
			}

			_ = json.Unmarshal(rec.Body.Bytes(), &m)

			for _, body := range histBodies(m.Chunk) {
				if body == stopAt {
					return got
				}

				got = append([]string{body}, got...)
			}

			if len(m.Chunk) == 0 || m.End == from {
				break
			}

			from = m.End
		}

		return got
	}

	send(0, 45)

	first := doSync("")
	tl := first.Rooms.Join[created.RoomID].Timeline
	bodies := histBodies(tl.Events)

	if len(bodies) == 0 || bodies[len(bodies)-1] != "h44" || !tl.Limited || tl.PrevBatch == "" {
		t.Fatalf("initial sync: последнее=%v limited=%v prev_batch=%q (ожидали последние события)", bodies, tl.Limited, tl.PrevBatch)
	}

	older := backfill(tl.PrevBatch, "")
	all := append(older, bodies...)

	for i := 0; i < 45; i++ {
		if i >= len(all) || all[i] != fmt.Sprintf("h%02d", i) {
			t.Fatalf("история после подгрузки назад: %v", all)
		}
	}

	// Инкремент: 30 новых при лимите 20 - последние 20, limited, догрузка 10 назад.
	send(45, 75)

	next := doSync(first.NextBatch)
	tl = next.Rooms.Join[created.RoomID].Timeline
	bodies = histBodies(tl.Events)

	if len(bodies) != 20 || bodies[19] != "h74" || bodies[0] != "h55" || !tl.Limited {
		t.Fatalf("incremental sync: %v limited=%v", bodies, tl.Limited)
	}

	gap := backfill(tl.PrevBatch, "h44")
	if len(gap) != 10 || gap[0] != "h45" || gap[9] != "h54" {
		t.Fatalf("догрузка дыры инкремента: %v", gap)
	}
}
