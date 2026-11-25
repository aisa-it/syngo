//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// requestRaw - подписанный запрос с телом ровно в переданных байтах (request
// перекодирует тело json.Marshal и экранирует <, >, &).
func (e *fedEnv) requestRaw(t *testing.T, method, path string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()

	header, err := signing.MatrixAuthHeader(e.remoteKey, "remote.test", "test.local", method, path, payload)
	if err != nil {
		t.Fatalf("sign request: %v", err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", header)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)

	return rec
}

// wirePDUs - PDU из ответа федерации байтами, как они пришли в теле.
func wirePDUs(t *testing.T, rec *httptest.ResponseRecorder, field string) []json.RawMessage {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}

	var pdus []json.RawMessage
	if err := json.Unmarshal(body[field], &pdus); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}

	return pdus
}

// TestFederationLocalEventIDMatchesWire - event_id нашего события
// (сообщение, state, redaction) совпадает с id, который посчитает пир по PDU с провода
// (/event). Раньше PDU собирался из колонок: у redaction до v11 пропадал top-level
// redacts, и у пира получался другой id - ответы и редакции не резолвились.
// Проверяется и исходный PDU, и пересборка для событий без него (до миграции 0041).
func TestFederationLocalEventIDMatchesWire(t *testing.T) {
	for _, version := range []int{10, 11} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			env := newFedEnv(t)
			ctx := context.Background()
			rooms := env.server.rooms

			const alice = "@alice:test.local"

			roomID, err := rooms.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, RoomVersion: version})
			if err != nil {
				t.Fatalf("create room: %v", err)
			}

			// /event отдается только серверу с участником комнаты.
			setRemoteMember(t, env.server, roomID, "@bob:remote.test", "join")

			message, _, err := rooms.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m1",
				json.RawMessage(`{"body":"<b>привет</b> &  ","msgtype":"m.text","n":9007199254740991}`))
			if err != nil {
				t.Fatalf("message: %v", err)
			}

			topic, _, err := rooms.SendState(ctx, alice, "DEV", roomID, "m.room.topic", "", "",
				json.RawMessage(`{"topic":"тема"}`))
			if err != nil {
				t.Fatalf("state: %v", err)
			}

			second, _, err := rooms.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m2",
				json.RawMessage(`{"body":"удалить","msgtype":"m.text"}`))
			if err != nil {
				t.Fatalf("message 2: %v", err)
			}

			legacy, _, err := rooms.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m3",
				json.RawMessage(`{"body":"старое","msgtype":"m.text"}`))
			if err != nil {
				t.Fatalf("message 3: %v", err)
			}

			redaction, _, err := rooms.Redact(ctx, alice, "DEV", roomID, second, "причина", "r1")
			if err != nil {
				t.Fatalf("redact: %v", err)
			}

			// Событие до миграции 0041 (без исходного PDU), удаленное уже после нее:
			// пересборка из вырезанного content обязана дать прежний id.
			if _, err := env.server.db.Pool().Exec(ctx, `UPDATE events SET pdu_json = NULL WHERE event_id = $1`, legacy); err != nil {
				t.Fatalf("drop pdu_json: %v", err)
			}

			legacyRedaction, _, err := rooms.Redact(ctx, alice, "DEV", roomID, legacy, "", "r2")
			if err != nil {
				t.Fatalf("redact legacy: %v", err)
			}

			check := func(t *testing.T, eventID, label string) {
				t.Helper()

				pdus := wirePDUs(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event/"+eventID, nil), "pdus")
				if len(pdus) != 1 {
					t.Fatalf("%s: pdus = %d", label, len(pdus))
				}

				// Получатель считает id нашим же разбором входящего PDU (parseFederationPDU).
				received, err := parseFederationPDU(pdus[0], version)
				if err != nil {
					t.Fatalf("%s: parse: %v", label, err)
				}

				if received.EventID != eventID {
					t.Fatalf("%s: id у пира %s, у нас %s\nPDU: %s", label, received.EventID, eventID, pdus[0])
				}

				var object map[string]any
				if err := json.Unmarshal(pdus[0], &object); err != nil {
					t.Fatalf("%s: decode: %v", label, err)
				}

				if _, has := object["event_id"]; has {
					t.Fatalf("%s: в PDU v3+ нет event_id: %s", label, pdus[0])
				}

				if key, has := object["state_key"]; has && key == nil {
					t.Fatalf("%s: state_key: null в PDU: %s", label, pdus[0])
				}

				if err := eventjson.VerifyVersioned(object, "test.local", version, env.server.federationKeyLookup(ctx)); err != nil {
					t.Fatalf("%s: подпись: %v", label, err)
				}
			}

			check(t, legacy, "удаленное без pdu_json")

			events := map[string]string{
				"сообщение": message, "state": topic, "redaction": redaction, "удаленное": second,
				"redaction старого": legacyRedaction,
			}

			for label, eventID := range events {
				var stored bool
				if err := env.server.db.Pool().QueryRow(ctx,
					`SELECT pdu_json IS NOT NULL FROM events WHERE event_id = $1`, eventID).Scan(&stored); err != nil || !stored {
					t.Fatalf("%s: исходный PDU не сохранен (%v)", label, err)
				}

				check(t, eventID, label)

				// Воркер outbox отправляет исходный PDU без пересборки и переподписи.
				var storedPDU string
				if err := env.server.db.Pool().QueryRow(ctx, `SELECT pdu_json FROM events WHERE event_id = $1`, eventID).Scan(&storedPDU); err != nil {
					t.Fatalf("%s: pdu_json: %v", label, err)
				}

				outgoing, err := env.server.FederationPDUs(ctx, []string{eventID})
				if err != nil || string(outgoing[eventID]) != storedPDU {
					t.Fatalf("%s: FederationPDUs = %s (%v), сохранен %s", label, outgoing[eventID], err, storedPDU)
				}
			}

			// События без исходного PDU (до 0041): пересборка из колонок дает тот же id.
			// Удаленное до 0041 событие сюда не входит: его hashes не восстановить.
			delete(events, "удаленное")

			if _, err := env.server.db.Pool().Exec(ctx,
				`UPDATE events SET pdu_json = NULL WHERE room_id = $1 AND redacted_by IS NULL`, roomID); err != nil {
				t.Fatalf("drop pdu_json: %v", err)
			}

			for label, eventID := range events {
				check(t, eventID, label+" без pdu_json")
			}
		})
	}
}

// orderedJSON кодирует объект с ключами в заданном порядке (остальные - в конце):
// так тело заведомо отличается от того, что дало бы перекодирование encoding/json.
func orderedJSON(t *testing.T, object map[string]any, order ...string) []byte {
	t.Helper()

	var buf bytes.Buffer

	buf.WriteString("{ ")

	keys := append([]string{}, order...)

	for key := range object {
		if !strings.Contains(" "+strings.Join(order, " ")+" ", " "+key+" ") {
			keys = append(keys, key)
		}
	}

	for i, key := range keys {
		if i > 0 {
			buf.WriteString(", ")
		}

		var value bytes.Buffer

		encoder := json.NewEncoder(&value)
		encoder.SetEscapeHTML(false)

		if err := encoder.Encode(object[key]); err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}

		name, _ := json.Marshal(key)
		buf.Write(name)
		buf.WriteString(": ")
		buf.Write(bytes.TrimRight(value.Bytes(), "\n"))
	}

	buf.WriteString(" }")

	return buf.Bytes()
}

// TestFederationForeignEventServedVerbatim - чужое событие из /send
// отдается в /event и /state байт-в-байт (с точностью до пробелов) - с подписями,
// hashes, origin, unsigned и тем же порядком ключей. Раньше оно хранилось без
// signatures и hashes и уходило пирам без подписи.
func TestFederationForeignEventServedVerbatim(t *testing.T) {
	env := newFedEnv(t)
	ctx := context.Background()

	const roomID = "!verbatim:test.local"
	env.seedRoom(t, roomID)

	object := map[string]any{
		"room_id": roomID, "type": "m.room.member", "sender": "@bob:remote.test", "state_key": "@bob:remote.test",
		"origin": "remote.test", "origin_server_ts": int64(1_700_000_000_123), "depth": 4,
		"content":     map[string]any{"membership": "join", "displayname": "<Боб> &  ", "n": 9007199254740991},
		"prev_events": []string{"$invite:test.local"},
		"auth_events": []string{"$create:test.local", "$pl:test.local", "$invite:test.local"},
	}

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	object["hashes"] = map[string]any{"sha256": hash}

	signed, err := eventjson.SignVersioned(env.remoteKey, "remote.test", object, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	signed["unsigned"] = map[string]any{"age": 5}

	raw := orderedJSON(t, signed, "type", "sender", "content", "room_id")

	eventID, err := eventjson.EventID(object, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	body := []byte(`{"origin":"remote.test","pdus":[` + string(raw) + `],"edus":[]}`)

	rec := env.requestRaw(t, http.MethodPut, "/_matrix/federation/v1/send/txn-verbatim", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	var want bytes.Buffer
	if err := json.Compact(&want, raw); err != nil {
		t.Fatalf("compact: %v", err)
	}

	pdus := wirePDUs(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/event/"+eventID, nil), "pdus")
	if len(pdus) != 1 || !bytes.Equal(pdus[0], want.Bytes()) {
		t.Fatalf("/event отдал не исходный PDU:\n got: %s\nwant: %s", pdus, want.Bytes())
	}

	var found bool

	for _, pdu := range wirePDUs(t, env.request(t, http.MethodGet, "/_matrix/federation/v1/state/"+roomID, nil), "pdus") {
		if bytes.Equal(pdu, want.Bytes()) {
			found = true
		}
	}

	if !found {
		t.Fatalf("в /state нет исходного PDU %s", eventID)
	}

	// Воркер outbox (ретрансляция третьим серверам) берет тот же исходный PDU.
	outgoing, err := env.server.FederationPDUs(ctx, []string{eventID})
	if err != nil || !bytes.Equal(outgoing[eventID], want.Bytes()) {
		t.Fatalf("FederationPDUs отдал не исходный PDU (%v):\n got: %s\nwant: %s", err, outgoing[eventID], want.Bytes())
	}

	// Подпись отправителя проверяется по отданному PDU.
	var served map[string]any
	if err := json.Unmarshal(pdus[0], &served); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if err := eventjson.VerifyVersioned(served, "remote.test", core.DefaultRoomVersion, env.server.federationKeyLookup(ctx)); err != nil {
		t.Fatalf("подпись отправителя в отданном PDU: %v", err)
	}
}

// TestFederationInviteKeepsDepthAndTS - тело invite разбирается с
// UseNumber, и depth/origin_server_ts раньше терялись (0 в events).
func TestFederationInviteKeepsDepthAndTS(t *testing.T) {
	env := newFedEnv(t)

	const roomID = "!invite-depth:remote.test"

	member, eventID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@carol:test.local"),
		map[string]any{"membership": "invite"}, []string{"$create:remote.test"}, nil, 4)

	rec := env.request(t, http.MethodPut, "/_matrix/federation/v2/invite/"+roomID+"/"+eventID,
		map[string]any{"room_version": "10", "event": member})
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body.String())
	}

	var depth, ts int64

	var stored bool
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT depth, origin_server_ts, pdu_json IS NOT NULL FROM events WHERE event_id = $1`, eventID).
		Scan(&depth, &ts, &stored); err != nil {
		t.Fatalf("event: %v", err)
	}

	if depth != 4 || ts != 1_700_000_000_000 || !stored {
		t.Fatalf("depth=%d origin_server_ts=%d pdu_json=%v; ожидали 4, 1700000000000, true", depth, ts, stored)
	}
}
