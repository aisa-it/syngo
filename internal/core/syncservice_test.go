package core_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// memSyncStores - in-memory хранилища SyncService: состояние комнат (одно на
// любую позицию, запросы запоминаются), очередь to-device и ключи.
type memSyncStores struct {
	state     map[string][]core.Event
	calls     []string
	pending   []core.ToDeviceEvent
	deleted   []int64
	changed   []string
	otk       map[string]int
	fallbacks []string
	err       error
}

func (m *memSyncStores) StateAtRoomsStream(ctx context.Context, positions map[string]int64) (map[string][]core.Event, error) {
	return m.StateAtRoomsStreamMembers(ctx, positions, nil)
}

func (m *memSyncStores) StateAtRoomsStreamMembers(
	_ context.Context, positions map[string]int64, members []string,
) (map[string][]core.Event, error) {
	m.calls = append(m.calls, fmt.Sprintf("%v %v", positions, members))

	if m.err != nil {
		return nil, m.err
	}

	out := map[string][]core.Event{}
	for roomID := range positions {
		out[roomID] = m.state[roomID]
	}

	return out, nil
}

func (m *memSyncStores) SyncPosition(context.Context) (int64, error) { return 42, m.err }

func (m *memSyncStores) PendingToDevice(_ context.Context, _, _ string, since int64, limit int) ([]core.ToDeviceEvent, error) {
	if m.err != nil {
		return nil, m.err
	}

	var out []core.ToDeviceEvent

	for _, ev := range m.pending {
		if ev.StreamPos > since && len(out) < limit {
			out = append(out, ev)
		}
	}

	return out, nil
}

func (m *memSyncStores) DeleteToDevice(_ context.Context, ids []int64) error {
	m.deleted = append(m.deleted, ids...)

	return m.err
}

func (m *memSyncStores) DeviceListChangesFor(context.Context, string, int64, int64) ([]string, error) {
	return m.changed, m.err
}

func (m *memSyncStores) OneTimeKeyCounts(context.Context, string, string) (map[string]int, error) {
	return m.otk, m.err
}

func (m *memSyncStores) UnusedFallbackKeyTypes(context.Context, string, string) ([]string, error) {
	return m.fallbacks, m.err
}

func syncEvent(id, sender string, stream int64) core.Event {
	return core.Event{EventID: id, Sender: sender, StreamOrdering: stream}
}

// State покинутых комнат - на момент выхода и только для отрисовки; state
// обрезанного timeline - на начало отданных событий, при lazy_load_members -
// отдельным запросом с членством отправителей порции.
func TestSyncServiceState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := func(s string) *string { return &s }
	st := &memSyncStores{state: map[string][]core.Event{
		"!left": {
			{Type: "m.room.name", StateKey: key("")},
			{Type: "m.room.power_levels", StateKey: key("")},
			{Type: "m.room.member", StateKey: key(alice)},
			{Type: "m.room.member", StateKey: key(bob)},
		},
		"!join":   {{Type: "m.room.create", StateKey: key("")}},
		"!invite": {{Type: "m.room.topic", StateKey: key("")}},
	}}
	svc := core.NewSyncService(st, st, st)

	data := core.SyncData{
		Memberships: []core.RoomMembership{
			{RoomID: "!left", Membership: "leave", LeaveStream: 7},
			{RoomID: "!ban", Membership: "ban"},
			{RoomID: "!join", Membership: "join"},
			{RoomID: "!invite", Membership: "invite"},
		},
		Timeline: map[string][]core.Event{
			"!join":   {syncEvent("$1", bob, 10), syncEvent("$2", carol, 11), syncEvent("$3", bob, 12)},
			"!invite": {syncEvent("$4", bob, 20), syncEvent("$5", bob, 21), syncEvent("$6", bob, 22)},
			"!short":  {syncEvent("$7", bob, 30)},
		},
		State: map[string][]core.Event{},
	}

	svc.FillLeaveState(ctx, &data, alice)

	if len(data.State["!left"]) != 2 || len(st.calls) != 1 || st.calls[0] != "map[!left:7] []" {
		t.Fatalf("state выхода: %v, запросы %v", data.State["!left"], st.calls)
	}

	// Incremental без новых комнат state не трогает.
	svc.FillStateAtStart(ctx, &data, alice, 5, 2, true)

	if len(st.calls) != 1 {
		t.Fatalf("incremental без новых комнат: %v", st.calls)
	}

	data.State = nil
	svc.FillStateAtStart(ctx, &data, alice, 0, 2, true)

	want := []string{"map[!left:7] []", "map[!invite:20] []", "map[!join:10] [" + alice + " " + carol + " " + bob + "]"}
	if fmt.Sprint(st.calls) != fmt.Sprint(want) || len(data.State["!join"]) != 1 || len(data.State["!invite"]) != 1 {
		t.Fatalf("state на начало timeline: %v, state %v", st.calls, data.State)
	}

	// Новая комната в incremental - как при первом sync; сбой - state как был.
	st.calls, st.err = nil, errors.New("db down")
	data.NewlyJoined = map[string]bool{"!join": true}

	svc.FillStateAtStart(ctx, &data, alice, 5, 2, false)
	svc.FillLeaveState(ctx, &data, alice)

	if len(st.calls) != 2 || st.calls[0] != "map[!join:10] []" || len(data.State["!join"]) != 1 {
		t.Fatalf("новая комната при сбое: %v %v", st.calls, data.State)
	}
}

// Очередь to-device, изменения ключей и счетчики ключей.
func TestSyncServiceQueues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := &memSyncStores{
		pending: []core.ToDeviceEvent{{ID: 1, StreamPos: 5}, {ID: 2, StreamPos: 9}, {ID: 3, StreamPos: 7}},
		changed: []string{bob}, otk: map[string]int{"signed_curve25519": 3},
	}
	svc := core.NewSyncService(st, st, st)

	if pos, err := svc.Position(ctx); err != nil || pos != 42 {
		t.Fatalf("позиция: %d %v", pos, err)
	}

	pending, ids, maxPos, err := svc.PendingToDevice(ctx, alice, "DEV", 5)
	if err != nil || len(pending) != 2 || fmt.Sprint(ids) != "[2 3]" || maxPos != 9 {
		t.Fatalf("to-device: %v %v %d %v", pending, ids, maxPos, err)
	}

	if err := svc.DeleteToDevice(ctx, ids); err != nil || fmt.Sprint(st.deleted) != "[2 3]" {
		t.Fatalf("удаление: %v %v", st.deleted, err)
	}

	if changed, err := svc.DeviceListChanges(ctx, alice, 1, 9); err != nil || fmt.Sprint(changed) != "["+bob+"]" {
		t.Fatalf("изменения ключей: %v %v", changed, err)
	}

	if otk, err := svc.OneTimeKeyCounts(ctx, alice, "DEV"); err != nil || otk["signed_curve25519"] != 3 {
		t.Fatalf("otk: %v %v", otk, err)
	}

	if types, err := svc.UnusedFallbackKeyTypes(ctx, alice, "DEV"); err != nil || types == nil || len(types) != 0 {
		t.Fatalf("fallback keys: %v %v", types, err)
	}

	st.err = errors.New("db down")

	if _, _, _, err := svc.PendingToDevice(ctx, alice, "DEV", 0); err == nil {
		t.Fatal("сбой очереди без ошибки")
	}

	if _, err := svc.UnusedFallbackKeyTypes(ctx, alice, "DEV"); err == nil {
		t.Fatal("сбой ключей без ошибки")
	}
}
