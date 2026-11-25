package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// stateOf - последнее событие состояния комнаты по типу и ключу.
func stateOf(t *testing.T, store *coretest.MemRoomStore, roomID, eventType, stateKey string) core.Event {
	t.Helper()

	events, err := store.StateEvents(context.Background(), roomID)
	if err != nil {
		t.Fatal(err)
	}

	for i := range events {
		if events[i].Type == eventType && events[i].StateKey != nil && *events[i].StateKey == stateKey {
			return events[i]
		}
	}

	t.Fatalf("нет состояния %s/%q в %s", eventType, stateKey, roomID)

	return core.Event{}
}

// TestCreateRoomV12 - комната v12 как в Synapse 1.165 - room_id это
// «!» и reference hash m.room.create, у самого create нет room_id, остальные
// события не ссылаются на create в auth_events, создатели (alice и
// additional_creators) не записаны в power_levels и действуют с бесконечным
// уровнем, tombstone требует 150. Раньше версия 12 отвергалась.
func TestCreateRoomV12(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	ctx := context.Background()

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: alice, Name: "v12", RoomVersion: 12, Invite: []string{bob},
		CreationExtras: map[string]any{"additional_creators": []any{carol}},
	})
	if err != nil {
		t.Fatalf("CreateRoom v12: %v", err)
	}

	create := stateOf(t, store, roomID, "m.room.create", "")
	if strings.Contains(roomID, ":") || roomID != "!"+strings.TrimPrefix(create.EventID, "$") {
		t.Fatalf("room_id %s, create %s", roomID, create.EventID)
	}

	// event_id create считается без room_id (его нет в PDU v12).
	object := map[string]any{
		"type": create.Type, "sender": create.Sender, "state_key": "", "content": create.Content,
		"prev_events": []string{}, "auth_events": []string{}, "depth": create.Depth,
		"origin_server_ts": create.OriginServerTS,
	}

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatal(err)
	}

	object["hashes"] = map[string]string{"sha256": hash}

	if id, err := core.EventID(object, 12); err != nil || id != create.EventID {
		t.Fatalf("event_id create без room_id = %s (%v), в хранилище %s", id, err, create.EventID)
	}

	var createContent map[string]any
	if err := json.Unmarshal(create.Content, &createContent); err != nil {
		t.Fatal(err)
	}

	if createContent["room_version"] != "12" || createContent["creator"] != nil {
		t.Fatalf("content create v12: %s", create.Content)
	}

	for _, ev := range store.Events() {
		if ev.RoomID != roomID {
			t.Fatalf("событие %s %s в комнате %q", ev.Type, ev.EventID, ev.RoomID)
		}

		for _, ref := range ev.AuthEvents {
			if ref == create.EventID {
				t.Fatalf("%s ссылается на create в auth_events: %v", ev.Type, ev.AuthEvents)
			}
		}

		// Вход создателя ссылается только на create - в v12 auth_events пусты (как в Synapse).
		if ev.Depth > 2 && len(ev.AuthEvents) == 0 {
			t.Fatalf("%s без auth_events", ev.Type)
		}
	}

	var levels struct {
		Users  map[string]int `json:"users"`
		Events map[string]int `json:"events"`
	}

	if err := json.Unmarshal(stateOf(t, store, roomID, "m.room.power_levels", "").Content, &levels); err != nil {
		t.Fatal(err)
	}

	if _, ok := levels.Users[alice]; ok || levels.Users[carol] != 0 || levels.Events["m.room.tombstone"] != 150 {
		t.Fatalf("power_levels v12: %+v", levels)
	}

	if _, ok := levels.Users[carol]; ok {
		t.Fatalf("дополнительный создатель в power_levels: %+v", levels)
	}

	// Создатель вне users меняет состояние, требующее 50 и 150.
	if _, _, err := svc.SendState(ctx, alice, "D", roomID, "m.room.topic", "", "", json.RawMessage(`{"topic":"t"}`)); err != nil {
		t.Fatalf("создатель меняет тему: %v", err)
	}

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("join bob: %v", err)
	}

	if _, _, err := svc.SendMessage(ctx, bob, "D", roomID, "m.room.message", "t1", json.RawMessage(`{"body":"x"}`)); err != nil {
		t.Fatalf("сообщение bob: %v", err)
	}

	if _, _, err := svc.SendState(ctx, bob, "D", roomID, "m.room.topic", "", "", json.RawMessage(`{"topic":"b"}`)); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("участник 0 меняет тему: %v", err)
	}

	// Создателя нельзя вписать в power_levels.
	if _, _, err := svc.SendState(ctx, alice, "D", roomID, "m.room.power_levels", "", "",
		json.RawMessage(`{"users":{"@alice:test.local":100}}`)); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("создатель в power_levels.users: %v", err)
	}
}

// TestCreateRoomV12Creators - trusted_private_chat делает приглашенных
// дополнительными создателями (вместо уровня 100), создатель в
// power_level_content_override и кривые additional_creators отвергаются.
func TestCreateRoomV12Creators(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	ctx := context.Background()

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: alice, RoomVersion: 12, Preset: "trusted_private_chat", Invite: []string{bob}, IsDirect: true,
	})
	if err != nil {
		t.Fatalf("trusted_private_chat v12: %v", err)
	}

	var create struct {
		AdditionalCreators []string `json:"additional_creators"`
	}

	if err := json.Unmarshal(stateOf(t, store, roomID, "m.room.create", "").Content, &create); err != nil {
		t.Fatal(err)
	}

	if len(create.AdditionalCreators) != 1 || create.AdditionalCreators[0] != bob {
		t.Fatalf("additional_creators = %v", create.AdditionalCreators)
	}

	if pl := string(stateOf(t, store, roomID, "m.room.power_levels", "").Content); strings.Contains(pl, bob) || strings.Contains(pl, alice) {
		t.Fatalf("создатели в power_levels: %s", pl)
	}

	for name, req := range map[string]core.CreateRoomRequest{
		"создатель в override": {
			Creator: alice, RoomVersion: 12,
			PowerLevelOverride: map[string]any{"users": map[string]any{alice: 100}},
		},
		"additional_creators не список": {
			Creator: alice, RoomVersion: 12,
			CreationExtras: map[string]any{"additional_creators": bob},
		},
		"кривой user_id": {
			Creator: alice, RoomVersion: 12,
			CreationExtras: map[string]any{"additional_creators": []any{"bob"}},
		},
	} {
		if _, err := svc.CreateRoom(ctx, req); !errors.Is(err, core.ErrBadJSON) {
			t.Fatalf("%s: %v, ожидали ErrBadJSON", name, err)
		}
	}

	// Одинаковые запросы в ту же миллисекунду (часы теста стоят) - разные комнаты.
	first, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, RoomVersion: 12})
	if err != nil {
		t.Fatal(err)
	}

	second, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, RoomVersion: 12})
	if err != nil || second == first {
		t.Fatalf("повтор createRoom v12: %s и %s, %v", first, second, err)
	}

	if _, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: alice, RoomVersion: 13}); !errors.Is(err, core.ErrInvalidRoomVersion) {
		t.Fatalf("v13: %v", err)
	}
}

// TestUpgradeRoomToV12 - замена комнаты на v12 (Element предлагает ее
// для v10/v11) не пишет инициатора в power_levels новой комнаты - он ее создатель;
// уровни остальных переносятся.
func TestUpgradeRoomToV12(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	ctx := context.Background()

	oldRoom, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: alice, RoomVersion: 11, Invite: []string{bob},
		PowerLevelOverride: map[string]any{"users": map[string]any{bob: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.UpgradeRoom(ctx, alice, "D", oldRoom, 12)
	if err != nil || len(res.Warnings) > 0 {
		t.Fatalf("upgrade: %+v, %v", res, err)
	}

	if strings.Contains(res.ReplacementRoom, ":") {
		t.Fatalf("замена не v12: %s", res.ReplacementRoom)
	}

	var levels struct {
		Users map[string]int `json:"users"`
	}

	if err := json.Unmarshal(stateOf(t, store, res.ReplacementRoom, "m.room.power_levels", "").Content, &levels); err != nil {
		t.Fatal(err)
	}

	if _, ok := levels.Users[alice]; ok || levels.Users[bob] != 50 {
		t.Fatalf("power_levels замены: %+v", levels.Users)
	}

	if _, _, err := svc.SendState(ctx, alice, "D", res.ReplacementRoom, "m.room.name", "", "", json.RawMessage(`{"name":"n"}`)); err != nil {
		t.Fatalf("создатель замены меняет название: %v", err)
	}
}
