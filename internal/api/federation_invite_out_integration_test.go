//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/eventjson"
)

// countersignInvite - ответ сервера приглашенного на /v2/invite: то же событие с
// его подписью рядом с нашей.
func countersignInvite(t *testing.T, env *fedEnv, body []byte) []byte {
	t.Helper()

	var request struct {
		Event map[string]any `json:"event"`
	}

	if err := json.Unmarshal(body, &request); err != nil {
		t.Errorf("invite body: %v", err)

		return nil
	}

	clone := map[string]any{}
	for key, value := range request.Event {
		clone[key] = value
	}

	signed, err := eventjson.SignVersioned(env.remoteKey, "remote.test", clone, 10)
	if err != nil {
		t.Errorf("countersign: %v", err)

		return nil
	}

	theirs := signed["signatures"].(map[string]any)["remote.test"]
	request.Event["signatures"].(map[string]any)["remote.test"] = theirs

	raw, _ := json.Marshal(map[string]any{"event": request.Event})

	return raw
}

// inviteRoom - комната alice, куда приглашается @bob:remote.test.
func inviteRoom(t *testing.T, env *fedEnv) (alice, roomID string) {
	t.Helper()

	alice = registerHTTP(t, env.server, "alice", "ALICE")
	roomID = createFedRoom(t, env.server, alice)

	return alice, roomID
}

// TestOutboundInviteCommitsAfterDelivery - приглашение пользователя другого
// сервера пишется только после успешного /invite, с подписью его сервера.
func TestOutboundInviteCommitsAfterDelivery(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := inviteRoom(t, env)

	newFedMock(t, env, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(r.URL.Path, "/v2/invite/") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(countersignInvite(t, env, body))
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/invite",
		`{"user_id":"@bob:remote.test"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()

	var (
		eventID string
		pdu     string
	)

	if err := env.server.db.Pool().QueryRow(ctx, `
		SELECT e.event_id, e.pdu_json FROM room_state rs JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = 'm.room.member' AND rs.state_key = '@bob:remote.test'`, roomID).
		Scan(&eventID, &pdu); err != nil {
		t.Fatalf("invite event: %v", err)
	}

	var stored struct {
		Signatures map[string]map[string]string `json:"signatures"`
	}

	if err := json.Unmarshal([]byte(pdu), &stored); err != nil {
		t.Fatalf("pdu: %v", err)
	}

	if len(stored.Signatures["remote.test"]) == 0 || len(stored.Signatures["test.local"]) == 0 {
		t.Fatalf("подписи сохраненного invite: %v, ожидали test.local и remote.test", stored.Signatures)
	}
}

// TestOutboundInviteDeliveryFailure - недоставленное приглашение - ошибка
// клиенту, событие не пишется: отказ сервера - 403, недоступность и ответ без
// подписи приглашенного - 502. Раньше событие писалось до доставки, клиент
// получал 200.
func TestOutboundInviteDeliveryFailure(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := inviteRoom(t, env)

	cases := []struct {
		name   string
		user   string
		status int
		reply  func(w http.ResponseWriter, body []byte)
	}{
		{"отказ", "@bob:remote.test", http.StatusForbidden, func(w http.ResponseWriter, _ []byte) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"no"}`))
		}},
		{"без подписи", "@dan:remote.test", http.StatusBadGateway, func(w http.ResponseWriter, body []byte) {
			var request map[string]json.RawMessage
			_ = json.Unmarshal(body, &request)
			_, _ = w.Write([]byte(`{"event":` + string(request["event"]) + `}`))
		}},
		{"недоступен", "@eve:remote.test", http.StatusBadGateway, func(w http.ResponseWriter, _ []byte) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
	}

	for _, tc := range cases {
		mock := newFedMock(t, env, func(w http.ResponseWriter, _ *http.Request, body []byte) { tc.reply(w, body) })

		rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/invite",
			`{"user_id":"`+tc.user+`"}`, alice)
		if rec.Code != tc.status {
			t.Errorf("%s: статус %d, ожидали %d: %s", tc.name, rec.Code, tc.status, rec.Body.String())
		}

		if !strings.Contains(mock.paths(), "/v2/invite/") || strings.Contains(mock.paths(), "/send/") {
			t.Errorf("%s: запросы %s, ожидали только /v2/invite", tc.name, mock.paths())
		}

		if got := membershipRow(t, env, roomID, tc.user); got != "" {
			t.Errorf("%s: недоставленное приглашение записано (membership %q)", tc.name, got)
		}
	}
}

// TestOutboundInviteNotResentToInvitee - приглашение, доставленное через
// /invite, не ставится в outbox серверу приглашенного, даже если там уже есть
// участники комнаты: раньше оно уходило ему повторно через /send, и Synapse
// делал soft-fail («already in the room»). Остальным серверам комнаты - как
// обычно.
func TestOutboundInviteNotResentToInvitee(t *testing.T) {
	env := newFedEnv(t)
	alice, roomID := inviteRoom(t, env)

	setRemoteMember(t, env.server, roomID, "@carol:remote.test", "join")
	setRemoteMember(t, env.server, roomID, "@frank:far.test", "join")

	newFedMock(t, env, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if !strings.Contains(r.URL.Path, "/v2/invite/") {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(countersignInvite(t, env, body))
	})

	rec := do(t, env.server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/invite",
		`{"user_id":"@bob:remote.test"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	var eventID string
	if err := env.server.db.Pool().QueryRow(context.Background(), `
		SELECT event_id FROM room_state
		WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID).
		Scan(&eventID); err != nil {
		t.Fatalf("invite event: %v", err)
	}

	if got := outboxFor(t, env.server, eventID); len(got) != 1 || got[0] != "far.test" {
		t.Fatalf("получатели invite в outbox: %v, ожидали [far.test]", got)
	}
}
