package core

import (
	"context"
	"errors"
)

// RelationsStore — чтение связей события (m.relates_to) из хранилища.
type RelationsStore interface {
	// Relations отдаёт связи события после позиции from (0 — с начала).
	Relations(ctx context.Context, roomID, eventID, relType, eventType string, from int64, dir string, limit int) ([]Event, error)
}

// Relations — события, ссылающиеся на eventID (реакции, правки, треды).
// Читать может только участник комнаты, как и остальные чтения.
func (s *RoomService) Relations(
	ctx context.Context, userID, roomID, eventID, relType, eventType string, from int64, dir string, limit int,
) ([]Event, string, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return nil, "", err
	}

	store, ok := s.store.(RelationsStore)
	if !ok {
		return nil, "", ErrRelationsUnsupported
	}

	if dir != "f" {
		dir = "b"
	}

	events, err := store.Relations(ctx, roomID, eventID, relType, eventType, from, dir, limit)
	if err != nil {
		return nil, "", err
	}

	// next_batch отдаём только если страница полная: иначе клиент сделает ещё один
	// пустой запрос. Курсор эксклюзивный и зависит от направления:
	// dir=b листает назад от минимальной позиции страницы, dir=f —
	// вперёд от максимальной.
	nextBatch := ""

	if limit > 0 && len(events) >= limit {
		next := events[len(events)-1].StreamOrdering

		if dir == "b" {
			for i := range events {
				if events[i].StreamOrdering < next {
					next = events[i].StreamOrdering
				}
			}
		}

		nextBatch = formatStreamToken(next)
	}

	return events, nextBatch, nil
}

// ErrRelationsUnsupported — хранилище не умеет связи события (например, тестовый фейк).
var ErrRelationsUnsupported = errors.New("core: relations are not supported by this store")

// ThreadsQuery — параметры списка тредов комнаты (GET /rooms/{roomId}/threads).
type ThreadsQuery struct {
	RoomID string
	UserID string
	// Participated — только треды, где пользователь автор корня или ответа (include=participated).
	Participated bool
	// VisibleBefore и VisibleFrom — границы видимости истории для пользователя:
	// видно событие с позицией < VisibleBefore или >= VisibleFrom (нули — видно все).
	VisibleBefore int64
	VisibleFrom   int64
	// From — эксклюзивный курсор: позиция последнего ответа треда, с которого
	// продолжать (0 — с самых свежих).
	From  int64
	Limit int
}

// ThreadsStore — список тредов комнаты из хранилища.
type ThreadsStore interface {
	// Threads отдает корни тредов, самые недавно обновленные первыми, и позицию
	// последнего ответа каждого (курсор следующей страницы).
	Threads(ctx context.Context, q ThreadsQuery) ([]Event, []int64, error)
}

// Threads — корни тредов комнаты по последнему ответу. Читать может
// только участник, как /relations. next_batch — только у полной страницы.
func (s *RoomService) Threads(ctx context.Context, q ThreadsQuery) ([]Event, string, error) {
	if err := s.requireJoined(ctx, q.RoomID, q.UserID); err != nil {
		return nil, "", err
	}

	store, ok := s.store.(ThreadsStore)
	if !ok {
		return nil, "", ErrRelationsUnsupported
	}

	events, latest, err := store.Threads(ctx, q)
	if err != nil {
		return nil, "", err
	}

	nextBatch := ""
	if q.Limit > 0 && len(events) >= q.Limit && len(latest) == len(events) {
		nextBatch = formatStreamToken(latest[len(latest)-1])
	}

	return events, nextBatch, nil
}
