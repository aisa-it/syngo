package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestNewEventStoresSignedPDU - с подписчиком каждое событие получает
// исходный PDU - подписанный объект, по которому посчитан его event_id; пересборка
// из полей (core.PDUObject) дает тот же id. Сообщение, state и redaction в v10
// (top-level redacts) и v11.
func TestNewEventStoresSignedPDU(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	lookup := func(_, _ string) (string, int64, error) { return key.Public(), 0, nil }

	for _, version := range []int{10, 11} {
		svc, mem := newRoomService(t)
		svc.WithPDUSigner(func(object map[string]any, v int) (map[string]any, error) {
			return eventjson.SignVersioned(key, "test.local", object, v)
		})

		ctx := context.Background()

		roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, RoomVersion: version})
		if err != nil {
			t.Fatalf("v%d: create: %v", version, err)
		}

		message, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m1",
			json.RawMessage(`{"body":"<b>&</b>","msgtype":"m.text"}`))
		if err != nil {
			t.Fatalf("v%d: message: %v", version, err)
		}

		topic, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.topic", "", "", json.RawMessage(`{"topic":"t"}`))
		if err != nil {
			t.Fatalf("v%d: state: %v", version, err)
		}

		redaction, _, err := svc.Redact(ctx, alice, "DEV", roomID, message, "", "r1")
		if err != nil {
			t.Fatalf("v%d: redact: %v", version, err)
		}

		for _, eventID := range []string{message, topic, redaction} {
			ev, err := mem.EventByID(ctx, roomID, eventID)
			if err != nil {
				t.Fatalf("v%d: event %s: %v", version, eventID, err)
			}

			if len(ev.PDU) == 0 {
				t.Fatalf("v%d: %s без исходного PDU", version, ev.Type)
			}

			var object map[string]any
			if err := json.Unmarshal(ev.PDU, &object); err != nil {
				t.Fatalf("v%d: decode PDU: %v", version, err)
			}

			if id, err := eventjson.EventID(object, version); err != nil || id != eventID {
				t.Fatalf("v%d: %s: id по PDU %s, у события %s (%v)", version, ev.Type, id, eventID, err)
			}

			if err := eventjson.VerifyVersioned(object, "test.local", version, lookup); err != nil {
				t.Fatalf("v%d: %s: подпись: %v", version, ev.Type, err)
			}

			// Пересборка из полей (событие без исходного PDU) - тот же id.
			rebuilt := core.PDUObject(ev)
			rebuilt["hashes"] = object["hashes"]

			if id, err := eventjson.EventID(rebuilt, version); err != nil || id != eventID {
				t.Fatalf("v%d: %s: id пересборки %s, у события %s (%v)", version, ev.Type, id, eventID, err)
			}

			if version < 11 && ev.Type == "m.room.redaction" && object["redacts"] != message {
				t.Fatalf("v%d: в PDU redaction нет top-level redacts: %s", version, ev.PDU)
			}
		}
	}
}

// TestNewEventWithoutSignerHasNoPDU - без подписчика (федерация выключена) PDU не
// хранится: отдающая сторона соберет его из полей.
func TestNewEventWithoutSignerHasNoPDU(t *testing.T) {
	t.Parallel()

	svc, mem := newRoomService(t)
	roomID := createRoom(t, svc)

	eventID, _, err := svc.SendMessage(context.Background(), alice, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"body":"x","msgtype":"m.text"}`))
	if err != nil {
		t.Fatalf("message: %v", err)
	}

	ev, err := mem.EventByID(context.Background(), roomID, eventID)
	if err != nil || len(ev.PDU) != 0 {
		t.Fatalf("PDU без подписчика: %s (%v)", ev.PDU, err)
	}
}
