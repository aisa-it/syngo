//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoomUpgradeIntegration - на PostgreSQL: upgrade публичной комнаты
// переносит публикацию в каталоге и локальный алиас, пишет tombstone и predecessor;
// повторный upgrade отдает ту же замену.
func TestRoomUpgradeIntegration(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "upgrader", "upgrader-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"name":"old","preset":"public_chat","visibility":"public","room_alias_name":"upgrade-int"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	upgrade := func() string {
		rec := do(http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/upgrade", `{"new_version":"11"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("upgrade: %d %s", rec.Code, rec.Body.String())
		}

		var resp upgradeResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return resp.ReplacementRoom
	}

	newRoomID := upgrade()
	if newRoomID == "" || newRoomID == created.RoomID {
		t.Fatalf("replacement_room = %q", newRoomID)
	}

	if again := upgrade(); again != newRoomID {
		t.Fatalf("повторный upgrade: %s, want %s", again, newRoomID)
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/rooms/"+newRoomID+"/state/m.room.create/", "")
	if !strings.Contains(rec.Body.String(), `"predecessor"`) || !strings.Contains(rec.Body.String(), created.RoomID) {
		t.Fatalf("create без predecessor: %s", rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/rooms/"+created.RoomID+"/state/m.room.tombstone/", "")
	if !strings.Contains(rec.Body.String(), newRoomID) {
		t.Fatalf("tombstone: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/directory/room/%23upgrade-int:test.local", "")
	if !strings.Contains(rec.Body.String(), newRoomID) {
		t.Fatalf("алиас не перенесен: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/publicRooms?limit=1000", "")

	var catalog struct {
		Chunk []struct {
			RoomID string `json:"room_id"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("publicRooms: %d %s", rec.Code, rec.Body.String())
	}

	published := map[string]bool{}
	for _, room := range catalog.Chunk {
		published[room.RoomID] = true
	}

	if published[created.RoomID] || !published[newRoomID] {
		t.Fatalf("каталог: old=%v new=%v", published[created.RoomID], published[newRoomID])
	}
}
