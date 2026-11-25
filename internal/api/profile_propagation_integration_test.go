//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitForCount ждёт, пока число member-событий в комнате не перестанет расти.
func waitForCount(t *testing.T, srv *Server, roomID string, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		var n int
		if err := srv.db.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM events WHERE room_id=$1 AND type='m.room.member' AND state_key='@profbob:test.local'`,
			roomID).Scan(&n); err == nil && n > want {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// waitForLastDisplayName ждёт, пока последнее member-событие не станет ожидаемым.
func waitForLastDisplayName(t *testing.T, srv *Server, roomID, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		var last string
		if err := srv.db.Pool().QueryRow(context.Background(), `
			SELECT content->>'displayname' FROM events
			WHERE room_id=$1 AND type='m.room.member' AND state_key='@profbob:test.local'
			ORDER BY stream_ordering DESC LIMIT 1`, roomID).Scan(&last); err == nil && last == want {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// TestProfilePropagation - MSC4429: смена displayname видна другому участнику.
func TestProfilePropagation(t *testing.T) {
	srv := newIntegrationServer(t)
	alice := registerPusherProbe(t, srv, "profalice", "prof-alice-pass")
	bob := registerPusherProbe(t, srv, "profbob", "prof-bob-pass")

	do := func(token, method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(alice, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"prof","preset":"public_chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	if rec = do(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("bob join: %d", rec.Code)
	}

	// bob меняет displayname.
	if rec = do(bob, http.MethodPut, "/_matrix/client/v3/profile/@profbob:test.local/displayname",
		`{"displayname":"Bobby"}`); rec.Code != http.StatusOK {
		t.Fatalf("set displayname: %d %s", rec.Code, rec.Body.String())
	}

	// Ждём фоновую рассылку первого PUT (polling с дедлайном).
	waitForCount(t, srv, created.RoomID, 1, 3*time.Second)

	// Повторный PUT с тем же значением — no-op, события не добавляются.
	var before int
	if err := srv.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE room_id=$1 AND type='m.room.member' AND state_key='@profbob:test.local'`,
		created.RoomID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}

	if rec = do(bob, http.MethodPut, "/_matrix/client/v3/profile/@profbob:test.local/displayname",
		`{"displayname":"Bobby"}`); rec.Code != http.StatusOK {
		t.Fatalf("no-op put: %d", rec.Code)
	}

	// polling вместо фиксированной паузы.
	waitForCount(t, srv, created.RoomID, before, 500*time.Millisecond)

	var after int
	if err := srv.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE room_id=$1 AND type='m.room.member' AND state_key='@profbob:test.local'`,
		created.RoomID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}

	if after != before {
		t.Fatalf("no-op PUT добавил события: %d → %d", before, after)
	}

	// alice видит m.room.member с новым именем в /sync.
	rec = do(alice, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d", rec.Code)
	}

	var sync struct {
		Rooms struct {
			Join map[string]struct {
				Timeline struct {
					Events []struct {
						Type     string `json:"type"`
						StateKey string `json:"state_key"`
						Content  struct {
							DisplayName string `json:"displayname"`
						} `json:"content"`
					} `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &sync); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	found := false

	for _, ev := range sync.Rooms.Join[created.RoomID].Timeline.Events {
		if ev.Type == "m.room.member" && ev.StateKey == "@profbob:test.local" && ev.Content.DisplayName == "Bobby" {
			found = true
		}
	}

	if !found {
		t.Fatalf("в /sync нет обновлённого displayname: %s", rec.Body.String())
	}

	// Две быстрые смены имени: последним в комнаты должно уехать новейшее.
	if rec = do(bob, http.MethodPut, "/_matrix/client/v3/profile/@profbob:test.local/displayname",
		`{"displayname":"Bobby2"}`); rec.Code != http.StatusOK {
		t.Fatalf("second put: %d", rec.Code)
	}

	if rec = do(bob, http.MethodPut, "/_matrix/client/v3/profile/@profbob:test.local/displayname",
		`{"displayname":"Bobby3"}`); rec.Code != http.StatusOK {
		t.Fatalf("third put: %d", rec.Code)
	}

	waitForLastDisplayName(t, srv, created.RoomID, "Bobby3", 2*time.Second)

	var last string
	if err := srv.db.Pool().QueryRow(context.Background(), `
		SELECT content->>'displayname' FROM events
		WHERE room_id=$1 AND type='m.room.member' AND state_key='@profbob:test.local'
		ORDER BY stream_ordering DESC LIMIT 1`, created.RoomID).Scan(&last); err != nil {
		t.Fatalf("last displayname: %v", err)
	}

	if last != "Bobby3" {
		t.Fatalf("последнее имя в комнате = %q, ожидали Bobby3 (гонка рассылки)", last)
	}
}
