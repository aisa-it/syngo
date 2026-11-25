package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// joinUser добавляет участника в комнату через публичную join_rule.
func joinUser(t *testing.T, svc *core.RoomService, roomID, userID string) {
	t.Helper()

	ctx := context.Background()

	if _, err := svc.Join(ctx, userID, roomID); err != nil {
		t.Fatalf("Join %s: %v", userID, err)
	}
}

// TestSendStatePermission — низкий power level не может менять состояние (403).
func TestSendStatePermission(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")
	joinUser(t, svc, roomID, bob)

	// bob (0) не может писать state_default=50.
	if _, _, err := svc.SendState(context.Background(), bob, "DEV", roomID,
		"m.room.topic", "", "st1", json.RawMessage(`{"topic":"from bob"}`)); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("ожидали ErrNotAllowed, получили %v", err)
	}

	// alice (создатель, 100) может.
	eventID, _, err := svc.SendState(context.Background(), alice, "DEV", roomID,
		"m.room.topic", "", "st2", json.RawMessage(`{"topic":"from alice"}`))
	if err != nil {
		t.Fatalf("SendState alice: %v", err)
	}

	ev, err := svc.StateEvent(context.Background(), alice, roomID, "m.room.topic", "")
	if err != nil {
		t.Fatalf("StateEvent: %v", err)
	}

	if ev.EventID != eventID {
		t.Fatalf("состояние topic = %s, ожидали %s", ev.EventID, eventID)
	}
}

// TestKickBanRights — kick/ban проверяют уровень цели и запрет на себя.
func TestKickBanRights(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")
	joinUser(t, svc, roomID, bob)

	ctx := context.Background()

	if _, _, err := svc.SendMembership(ctx, alice, "DEV", roomID, bob, "leave", "flood", "k1"); err != nil {
		t.Fatalf("kick bob: %v", err)
	}

	// Повторный kick: bob уже не в комнате.
	if _, _, err := svc.SendMembership(ctx, alice, "DEV", roomID, bob, "leave", "", "k2"); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("повторный kick: ожидали ErrNotAllowed, получили %v", err)
	}

	joinUser(t, svc, roomID, bob)

	if _, _, err := svc.SendMembership(ctx, alice, "DEV", roomID, bob, "ban", "spam", "b1"); err != nil {
		t.Fatalf("ban bob: %v", err)
	}

	// bob в бане: unban — это leave из ban, состояние должно обновиться.
	if _, _, err := svc.SendMembership(ctx, alice, "DEV", roomID, bob, "leave", "unban", "u1"); err != nil {
		t.Fatalf("unban bob: %v", err)
	}

	membership, err := svc.StateEvent(ctx, alice, roomID, "m.room.member", bob)
	if err != nil {
		t.Fatalf("StateEvent member: %v", err)
	}

	if !json.Valid(membership.Content) {
		t.Fatal("content не JSON")
	}

	// Нельзя кикнуть себя.
	if _, _, err := svc.SendMembership(ctx, alice, "DEV", roomID, alice, "leave", "", "s1"); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("kick себя: ожидали ErrNotAllowed, получили %v", err)
	}
}

// TestRedactHidesContent — redaction вырезает content, автор может всегда.
func TestRedactHidesContent(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")

	ctx := context.Background()

	eventID, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"msgtype":"m.text","body":"секрет","extra":"поле"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if _, _, err := svc.Redact(ctx, bob, "DEV", roomID, eventID, "", "r0"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("чужой redaction: ожидали ErrForbidden, получили %v", err)
	}

	joinUser(t, svc, roomID, bob)

	if _, _, err := svc.Redact(ctx, bob, "DEV", roomID, eventID, "", "r1"); !errors.Is(err, core.ErrNotAllowed) {
		t.Fatalf("redaction без прав: ожидали ErrNotAllowed, получили %v", err)
	}

	if _, _, err := svc.Redact(ctx, alice, "DEV", roomID, eventID, "cleanup", "r2"); err != nil {
		t.Fatalf("redaction автором: %v", err)
	}

	ev, err := svc.Event(ctx, alice, roomID, eventID)
	if err != nil {
		t.Fatalf("Event после redaction: %v", err)
	}

	var content map[string]any
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		t.Fatalf("content: %v", err)
	}

	if _, ok := content["body"]; ok {
		t.Fatalf("body не вырезан: %s", ev.Content)
	}

	if _, ok := content["extra"]; ok {
		t.Fatalf("extra не вырезан: %s", ev.Content)
	}

	// История (/messages) тоже отдаёт событие без вырезанных полей.
	messages, err := svc.Messages(ctx, alice, roomID, "", "b", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	found := false

	for i := range messages.Events {
		if messages.Events[i].EventID != eventID {
			continue
		}

		found = true

		if strings.Contains(string(messages.Events[i].Content), "секрет") {
			t.Fatalf("/messages отдал оригинальный content: %s", messages.Events[i].Content)
		}
	}

	if !found {
		t.Fatal("событие не найдено в /messages")
	}
}

// TestRedactContentRules — правила redaction v10/v11 для служебных событий.
func TestRedactContentRules(t *testing.T) {
	t.Parallel()

	got := core.RedactContent("m.room.member", []byte(`{"membership":"join","displayname":"x"}`))
	if string(got) != `{"membership":"join"}` {
		t.Fatalf("member redaction = %s", got)
	}

	got = core.RedactContent("m.room.power_levels", []byte(`{"users":{"@a:b":100},"secret":1}`))
	if string(got) != `{"users":{"@a:b":100}}` {
		t.Fatalf("power_levels redaction = %s", got)
	}

	// Кастомные и не-m.room типы (m.reaction, m.sticker) - content удаляется целиком.
	for _, eventType := range []string{"com.example.custom", "m.reaction", "m.sticker"} {
		got = core.RedactContent(eventType, []byte(`{"a":1}`))
		if string(got) != `{}` {
			t.Fatalf("%s после redaction = %s, ожидали {}", eventType, got)
		}
	}
}

// createRoomWithJoinRules создаёт комнату с заданным join_rule.
func createRoomWithJoinRules(t *testing.T, svc *core.RoomService, joinRule string) string {
	t.Helper()

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator:  alice,
		Name:     "state api",
		IsPublic: joinRule == "public",
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	return roomID
}

// TestStateAndJoinIdempotent - то же значение состояния от того же отправителя и
// повторный join с тем же content не создают событий (Synapse
// deduplicate_state_event, Complement TestRoomCreationReportsEventsToMyself).
// A -> B -> A по-прежнему дает новое событие.
func TestStateAndJoinIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")

	content := json.RawMessage(`{"a_key":"a_value"}`)

	first, _, err := svc.SendState(ctx, alice, "DEV", roomID, "a.test.state.type", "", "", content)
	if err != nil {
		t.Fatalf("первый SendState: %v", err)
	}

	before := len(store.Events())

	second, duplicate, err := svc.SendState(ctx, alice, "DEV", roomID, "a.test.state.type", "", "",
		json.RawMessage(`{ "a_key" : "a_value" }`))
	if err != nil || second != first || !duplicate || len(store.Events()) != before {
		t.Fatalf("повтор SendState: %s/%s duplicate=%v err=%v, событий %d -> %d",
			first, second, duplicate, err, before, len(store.Events()))
	}

	changed, _, err := svc.SendState(ctx, alice, "DEV", roomID, "a.test.state.type", "", "", json.RawMessage(`{"a_key":"b"}`))
	if err != nil {
		t.Fatalf("смена значения: %v", err)
	}

	back, _, err := svc.SendState(ctx, alice, "DEV", roomID, "a.test.state.type", "", "", content)
	if err != nil || back == first || back == changed {
		t.Fatalf("A -> B -> A должно дать новое событие: %s (было %s, %s), err=%v", back, first, changed, err)
	}

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("join: %v", err)
	}

	joins := len(store.Events())

	if _, err := svc.Join(ctx, bob, roomID); err != nil || len(store.Events()) != joins {
		t.Fatalf("повторный join: err=%v, событий %d -> %d", err, joins, len(store.Events()))
	}
}
