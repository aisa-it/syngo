//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestSyncRoomSummaryLazyLoad - при lazy_load_members join-комнаты в
// /sync несут summary (m.heroes, m.joined_member_count, m.invited_member_count),
// как Synapse: по нему клиент называет комнату без полного списка участников.
func TestSyncRoomSummaryLazyLoad(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "smalice", "ADEV")
	bob := registerHTTP(t, server, "smbob", "BDEV")
	registerHTTP(t, server, "smcarol", "CDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","invite":["@smcarol:test.local"]}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	sync := func(filter string) map[string]json.RawMessage {
		t.Helper()

		path := "/_matrix/client/v3/sync?timeout=0"
		if filter != "" {
			path += "&filter=" + url.QueryEscape(filter)
		}

		rec := do(t, server, http.MethodGet, path, "", alice)

		var resp struct {
			Rooms struct {
				Join map[string]map[string]json.RawMessage `json:"join"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v", err)
		}

		return resp.Rooms.Join[created.RoomID]
	}

	room := sync(`{"room":{"state":{"lazy_load_members":true}}}`)

	var summary struct {
		Heroes  []string `json:"m.heroes"`
		Joined  int      `json:"m.joined_member_count"`
		Invited int      `json:"m.invited_member_count"`
	}

	if err := json.Unmarshal(room["summary"], &summary); err != nil || summary.Joined != 2 || summary.Invited != 1 ||
		len(summary.Heroes) != 2 || summary.Heroes[0] != "@smbob:test.local" || summary.Heroes[1] != "@smcarol:test.local" {
		t.Fatalf("summary: %s", room["summary"])
	}

	if _, ok := sync("")["summary"]; ok {
		t.Fatal("summary без lazy_load_members не ожидается")
	}
}

// TestSyncRoomSummaryOnlyOnMembershipChange - incremental с
// lazy_load_members несет summary только при изменении членства, как Synapse.
// Члены отправителей, которые lazy-loading кладет в state, изменением не
// считаются: раньше сводка по всем участникам считалась на каждое сообщение.
func TestSyncRoomSummaryOnlyOnMembershipChange(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "soalice", "ADEV")
	bob := registerHTTP(t, server, "sobob", "BDEV")
	carol := registerHTTP(t, server, "socarol", "CDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	filter := url.QueryEscape(`{"room":{"state":{"lazy_load_members":true}}}`)

	sync := func(since string) (map[string]json.RawMessage, string) {
		t.Helper()

		path := "/_matrix/client/v3/sync?timeout=0&filter=" + filter
		if since != "" {
			path += "&since=" + url.QueryEscape(since)
		}

		rec := do(t, server, http.MethodGet, path, "", alice)

		var resp struct {
			NextBatch string `json:"next_batch"`
			Rooms     struct {
				Join map[string]map[string]json.RawMessage `json:"join"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v", err)
		}

		return resp.Rooms.Join[created.RoomID], resp.NextBatch
	}

	room, next := sync("")
	if _, ok := room["summary"]; !ok {
		t.Fatal("initial sync без summary")
	}

	path := "/_matrix/client/v3/rooms/" + created.RoomID + "/send/m.room.message/so1"
	if rec := do(t, server, http.MethodPut, path, `{"msgtype":"m.text","body":"x"}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	room, next = sync(next)
	if room == nil {
		t.Fatal("комната с сообщением не пришла")
	}

	if raw, ok := room["summary"]; ok {
		t.Fatalf("summary на обычное сообщение: %s", raw)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, carol); rec.Code != http.StatusOK {
		t.Fatalf("join carol: %d %s", rec.Code, rec.Body.String())
	}

	room, _ = sync(next)

	var summary struct {
		Joined int `json:"m.joined_member_count"`
	}
	if err := json.Unmarshal(room["summary"], &summary); err != nil || summary.Joined != 3 {
		t.Fatalf("summary после входа: %s", room["summary"])
	}
}
