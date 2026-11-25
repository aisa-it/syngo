//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestSyncDeviceListsLeft (Complement TestDeviceListUpdates): после
// выхода из последней общей комнаты оба участника получают друг друга в
// device_lists.left; при оставшейся общей комнате - нет.
func TestSyncDeviceListsLeft(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "dlalice", "ADEV")
	bob := registerHTTP(t, server, "dlbob", "BDEV")

	createRoom := func() string {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

		var created struct {
			RoomID string `json:"room_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &created)

		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/join/"+created.RoomID, `{}`, bob); rec.Code != http.StatusOK {
			t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
		}

		return created.RoomID
	}

	sync := func(token, since string) (left []string, next string) {
		t.Helper()

		path := "/_matrix/client/v3/sync?timeout=0"
		if since != "" {
			path += "&since=" + since
		}

		rec := do(t, server, http.MethodGet, path, "", token)

		var resp struct {
			NextBatch   string `json:"next_batch"`
			DeviceLists struct {
				Left []string `json:"left"`
			} `json:"device_lists"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v", err)
		}

		return resp.DeviceLists.Left, resp.NextBatch
	}

	first := createRoom()
	second := createRoom()

	_, aliceSince := sync(alice, "")
	_, bobSince := sync(bob, "")

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+first+"/leave", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("leave first: %d", rec.Code)
	}

	left, aliceSince := sync(alice, aliceSince)
	if slices.Contains(left, "@dlbob:test.local") {
		t.Fatalf("общая комната осталась, а bob в left: %v", left)
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+second+"/leave", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("leave second: %d", rec.Code)
	}

	if left, _ = sync(alice, aliceSince); !slices.Contains(left, "@dlbob:test.local") {
		t.Fatalf("alice: bob не в device_lists.left: %v", left)
	}

	if left, _ = sync(bob, bobSince); !slices.Contains(left, "@dlalice:test.local") {
		t.Fatalf("bob: alice не в device_lists.left: %v", left)
	}
}
