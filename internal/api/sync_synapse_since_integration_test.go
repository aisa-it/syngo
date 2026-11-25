//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// synapseSince - since-токен Synapse, с которым клиент приходит после переезда.
const synapseSince = "s72594_4483_1934_0_1_1_1_1_0_1"

type ssinceEvent struct {
	Type     string         `json:"type"`
	Sender   string         `json:"sender"`
	StateKey *string        `json:"state_key"`
	Content  map[string]any `json:"content"`
}

type ssinceResp struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events    []ssinceEvent `json:"events"`
				Limited   bool          `json:"limited"`
				PrevBatch string        `json:"prev_batch"`
			} `json:"timeline"`
			State struct {
				Events []ssinceEvent `json:"events"`
			} `json:"state"`
		} `json:"join"`
		Invite map[string]json.RawMessage `json:"invite"`
		Leave  map[string]struct {
			Timeline struct {
				Events  []ssinceEvent `json:"events"`
				Limited bool          `json:"limited"`
			} `json:"timeline"`
			State struct {
				Events []ssinceEvent `json:"events"`
			} `json:"state"`
		} `json:"leave"`
	} `json:"rooms"`
}

// hasState - в state есть событие типа с state_key.
func hasState(events []ssinceEvent, eventType, stateKey string) bool {
	for i := range events {
		if events[i].Type == eventType && events[i].StateKey != nil && *events[i].StateKey == stateKey {
			return true
		}
	}

	return false
}

// TestSyncSynapseSinceIsGap - since Synapse после переезда: ответ без
// событий timeline, с limited и prev_batch, текущим состоянием комнат целиком,
// приглашениями и выходами; next_batch нашего формата, следующий incremental -
// обычный. Раньше такой запрос обслуживался как initial sync с последними
// сообщениями, и боты отвечали на старые сообщения как на новые.
func TestSyncSynapseSinceIsGap(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "ssyalice", "ADEV")
	bob := registerHTTP(t, server, "ssybob", "BDEV")

	createRoom := func(body string) string {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", body, alice)

		var created struct {
			RoomID string `json:"room_id"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
			t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
		}

		return created.RoomID
	}

	post := func(tok, path, body string) {
		t.Helper()

		if rec := do(t, server, http.MethodPost, path, body, tok); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	send := func(roomID, txn, body string) {
		t.Helper()

		if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/send/m.room.message/"+txn,
			`{"msgtype":"m.text","body":"`+body+`"}`, alice); rec.Code != http.StatusOK {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
	}

	sync := func(since, filter string) (ssinceResp, string) {
		t.Helper()

		path := "/_matrix/client/v3/sync?timeout=0&since=" + url.QueryEscape(since)
		if filter != "" {
			path += "&filter=" + url.QueryEscape(filter)
		}

		rec := do(t, server, http.MethodGet, path, "", bob)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		var resp ssinceResp
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %v %s", err, rec.Body.String())
		}

		return resp, rec.Body.String()
	}

	room := createRoom(`{"preset":"public_chat","name":"общая"}`)
	post(bob, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/join", `{}`)

	for i, body := range []string{"старое-1", "старое-2", "старое-3"} {
		send(room, "old"+string(rune('a'+i)), body)
	}

	left := createRoom(`{"preset":"public_chat","name":"покинутая"}`)
	post(bob, "/_matrix/client/v3/rooms/"+url.PathEscape(left)+"/join", `{}`)
	send(left, "leftold", "старое-в-покинутой")
	post(bob, "/_matrix/client/v3/rooms/"+url.PathEscape(left)+"/leave", `{}`)

	invited := createRoom(`{"preset":"private_chat","name":"приглашение"}`)
	post(alice, "/_matrix/client/v3/rooms/"+url.PathEscape(invited)+"/invite", `{"user_id":"@ssybob:test.local"}`)

	for _, tc := range []struct {
		name, txn, filter string
		lazy              bool
	}{
		{name: "без фильтра", txn: "plain"},
		{name: "lazy_load_members", txn: "lazy", filter: `{"room":{"state":{"lazy_load_members":true}}}`, lazy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := sync(synapseSince, tc.filter)

			if strings.Contains(raw, "старое") {
				t.Fatalf("старые сообщения в ответе на since Synapse: %s", raw)
			}

			joined, ok := resp.Rooms.Join[room]
			if !ok {
				t.Fatalf("комнаты нет в rooms.join: %s", raw)
			}

			if len(joined.Timeline.Events) != 0 || !joined.Timeline.Limited || joined.Timeline.PrevBatch == "" {
				t.Fatalf("timeline: ожидали пустой, limited, prev_batch; %+v", joined.Timeline)
			}

			state := joined.State.Events
			for _, need := range [][2]string{
				{"m.room.create", ""}, {"m.room.name", ""}, {"m.room.power_levels", ""},
				{"m.room.join_rules", ""}, {"m.room.member", "@ssybob:test.local"},
			} {
				if !hasState(state, need[0], need[1]) {
					t.Fatalf("в state нет %s/%s: %s", need[0], need[1], raw)
				}
			}

			// Участники: без lazy - все, с lazy - только свое членство (timeline
			// пуст, отправителей нет; остальных клиент берет из summary и /members).
			if hasState(state, "m.room.member", "@ssyalice:test.local") == tc.lazy {
				t.Fatalf("членство alice в state (lazy=%v): %s", tc.lazy, raw)
			}

			if _, ok := resp.Rooms.Invite[invited]; !ok {
				t.Fatalf("приглашения нет в rooms.invite: %s", raw)
			}

			leftRoom, ok := resp.Rooms.Leave[left]
			if !ok {
				t.Fatalf("покинутой комнаты нет в rooms.leave: %s", raw)
			}

			if len(leftRoom.Timeline.Events) != 0 || !leftRoom.Timeline.Limited ||
				!hasState(leftRoom.State.Events, "m.room.member", "@ssybob:test.local") {
				t.Fatalf("покинутая комната: %+v", leftRoom)
			}

			if !strings.HasPrefix(resp.NextBatch, "v1.") {
				t.Fatalf("next_batch не нашего формата: %q", resp.NextBatch)
			}

			// История - через /messages от prev_batch.
			rec := do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+
				"/messages?dir=b&limit=10&from="+url.QueryEscape(joined.Timeline.PrevBatch), "", bob)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "старое-3") ||
				!strings.Contains(rec.Body.String(), "старое-1") {
				t.Fatalf("/messages от prev_batch: %d %s", rec.Code, rec.Body.String())
			}

			// Следующий incremental - обычный: только новое сообщение.
			send(room, "new-"+tc.txn, "новое-"+tc.txn)

			next, raw := sync(resp.NextBatch, tc.filter)

			nextRoom, ok := next.Rooms.Join[room]
			if !ok || len(nextRoom.Timeline.Events) != 1 || nextRoom.Timeline.Limited ||
				nextRoom.Timeline.Events[0].Content["body"] != "новое-"+tc.txn {
				t.Fatalf("incremental после since Synapse: %s", raw)
			}

			if strings.Contains(raw, "старое") {
				t.Fatalf("старые сообщения в incremental: %s", raw)
			}
		})
	}
}
