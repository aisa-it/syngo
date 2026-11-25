//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSyncNewlyJoinedRoomHasHistory — комната, в которую пользователь вошел после
// since, приходит в инкрементальном /sync с последними событиями, а не только с
// событием входа. Так Element получает запрос верификации, отправленный
// в новый личный чат до того, как собеседник принял приглашение.
func TestSyncNewlyJoinedRoomHasHistory(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "njalice", "ADEV")
	bob := registerHTTP(t, server, "njbob", "BDEV")

	sync := func(tok, since string) (string, map[string]json.RawMessage) {
		t.Helper()

		path := "/_matrix/client/v3/sync?timeout=0"
		if since != "" {
			path += "&since=" + url.QueryEscape(since)
		}

		rec := do(t, server, http.MethodGet, path, "", tok)

		var resp struct {
			NextBatch string `json:"next_batch"`
			Rooms     struct {
				Join map[string]json.RawMessage `json:"join"`
			} `json:"rooms"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		return resp.NextBatch, resp.Rooms.Join
	}

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"trusted_private_chat","is_direct":true,"invite":["@njbob:test.local"],
		  "initial_state":[{"type":"m.room.history_visibility","state_key":"","content":{"history_visibility":"invited"}}]}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	room := url.PathEscape(created.RoomID)
	bobSince, _ := sync(bob, "")

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/send/m.room.message/req1",
		`{"msgtype":"m.key.verification.request","body":"verify","to":"@njbob:test.local"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	bobSince, _ = sync(bob, bobSince)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	_, joined := sync(bob, bobSince)

	raw, ok := joined[created.RoomID]
	if !ok {
		t.Fatal("комнаты нет в rooms.join после входа")
	}

	if !strings.Contains(string(raw), "m.key.verification.request") {
		t.Fatalf("timeline после входа без сообщения до входа: %s", raw)
	}

	if !strings.Contains(string(raw), `"m.room.create"`) {
		t.Fatalf("после входа нет полного состояния: %s", raw)
	}
}

// TestHistoryVisibilityJoined — в комнате с history_visibility joined новый участник
// не видит сообщений до своего входа ни в /sync, ни в /messages, ни через /event;
// с invited - видит с момента приглашения.
func TestHistoryVisibilityJoined(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "hvalice", "ADEV")
	bob := registerHTTP(t, server, "hvbob", "BDEV")
	carol := registerHTTP(t, server, "hvcarol", "CDEV")

	create := func(visibility string) string {
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
			`{"preset":"public_chat","initial_state":[{"type":"m.room.history_visibility","state_key":"","content":{"history_visibility":"`+visibility+`"}}]}`, alice)

		var created struct {
			RoomID string `json:"room_id"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &created)

		return created.RoomID
	}

	send := func(roomID, txn, body string) string {
		rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/send/m.room.message/"+txn,
			`{"msgtype":"m.text","body":"`+body+`"}`, alice)

		var resp struct {
			EventID string `json:"event_id"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return resp.EventID
	}

	get := func(tok, path string) (int, string) {
		rec := do(t, server, http.MethodGet, path, "", tok)

		return rec.Code, rec.Body.String()
	}

	joined := create("joined")
	secret := send(joined, "j1", "до входа")

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(joined)+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	send(joined, "j2", "после входа")

	_, syncBody := get(bob, "/_matrix/client/v3/sync?timeout=0")
	if strings.Contains(syncBody, "до входа") || !strings.Contains(syncBody, "после входа") || !strings.Contains(syncBody, `"m.room.create"`) {
		t.Fatalf("sync bob (joined): %s", syncBody)
	}

	_, msgs := get(bob, "/_matrix/client/v3/rooms/"+url.PathEscape(joined)+"/messages?dir=b&limit=50")
	if strings.Contains(msgs, "до входа") || !strings.Contains(msgs, "после входа") {
		t.Fatalf("messages bob (joined): %s", msgs)
	}

	if code, _ := get(bob, "/_matrix/client/v3/rooms/"+url.PathEscape(joined)+"/event/"+url.PathEscape(secret)); code != http.StatusNotFound {
		t.Fatalf("event до входа: %d, ожидали 404", code)
	}

	if _, msgs := get(alice, "/_matrix/client/v3/rooms/"+url.PathEscape(joined)+"/messages?dir=b&limit=50"); !strings.Contains(msgs, "до входа") {
		t.Fatalf("создатель не видит свою историю: %s", msgs)
	}

	// invited: сообщение после приглашения видно, до приглашения - нет.
	invited := create("invited")
	send(invited, "i1", "до приглашения")

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(invited)+"/invite",
		`{"user_id":"@hvcarol:test.local"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	send(invited, "i2", "после приглашения")

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+url.PathEscape(invited)+"/join", `{}`, carol); rec.Code != http.StatusOK {
		t.Fatalf("join carol: %d", rec.Code)
	}

	_, msgs = get(carol, "/_matrix/client/v3/rooms/"+url.PathEscape(invited)+"/messages?dir=b&limit=50")
	if strings.Contains(msgs, "до приглашения") || !strings.Contains(msgs, "после приглашения") {
		t.Fatalf("messages carol (invited): %s", msgs)
	}
}
