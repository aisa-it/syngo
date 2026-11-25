package core_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// extremityStore - in-memory хранилище с заданными forward extremities.
type extremityStore struct {
	*coretest.MemRoomStore

	extremities []string
	limit       int
}

func (s *extremityStore) ForwardExtremityEvents(_ context.Context, _ string, limit int) ([]core.Event, error) {
	s.limit = limit

	out := make([]core.Event, 0, len(s.extremities))

	events := s.Events()

	for i := range events {
		for _, id := range s.extremities {
			if events[i].EventID == id {
				out = append(out, events[i])
			}
		}
	}

	return out, nil
}

// prev_events локального события - forward extremities хранилища (не больше 10),
// без записанных extremities - окно последних событий.
func TestPrevEventsFromForwardExtremities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := &extremityStore{MemRoomStore: coretest.NewMemRoomStore()}
	svc := core.NewRoomService(store, "test.local").WithClock(func() time.Time {
		return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	})

	roomID := createRoom(t, svc)

	// Без extremities: prev - последнее событие комнаты.
	first, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "t1", []byte(`{"msgtype":"m.text","body":"1"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	events := store.Events()
	genesisLast := events[len(events)-2].EventID

	if got := prevOf(t, store, first); !reflect.DeepEqual(got, []string{genesisLast}) {
		t.Fatalf("prev без extremities = %v, ожидали [%s]", got, genesisLast)
	}

	// С extremities: prev - они, а не последнее событие.
	store.extremities = []string{genesisLast}

	second, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "t2", []byte(`{"msgtype":"m.text","body":"2"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if got := prevOf(t, store, second); !reflect.DeepEqual(got, []string{genesisLast}) || store.limit != 10 {
		t.Fatalf("prev = %v (limit %d), ожидали [%s] и limit 10", got, store.limit, genesisLast)
	}
}

func prevOf(t *testing.T, store *extremityStore, eventID string) []string {
	t.Helper()

	events := store.Events()

	for i := range events {
		if events[i].EventID == eventID {
			return events[i].PrevEvents
		}
	}

	t.Fatalf("событие %s не найдено", eventID)

	return nil
}
