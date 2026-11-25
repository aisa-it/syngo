package api

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// syncRoomsSince - incremental /sync bob: join-комнаты ответа и next_batch.
func syncRoomsSince(t *testing.T, server *Server, token, since string) ([]string, syncResponse) {
	t.Helper()

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?since="+url.QueryEscape(since), "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	resp := decodeSync(t, rec.Body.Bytes())

	rooms := make([]string, 0, len(resp.Rooms.Join))
	for roomID := range resp.Rooms.Join {
		rooms = append(rooms, roomID)
	}

	sort.Strings(rooms)

	return rooms, resp
}

// TestSyncSkipsQuietRooms - incremental /sync отдает только комнаты с
// изменениями (событие, typing, receipt), тихие пропускает; токен без версии
// эфемерных событий отдает все комнаты, как раньше.
func TestSyncSkipsQuietRooms(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	rooms := make([]string, 0, 3)

	for range 3 {
		roomID := createRoomWith(t, server, alice.AccessToken, `{"preset":"public_chat"}`)
		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", "", bob.AccessToken); rec.Code != http.StatusOK {
			t.Fatalf("join: %d", rec.Code)
		}

		rooms = append(rooms, roomID)
	}

	message := sendMessage(t, server, alice.AccessToken, rooms[0], "q1", `{"msgtype":"m.text","body":"x"}`)

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", bob.AccessToken)
	next := decodeSync(t, rec.Body.Bytes()).NextBatch

	if !strings.Contains(next, ".r") {
		t.Fatalf("next_batch без версии эфемерных событий: %s", next)
	}

	expect := func(name string, want ...string) syncResponse {
		t.Helper()

		got, resp := syncRoomsSince(t, server, bob.AccessToken, next)
		sort.Strings(want)

		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: комнаты %v, ожидали %v", name, got, want)
		}

		next = resp.NextBatch

		return resp
	}

	expect("пустой incremental")

	sendMessage(t, server, alice.AccessToken, rooms[1], "q2", `{"msgtype":"m.text","body":"y"}`)
	expect("сообщение", rooms[1])

	typing := fmt.Sprintf("/_matrix/client/v3/rooms/%s/typing/%s", rooms[2], alice.UserID)
	do(t, server, http.MethodPut, typing, `{"typing":true,"timeout":30000}`, alice.AccessToken)
	expect("typing", rooms[2])

	do(t, server, http.MethodPut, typing, `{"typing":false}`, alice.AccessToken)

	resp := expect("остановка typing", rooms[2])
	if events := resp.Rooms.Join[rooms[2]].Ephemeral.Events; len(events) != 1 || events[0].Type != "m.typing" {
		t.Fatalf("остановка typing: %+v", events)
	}

	do(t, server, http.MethodPost, fmt.Sprintf("/_matrix/client/v3/rooms/%s/receipt/m.read/%s", rooms[0], message),
		"", alice.AccessToken)

	resp = expect("receipt", rooms[0])
	if events := resp.Rooms.Join[rooms[0]].Ephemeral.Events; len(events) != 1 || events[0].Type != "m.receipt" {
		t.Fatalf("receipt: %+v", events)
	}

	expect("снова тихо")

	// Токен старого формата (без версии): все комнаты.
	position, _, err := server.syncTokens.Decode2(next)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	next = server.syncTokens.Encode2(position, 0)
	expect("старый токен", rooms...)

	// Long-poll по таймауту сохраняет версию в токене.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=50&since="+url.QueryEscape(next), "", bob.AccessToken)
	if got := decodeSync(t, rec.Body.Bytes()).NextBatch; got != next {
		t.Fatalf("next_batch по таймауту %s, ожидали %s", got, next)
	}
}

// TestPruneQuietRooms - пропускаются только join-комнаты без изменений.
func TestPruneQuietRooms(t *testing.T) {
	t.Parallel()

	resp := syncResponse{Rooms: emptySyncRooms()}
	resp.Rooms.Join["!quiet"] = syncJoinedRoom{}
	resp.Rooms.Join["!new"] = syncJoinedRoom{}
	resp.Rooms.Join["!timeline"] = syncJoinedRoom{Timeline: syncTimeline{Events: []eventView{{}}}}
	resp.Rooms.Join["!state"] = syncJoinedRoom{State: syncEvents{Events: []eventView{{}}}}
	resp.Rooms.Join["!eph"] = syncJoinedRoom{Ephemeral: ephemeralEvents{Events: []ephemeralEvent{{Type: "m.typing"}}}}
	resp.Rooms.Join["!ad"] = syncJoinedRoom{AccountData: accountDataEvents{Events: []accountDataEvent{{Type: "m.tag"}}}}

	pruneQuietRooms(&resp, map[string]bool{"!new": true})

	if _, ok := resp.Rooms.Join["!quiet"]; ok || len(resp.Rooms.Join) != 5 {
		t.Fatalf("после отсева: %v", resp.Rooms.Join)
	}
}
