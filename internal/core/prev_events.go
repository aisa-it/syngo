package core

import "context"

// prevEventsWindow - сколько последних событий комнаты смотреть при выборе
// prev_events; maxPrevEvents - предел prev_events у нового события (как в Synapse).
const (
	prevEventsWindow = 50
	maxPrevEvents    = 10
)

// ForwardExtremityReader - хранилище с forward extremities комнаты.
type ForwardExtremityReader interface {
	// ForwardExtremityEvents - события forward extremities комнаты, новые первыми,
	// не больше limit.
	ForwardExtremityEvents(ctx context.Context, roomID string, limit int) ([]Event, error)
}

// prevEvents возвращает prev_events нового события: forward extremities комнаты
// (таблица room_forward_extremities), не больше maxPrevEvents. Комната без
// записанных extremities (до миграции 0044) или хранилище без них - события из
// последнего окна, на которые не ссылается ни одно другое событие окна. Раньше
// брались последние 20 событий целиком: новое сообщение ссылалось на состояние,
// которое пир получил вне DAG (ответ send_join), и Synapse отвергал его («failed to
// fetch prev events»).
func (s *RoomService) prevEvents(ctx context.Context, roomID string) ([]Event, error) {
	if reader, ok := s.store.(ForwardExtremityReader); ok {
		extremities, err := reader.ForwardExtremityEvents(ctx, roomID, maxPrevEvents)
		if err != nil {
			return nil, err
		}

		if len(extremities) > 0 {
			return extremities, nil
		}
	}

	latest, err := s.store.LatestEvents(ctx, roomID, prevEventsWindow)
	if err != nil {
		return nil, err
	}

	return forwardExtremities(latest, maxPrevEvents), nil
}

// forwardExtremities выбирает из событий (новые первыми) те, на которые не ссылаются
// prev_events других событий набора; не больше limit, порядок сохраняется.
func forwardExtremities(events []Event, limit int) []Event {
	referenced := make(map[string]struct{}, len(events))

	for i := range events {
		for _, id := range events[i].PrevEvents {
			referenced[id] = struct{}{}
		}
	}

	out := make([]Event, 0, limit)

	for i := range events {
		if _, ok := referenced[events[i].EventID]; ok {
			continue
		}

		out = append(out, events[i])
		if len(out) == limit {
			break
		}
	}

	if len(out) == 0 && len(events) > 0 {
		out = append(out, events[0])
	}

	return out
}
