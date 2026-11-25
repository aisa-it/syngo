//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
)

// inviteEvent - приглашение target от @bob:remote.test в комнату roomID.
func (e *fedEnv) inviteEvent(t *testing.T, roomID, target string, depth int64) (map[string]any, string) {
	t.Helper()

	return e.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr(target),
		map[string]any{"membership": "invite"}, []string{"$create:remote.test"}, nil, depth)
}

// membershipOfUser - membership пользователя в room_memberships ("" - нет строки).
func membershipOfUser(t *testing.T, env *fedEnv, roomID, userID string) string {
	t.Helper()

	var membership string

	_ = env.server.db.Pool().QueryRow(context.Background(),
		"SELECT membership FROM room_memberships WHERE room_id=$1 AND user_id=$2", roomID, userID).Scan(&membership)

	return membership
}

// TestFederationInviteIncomingWrapper — входящий invite по спеке
// принимает {room_version, event}, а голое событие — только для совместимости.
// Ответ - событие с нашей подписью.
func TestFederationInviteIncomingWrapper(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!invite-in:remote.test"

	signed, eventID := env.inviteEvent(t, roomID, "@carol:test.local", 4)
	path := "/_matrix/federation/v1/invite/" + roomID + "/" + eventID

	// 1. Спека: {room_version, event}.
	rec := env.request(t, http.MethodPut, path, map[string]any{"room_version": "10", "event": signed})
	if rec.Code != http.StatusOK {
		t.Fatalf("invite в обёртке: status %d, body %s", rec.Code, rec.Body.String())
	}

	if got := membershipOfUser(t, env, roomID, "@carol:test.local"); got != "invite" {
		t.Fatalf("membership = %q, ожидали invite", got)
	}

	// v1 отвечает [200, {"event": ...}], событие подписано и отправителем, и нами.
	var reply []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply) != 2 {
		t.Fatalf("ответ v1: %s", rec.Body.String())
	}

	var body struct {
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal(reply[1], &body); err != nil {
		t.Fatalf("ответ v1: %v", err)
	}

	lookup := env.server.federationKeyLookup(context.Background())
	for _, server := range []string{"test.local", "remote.test"} {
		if err := eventjson.VerifyVersioned(body.Event, server, core.DefaultRoomVersion, lookup); err != nil {
			t.Fatalf("подпись %s в ответе: %v (%s)", server, err, rec.Body.String())
		}
	}

	// 2. Голое событие (совместимость с прежними нашими серверами).
	bare, bareID := env.inviteEvent(t, roomID, "@dave:test.local", 5)

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v1/invite/"+roomID+"/"+bareID, bare)
	if rec.Code != http.StatusOK {
		t.Fatalf("голое событие: status %d, body %s", rec.Code, rec.Body.String())
	}

	// 3. Неизвестная версия комнаты — 400.
	rec = env.request(t, http.MethodPut, path, map[string]any{"room_version": "99", "event": signed})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестная версия: status %d, body %s", rec.Code, rec.Body.String())
	}

	// 4. Событие без подписи — отказ (403).
	tampered, tamperedID := env.inviteEvent(t, roomID, "@eve:test.local", 6)
	delete(tampered, "signatures")

	rec = env.request(t, http.MethodPut, "/_matrix/federation/v1/invite/"+roomID+"/"+tamperedID,
		map[string]any{"room_version": "10", "event": tampered})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("без подписи: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestFederationInviteV2 — Synapse 1.165 шлёт приглашение в v2; эндпоинт
// должен принимать то же тело {room_version, event}, иначе createRoom с нашим
// пользователем падает с «User's homeserver does not support this room version».
func TestFederationInviteV2(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!invite-v2:remote.test"

	member, eventID := env.inviteEvent(t, roomID, "@carol:test.local", 4)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/invite/"+roomID+"/"+eventID,
		map[string]any{"room_version": "10", "event": member})
	if rec.Code != http.StatusOK {
		t.Fatalf("v2 invite: status %d, body %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ v2: %v", err)
	}

	if _, has := body.Event["event_id"]; has {
		t.Fatalf("event_id в ответе v2 (room v3+ запрещает): %s", rec.Body.String())
	}

	if err := eventjson.VerifyVersioned(body.Event, "test.local", core.DefaultRoomVersion,
		env.server.federationKeyLookup(context.Background())); err != nil {
		t.Fatalf("ответ v2 без нашей подписи: %v", err)
	}
}

// TestFederationInviteIncomingChecks — /invite принимает только
// m.room.member invite нашему пользователю от сервера отправителя; ничего, кроме
// этого приглашения, в состояние чужой комнаты не пишется.
func TestFederationInviteIncomingChecks(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!invite-checks:remote.test"

	ctx := context.Background()

	put := func(t *testing.T, object map[string]any, eventID string) int {
		t.Helper()

		rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/invite/"+roomID+"/"+eventID,
			map[string]any{"room_version": "10", "event": object})

		return rec.Code
	}

	cases := []struct {
		name     string
		sender   string
		typ      string
		stateKey string
		content  map[string]any
		want     int
	}{
		{
			"power_levels с membership", "@bob:remote.test", "m.room.power_levels", "",
			map[string]any{"membership": "invite", "users": map[string]any{"@bob:remote.test": 100}},
			http.StatusBadRequest,
		},
		{
			"membership join", "@bob:remote.test", "m.room.member", "@carol:test.local",
			map[string]any{"membership": "join"},
			http.StatusBadRequest,
		},
		{
			"membership ban", "@bob:remote.test", "m.room.member", "@carol:test.local",
			map[string]any{"membership": "ban"},
			http.StatusBadRequest,
		},
		{
			"нелокальный state_key", "@bob:remote.test", "m.room.member", "@zed:remote.test",
			map[string]any{"membership": "invite"},
			http.StatusBadRequest,
		},
		{
			"origin не из домена sender", "@eve:other.test", "m.room.member", "@carol:test.local",
			map[string]any{"membership": "invite"},
			http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		object, eventID := env.event(t, tc.sender, roomID, tc.typ, strPtr(tc.stateKey), tc.content,
			[]string{"$create:remote.test"}, nil, 4)

		if code := put(t, object, eventID); code != tc.want {
			t.Fatalf("%s: status %d, ожидали %d", tc.name, code, tc.want)
		}
	}

	// event_id пути не совпадает с содержимым - 400.
	object, _ := env.inviteEvent(t, roomID, "@carol:test.local", 4)
	if code := put(t, object, "$forged:remote.test"); code != http.StatusBadRequest {
		t.Fatalf("event_id пути: status %d", code)
	}

	var stateRows int
	if err := env.server.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM room_state WHERE room_id = $1`, roomID).Scan(&stateRows); err != nil {
		t.Fatal(err)
	}

	if stateRows != 0 {
		t.Fatalf("отклоненные приглашения записаны в room_state: %d строк", stateRows)
	}
}

// TestFederationInviteIntoOurRoom — в комнате, где мы участник,
// приглашение авторизуется по текущему состоянию: не-участник пригласить не
// может, участник - может.
func TestFederationInviteIntoOurRoom(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!invite-ours:test.local"
	env.seedRoom(t, roomID)

	// bob только приглашен - приглашать других не вправе.
	invite, inviteID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@carol:test.local"),
		map[string]any{"membership": "invite"}, []string{"$create:test.local", "$pl:test.local", "$invite:test.local"},
		[]string{"$invite:test.local"}, 4)

	path := "/_matrix/federation/v2/invite/" + roomID + "/" + inviteID

	rec := env.request(t, http.MethodPut, path, map[string]any{"room_version": "10", "event": invite})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("приглашение от не-участника: %d %s, ожидали 403", rec.Code, rec.Body.String())
	}

	if got := membershipOfUser(t, env, roomID, "@carol:test.local"); got != "" {
		t.Fatalf("отклоненное приглашение записано: %q", got)
	}

	// bob вошел - теперь приглашение проходит.
	setRemoteMember(t, env.server, roomID, "@bob:remote.test", "join")

	rec = env.request(t, http.MethodPut, path, map[string]any{"room_version": "10", "event": invite})
	if rec.Code != http.StatusOK {
		t.Fatalf("приглашение от участника: %d %s", rec.Code, rec.Body.String())
	}

	if got := membershipOfUser(t, env, roomID, "@carol:test.local"); got != "invite" {
		t.Fatalf("membership = %q, ожидали invite", got)
	}
}
