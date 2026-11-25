//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// setRemoteMember пишет членство удаленного пользователя в состояние комнаты
// (как после /send или send_join): событие, room_state, room_memberships.
func setRemoteMember(t *testing.T, server *Server, roomID, userID, membership string) {
	t.Helper()

	ctx := context.Background()
	eventID := "$" + membership + "-" + strings.TrimPrefix(userID, "@")
	domain := domainOfID(userID)

	if _, err := server.db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		VALUES ($1, $2, 'm.room.member', $3, $3, jsonb_build_object('membership', $4::text), 50, 1, $5)`,
		eventID, roomID, userID, membership, domain); err != nil {
		t.Fatalf("remote member event: %v", err)
	}

	if _, err := server.db.Pool().Exec(ctx, `
		INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1, 'm.room.member', $2, $3)
		ON CONFLICT (room_id, type, state_key) DO UPDATE SET event_id = EXCLUDED.event_id`,
		roomID, userID, eventID); err != nil {
		t.Fatalf("remote member state: %v", err)
	}

	if _, err := server.db.Pool().Exec(ctx, `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership) VALUES ($1, $2, $3, $3, $4)
		ON CONFLICT (room_id, user_id) DO UPDATE SET event_id = EXCLUDED.event_id, membership = EXCLUDED.membership`,
		eventID, roomID, userID, membership); err != nil {
		t.Fatalf("remote membership: %v", err)
	}
}

// outboxFor возвращает получателей события в federation_outbox.
func outboxFor(t *testing.T, server *Server, eventID string) []string {
	t.Helper()

	rows, err := server.db.Pool().Query(context.Background(),
		`SELECT destination FROM federation_outbox WHERE event_id = $1 ORDER BY destination`, eventID)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			t.Fatalf("scan: %v", err)
		}

		out = append(out, destination)
	}

	return out
}

// destinationRoomsOf — серверы комнаты в destination_rooms.
func destinationRoomsOf(t *testing.T, server *Server, roomID string) []string {
	t.Helper()

	rows, err := server.db.Pool().Query(context.Background(),
		`SELECT destination FROM destination_rooms WHERE room_id = $1 ORDER BY destination`, roomID)
	if err != nil {
		t.Fatalf("destination_rooms: %v", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var destination string
		if err := rows.Scan(&destination); err != nil {
			t.Fatalf("scan: %v", err)
		}

		out = append(out, destination)
	}

	return out
}

// createFedRoom создает публичную комнату alice и возвращает ее id.
func createFedRoom(t *testing.T, server *Server, alice string) string {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"name":"fed","preset":"public_chat","visibility":"public"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	return created.RoomID
}

// sendText отправляет сообщение и возвращает event_id.
func sendText(t *testing.T, server *Server, token, roomID, txnID, body string) string {
	t.Helper()

	rec := do(t, server, http.MethodPut,
		"/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/"+txnID,
		`{"msgtype":"m.text","body":"`+body+`"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		EventID string `json:"event_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	return resp.EventID
}

// TestFederationEnqueueSurvivesClientAbort — событие ставится в outbox в
// транзакции записи: обработчик, оборванный сразу после записи
// события (клиент закрыл соединение), рассылку не теряет. Раньше постановка
// шла после коммита на контексте запроса и при обрыве не происходила.
func TestFederationEnqueueSurvivesClientAbort(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")

	ctx, abort := context.WithCancel(context.Background())

	eventID, _, err := server.rooms.SendMessage(ctx, "@alice:test.local", "DEV", roomID,
		"m.room.message", "txn-abort", json.RawMessage(`{"msgtype":"m.text","body":"до обрыва"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Клиент оборвал соединение: обработчик дальше не выполняется.
	abort()

	if got := outboxFor(t, server, eventID); !slices.Equal(got, []string{"remote.test"}) {
		t.Fatalf("outbox после обрыва = %v, ожидали [remote.test]", got)
	}

	// PDU собирается при отправке и подписан нашим сервером.
	pdus, err := server.FederationPDUs(context.Background(), []string{eventID, "$missing:test.local"})
	if err != nil {
		t.Fatalf("FederationPDUs: %v", err)
	}

	if _, ok := pdus["$missing:test.local"]; ok || len(pdus) != 1 {
		t.Fatalf("PDU: %v", pdus)
	}

	var object map[string]any
	if err := json.Unmarshal(pdus[eventID], &object); err != nil || object["signatures"] == nil ||
		!strings.Contains(string(pdus[eventID]), "до обрыва") {
		t.Fatalf("PDU без подписи или тела: %s", pdus[eventID])
	}
}

// TestFederationKickStopsDelivery — получатели - серверы с joined-участниками
// на момент события: кикнутый сервер получает свой kick, но не
// последующие события; destination_rooms его теряет. Раньше destination_rooms
// не чистился, и кикнутые серверы получали все события комнаты.
func TestFederationKickStopsDelivery(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")
	setRemoteMember(t, server, roomID, "@dave:other.test", "join")

	before := sendText(t, server, alice, roomID, "t1", "всем")
	if got := outboxFor(t, server, before); !slices.Equal(got, []string{"other.test", "remote.test"}) {
		t.Fatalf("до kick получатели %v", got)
	}

	if got := destinationRoomsOf(t, server, roomID); !slices.Equal(got, []string{"other.test", "remote.test"}) {
		t.Fatalf("destination_rooms до kick = %v", got)
	}

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/kick",
		`{"user_id":"@bob:remote.test","reason":"spam"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("kick: %d %s", rec.Code, rec.Body.String())
	}

	var kickID string
	if err := server.db.Pool().QueryRow(context.Background(), `
		SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`,
		roomID).Scan(&kickID); err != nil {
		t.Fatalf("kick event: %v", err)
	}

	if got := outboxFor(t, server, kickID); !slices.Equal(got, []string{"other.test", "remote.test"}) {
		t.Fatalf("kick получили %v, ожидали оба сервера (кикнутый узнает о kick)", got)
	}

	after := sendText(t, server, alice, roomID, "t2", "без bob")
	if got := outboxFor(t, server, after); !slices.Equal(got, []string{"other.test"}) {
		t.Fatalf("после kick получатели %v, ожидали [other.test]", got)
	}

	if got := destinationRoomsOf(t, server, roomID); !slices.Equal(got, []string{"other.test"}) {
		t.Fatalf("destination_rooms после kick = %v", got)
	}
}

// TestFederationLocalJoinEnqueued — локальный join в комнату с удаленными
// участниками уходит в outbox: раньше /join не ставил событие.
func TestFederationLocalJoinEnqueued(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	carol := registerHTTP(t, server, "carol", "CAROL")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", `{}`, carol)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	var joinID string
	if err := server.db.Pool().QueryRow(context.Background(), `
		SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@carol:test.local'`,
		roomID).Scan(&joinID); err != nil {
		t.Fatalf("join event: %v", err)
	}

	if got := outboxFor(t, server, joinID); !slices.Equal(got, []string{"remote.test"}) {
		t.Fatalf("join получатели %v, ожидали [remote.test]", got)
	}
}

// TestFederationInvitedServerNotRecipient — сервер приглашенного получает
// invite (через /invite), но не события комнаты до входа: раньше
// он добавлялся в destination_rooms до доставки приглашения.
func TestFederationInvitedServerNotRecipient(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")

	// Приглашение уже в состоянии комнаты (доставлено через /invite): сервер
	// приглашенного в outbox попасть не должен. Раньше тест приглашал без
	// транспорта и опирался на 200 при недоставленном invite.
	setRemoteMember(t, server, roomID, "@zed:far.test", "invite")

	message := sendText(t, server, alice, roomID, "t1", "приглашенный не видит")
	if got := outboxFor(t, server, message); !slices.Equal(got, []string{"remote.test"}) {
		t.Fatalf("получатели %v, ожидали [remote.test] (far.test только приглашен)", got)
	}

	var farRows int
	if err := server.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM federation_outbox WHERE destination = 'far.test'`).Scan(&farRows); err != nil || farRows != 0 {
		t.Fatalf("far.test в outbox: %d err=%v", farRows, err)
	}

	if got := destinationRoomsOf(t, server, roomID); slices.Contains(got, "far.test") {
		t.Fatalf("far.test в destination_rooms: %v", got)
	}

	// Вошел - стал получателем.
	setRemoteMember(t, server, roomID, "@zed:far.test", "join")

	joined := sendText(t, server, alice, roomID, "t2", "после входа")
	if got := outboxFor(t, server, joined); !slices.Equal(got, []string{"far.test", "remote.test"}) {
		t.Fatalf("после входа получатели %v", got)
	}
}

// TestFederationOutboxRespectsAllowHosts — получатели вне политики федерации
// в outbox не ставятся: раньше такие элементы ретраились вечно.
func TestFederationOutboxRespectsAllowHosts(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")
	setRemoteMember(t, server, roomID, "@eve:denied.test", "join")

	server.db.SetOutboxDestinationFilter(func(destination string) bool { return destination != "denied.test" })
	t.Cleanup(func() { server.db.SetOutboxDestinationFilter(nil) })

	eventID := sendText(t, server, alice, roomID, "t1", "только разрешенным")
	if got := outboxFor(t, server, eventID); !slices.Equal(got, []string{"remote.test"}) {
		t.Fatalf("получатели %v, ожидали [remote.test]", got)
	}
}

// TestFederationOutboxDisabledSkipsFanout - без федерации запись
// события не ставит outbox (доставлять некому) и не читает серверы комнаты под
// ее блокировкой.
func TestFederationOutboxDisabledSkipsFanout(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ALICE")
	roomID := createFedRoom(t, server, alice)

	setRemoteMember(t, server, roomID, "@bob:remote.test", "join")

	if got := outboxFor(t, server, sendText(t, server, alice, roomID, "t1", "с федерацией")); len(got) != 1 {
		t.Fatalf("с федерацией получатели %v", got)
	}

	server.db.DisableFederationOutbox()

	eventID := sendText(t, server, alice, roomID, "t2", "без федерации")
	if got := outboxFor(t, server, eventID); len(got) != 0 {
		t.Fatalf("без федерации получатели %v", got)
	}
}

// TestFederationSendJoinLeaveRelayed — join и leave, принятые через
// send_join/send_leave, ретранслируются остальным серверам комнаты, но не
// серверу-источнику: раньше они до других серверов не доходили.
func TestFederationSendJoinLeaveRelayed(t *testing.T) {
	env := newFedEnv(t)
	roomID := "!relay:test.local"
	env.seedRoom(t, roomID)

	setRemoteMember(t, env.server, roomID, "@alice:test.local", "join")
	setRemoteMember(t, env.server, roomID, "@dave:other.test", "join")

	joinPDU, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID,
		joinPDU); rec.Code != http.StatusOK {
		t.Fatalf("send_join: %d %s", rec.Code, rec.Body.String())
	}

	if got := outboxFor(t, env.server, joinID); !slices.Equal(got, []string{"other.test"}) {
		t.Fatalf("join ретранслирован %v, ожидали [other.test]", got)
	}

	if got := destinationRoomsOf(t, env.server, roomID); !slices.Equal(got, []string{"other.test", "remote.test"}) {
		t.Fatalf("destination_rooms после join = %v", got)
	}

	leavePDU, leaveID := env.member(t, roomID, "leave", []string{"$create:test.local", "$pl:test.local", joinID}, 5)

	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send_leave/"+roomID+"/"+leaveID,
		leavePDU); rec.Code != http.StatusOK {
		t.Fatalf("send_leave: %d %s", rec.Code, rec.Body.String())
	}

	if got := outboxFor(t, env.server, leaveID); !slices.Equal(got, []string{"other.test"}) {
		t.Fatalf("leave ретранслирован %v, ожидали [other.test]", got)
	}

	// Последний участник remote.test вышел - сервер больше не получатель.
	if got := destinationRoomsOf(t, env.server, roomID); !slices.Equal(got, []string{"other.test"}) {
		t.Fatalf("destination_rooms после leave = %v", got)
	}

	// Повтор того же send_join не ставит событие второй раз.
	if rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/send_join/"+roomID+"/"+joinID,
		joinPDU); rec.Code/100 == 2 {
		if got := outboxFor(t, env.server, joinID); len(got) != 1 {
			t.Fatalf("повтор send_join поставил событие еще раз: %v", got)
		}
	}
}
