package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

const (
	alice = "@alice:test.local"
	bob   = "@bob:test.local"
)

// newRoomService собирает сервис комнат на in-memory хранилище.
func newRoomService(t *testing.T) (*core.RoomService, *coretest.MemRoomStore) {
	t.Helper()

	store := coretest.NewMemRoomStore()
	svc := core.NewRoomService(store, "test.local").WithClock(func() time.Time {
		return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	})

	return svc, store
}

// createRoom создаёт комнату от имени alice.
func createRoom(t *testing.T, svc *core.RoomService) string {
	t.Helper()

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator: alice,
		Name:    "Тестовая комната",
	})
	if err != nil {
		t.Fatalf("CreateRoom() error: %v", err)
	}

	return roomID
}

func TestCreateRoomGenesis(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	roomID := createRoom(t, svc)

	if roomID == "" || roomID[0] != '!' || roomID[len(roomID)-1] != 'l' {
		t.Fatalf("room_id = %q, ожидали !opaque:test.local", roomID)
	}

	events := store.Events()
	// Пресет private_chat (по умолчанию) пишет join_rules, history_visibility и
	// guest_access, как Synapse.
	if len(events) != 7 {
		t.Fatalf("создано %d событий, ожидали 7 (create, member, power_levels, join_rules, history_visibility, guest_access, name)", len(events))
	}

	types := make([]string, 0, len(events))

	for i := range events {
		event := &events[i]
		types = append(types, event.Type)

		if event.Depth == 0 || event.OriginServerTS == 0 || event.EventID == "" {
			t.Fatalf("событие без обязательных полей: %+v", event)
		}
	}

	want := []string{
		"m.room.create", "m.room.member", "m.room.power_levels", "m.room.join_rules",
		"m.room.history_visibility", "m.room.guest_access", "m.room.name",
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("порядок событий %v, ожидали %v", types, want)
		}
	}

	// prev_events второго события ссылается на первое, depth растёт.
	if len(events[1].PrevEvents) != 1 || events[1].PrevEvents[0] != events[0].EventID {
		t.Fatalf("prev_events второго события = %v", events[1].PrevEvents)
	}

	if events[1].Depth != events[0].Depth+1 {
		t.Fatalf("depth не растёт: %d → %d", events[0].Depth, events[1].Depth)
	}
}

func TestCreateRoomInvalidVersion(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)

	if _, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator: alice, RoomVersion: 9,
	}); !errors.Is(err, core.ErrInvalidRoomVersion) {
		t.Fatalf("CreateRoom(v9) = %v, ожидали ErrInvalidRoomVersion", err)
	}
}

func TestSendMessageAndIdempotency(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	roomID := createRoom(t, svc)

	content := json.RawMessage(`{"msgtype":"m.text","body":"привет"}`)

	first, duplicate, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "txn1", content)
	if err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}

	if duplicate {
		t.Fatal("первый вызов не должен помечаться как дубль")
	}

	second, duplicate, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "txn1", content)
	if err != nil {
		t.Fatalf("повторный SendMessage() error: %v", err)
	}

	if !duplicate {
		t.Fatal("повтор с тем же txn_id должен быть дублем")
	}

	if second != first {
		t.Fatalf("event_id повторного вызова %q, ожидали %q", second, first)
	}

	if got := len(store.Events()); got != 8 {
		t.Fatalf("событий в комнате %d, ожидали 8 (7 genesis + 1 сообщение)", got)
	}

	// Другой txn_id создаёт новое событие.
	third, duplicate, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "txn2", content)
	if err != nil {
		t.Fatalf("SendMessage(txn2) error: %v", err)
	}

	if duplicate || third == first {
		t.Fatalf("новый txn должен создать новое событие: %q", third)
	}
}

func TestSendMessageForbiddenForNonMember(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	if _, _, err := svc.SendMessage(context.Background(), bob, "DEV2", roomID,
		"m.room.message", "txn1", json.RawMessage(`{"body":"нельзя"}`)); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("SendMessage(не участник) = %v, ожидали ErrForbidden", err)
	}
}

func TestJoinInviteLeave(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	// Без приглашения в приватную комнату — запрещено.
	if _, err := svc.Join(context.Background(), bob, roomID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Join(без приглашения) = %v, ожидали ErrForbidden", err)
	}

	if _, err := svc.Invite(context.Background(), alice, roomID, bob); err != nil {
		t.Fatalf("Invite() error: %v", err)
	}

	if _, err := svc.Join(context.Background(), bob, roomID); err != nil {
		t.Fatalf("Join(после приглашения) error: %v", err)
	}

	if _, _, err := svc.SendMessage(context.Background(), bob, "DEV2", roomID,
		"m.room.message", "txn2", json.RawMessage(`{"body":"привет"}`)); err != nil {
		t.Fatalf("SendMessage(участник) error: %v", err)
	}

	if _, err := svc.Leave(context.Background(), bob, roomID); err != nil {
		t.Fatalf("Leave() error: %v", err)
	}

	if _, _, err := svc.SendMessage(context.Background(), bob, "DEV2", roomID,
		"m.room.message", "txn3", json.RawMessage(`{"body":"после leave"}`)); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("SendMessage(после leave) = %v, ожидали ErrForbidden", err)
	}
}

func TestJoinPublicRoom(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator: alice, Preset: "public_chat",
	})
	if err != nil {
		t.Fatalf("CreateRoom() error: %v", err)
	}

	if _, err := svc.Join(context.Background(), bob, roomID); err != nil {
		t.Fatalf("Join(public) error: %v", err)
	}
}

func TestMessagesPagination(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	for i, body := range []string{"первое", "второе", "третье"} {
		content, err := json.Marshal(map[string]string{"body": body})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		if _, _, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID,
			"m.room.message", "txn"+string(rune('1'+i)), content); err != nil {
			t.Fatalf("SendMessage(%s) error: %v", body, err)
		}
	}

	// dir=b: от новых к старым.
	backward, err := svc.Messages(context.Background(), alice, roomID, "", "b", 2)
	if err != nil {
		t.Fatalf("Messages(b) error: %v", err)
	}

	if len(backward.Events) != 2 {
		t.Fatalf("получено %d событий, ожидали 2", len(backward.Events))
	}

	if backward.Events[0].EventID == backward.Events[1].EventID {
		t.Fatal("пагинация вернула одно и то же событие дважды")
	}

	if backward.Events[0].StreamOrdering < backward.Events[1].StreamOrdering {
		t.Fatal("dir=b должен идти от новых к старым")
	}

	// Следующая страница от end курсора.
	next, err := svc.Messages(context.Background(), alice, roomID, backward.End, "b", 2)
	if err != nil {
		t.Fatalf("Messages(b, end=%s) error: %v", backward.End, err)
	}

	if len(next.Events) == 0 {
		t.Fatal("вторая страница пуста, хотя в комнате есть genesis-события")
	}

	for _, event := range next.Events {
		if event.StreamOrdering >= backward.Events[1].StreamOrdering {
			t.Fatalf("вторая страница пересекается с первой: %d", event.StreamOrdering)
		}
	}

	// dir=f: от старых к новым.
	forward, err := svc.Messages(context.Background(), alice, roomID, "", "f", 10)
	if err != nil {
		t.Fatalf("Messages(f) error: %v", err)
	}

	for i := 1; i < len(forward.Events); i++ {
		if forward.Events[i-1].StreamOrdering > forward.Events[i].StreamOrdering {
			t.Fatal("dir=f должен идти от старых к новым")
		}
	}
}

func TestMessagesForbiddenForNonMember(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	if _, err := svc.Messages(context.Background(), bob, roomID, "", "b", 10); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Messages(не участник) = %v, ожидали ErrForbidden", err)
	}
}

func TestEventReadForbiddenForNonMember(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	roomID := createRoom(t, svc)

	eventID := store.Events()[0].EventID

	if _, err := svc.Event(context.Background(), bob, roomID, eventID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Event(не участник) = %v, ожидали ErrForbidden", err)
	}

	event, err := svc.Event(context.Background(), alice, roomID, eventID)
	if err != nil {
		t.Fatalf("Event(участник) error: %v", err)
	}

	if event.EventID != eventID {
		t.Fatalf("Event() = %q, ожидали %q", event.EventID, eventID)
	}
}

func TestEventIDsAreDeterministic(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	content := json.RawMessage(`{"body":"одинаково"}`)

	first, _, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "a", content)
	if err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}

	// Другой txn, но то же содержимое, тот же prev_events и то же время (фиксированные часы)
	// не должны давать тот же event_id: depth и prev_events уже другие.
	second, _, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "b", content)
	if err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}

	if first == second {
		t.Fatal("event_id двух разных событий совпали")
	}
}

// TestEventIDReferenceHash — event_id считается как reference hash
// (canonical JSON с hashes, без signatures/unsigned/event_id/origin), а не по
// самодельному набору полей. Вектор снят с canonicaljson Python.
func TestEventIDReferenceHash(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	roomID := createRoom(t, svc)

	content := json.RawMessage(`{"msgtype":"m.text","body":"<b>&</b>"}`)

	eventID, _, err := svc.SendMessage(context.Background(), alice, "DEV1", roomID, "m.room.message", "t1", content)
	if err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}

	if eventID == "" || eventID[0] != '$' {
		t.Fatalf("event_id = %q", eventID)
	}

	events := store.Events()
	last := events[len(events)-1]

	if last.EventID != eventID {
		t.Fatalf("event_id в БД %q, ожидали %q", last.EventID, eventID)
	}

	// auth_events проставлены до расчёта id: у сообщения это как минимум create.
	if len(last.AuthEvents) == 0 {
		t.Fatal("auth_events пусты — id посчитан до их заполнения")
	}

	// Тот же набор полей, что уходит пиру, даёт тот же id.
	authSorted := append([]string{}, last.AuthEvents...)
	sort.Strings(authSorted)

	object := map[string]any{
		"type": last.Type, "sender": last.Sender, "room_id": last.RoomID,
		"content": last.Content, "prev_events": last.PrevEvents, "auth_events": authSorted,
		"depth": last.Depth, "origin_server_ts": last.OriginServerTS,
	}

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}

	object["hashes"] = map[string]string{"sha256": hash}

	// reference hash считается по redacted-событию (как compute_event_reference_hash).
	object["content"] = json.RawMessage(core.RedactContent(last.Type, last.Content))

	referenceID, err := signing.ReferenceHash(object)
	if err != nil {
		t.Fatalf("ReferenceHash: %v", err)
	}

	dbg, _ := signing.CanonicalJSON(object)
	t.Logf("тест: %s", dbg)
	t.Logf("auth: %v prev: %v content: %s", last.AuthEvents, last.PrevEvents, last.Content)

	if referenceID != eventID {
		t.Fatalf("id не равен reference hash: id=%q ref=%q", eventID, referenceID)
	}
}

// TestCreateRoomAuthRefsResolve — все auth_events genesis-событий
// ссылаются на реально собранные события с финальными id.
func TestCreateRoomAuthRefsResolve(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)

	roomID, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator: alice, Name: "refs", Invite: []string{bob},
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	ids := map[string]struct{}{}

	for _, ev := range store.Events() {
		if ev.RoomID == roomID {
			ids[ev.EventID] = struct{}{}
		}
	}

	for _, ev := range store.Events() {
		if ev.RoomID != roomID {
			continue
		}

		for _, authID := range ev.AuthEvents {
			if _, ok := ids[authID]; !ok {
				t.Fatalf("%s (%s) ссылается на неизвестный auth %s", ev.EventID, ev.Type, authID)
			}
		}
	}
}

// TestVisibleEventWorldReadable - GET /event не участнику: в world_readable
// комнате событие видно, иначе ErrNotFound (404, как в Synapse).
func TestVisibleEventWorldReadable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRoomService(t)
	roomID := createRoom(t, svc)
	eventID := store.Events()[0].EventID

	if _, err := svc.VisibleEvent(ctx, bob, roomID, eventID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("VisibleEvent(не участник, shared) = %v, ожидали ErrNotFound", err)
	}

	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.history_visibility", "", "",
		json.RawMessage(`{"history_visibility":"world_readable"}`)); err != nil {
		t.Fatalf("history_visibility: %v", err)
	}

	event, err := svc.VisibleEvent(ctx, bob, roomID, eventID)
	if err != nil || event.EventID != eventID {
		t.Fatalf("VisibleEvent(не участник, world_readable) = %v, %v", event.EventID, err)
	}

	if _, err := svc.Event(ctx, bob, roomID, eventID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Event(не участник) = %v, ожидали ErrForbidden: проверка членства не меняется", err)
	}
}
