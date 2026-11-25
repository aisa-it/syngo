package core

import "context"

// Видимость истории по history_visibility. Логика перенесена из
// HTTP-слоя.

// Visibility - какие события комнаты видит пользователь: позиция < Before (события
// до установки текущего history_visibility, по умолчанию shared) или >= From.
type Visibility struct {
	Before int64
	From   int64
}

// Visible сообщает, видно ли событие с позицией pos.
func (v Visibility) Visible(pos int64) bool {
	return pos < v.Before || pos >= v.From
}

// VisibleTo - видно ли событие пользователю: собственные события членства видны всегда.
func (v Visibility) VisibleTo(userID string, event *Event) bool {
	if event.Type == "m.room.member" && event.StateKey != nil && *event.StateKey == userID {
		return true
	}

	return v.Visible(event.StreamOrdering)
}

// FilterVisible оставляет видимые пользователю события; второй результат - было
// ли что-то скрыто.
func (v Visibility) FilterVisible(userID string, events []Event) ([]Event, bool) {
	kept := events[:0:0]
	hidden := false

	for i := range events {
		if v.VisibleTo(userID, &events[i]) {
			kept = append(kept, events[i])
		} else {
			hidden = true
		}
	}

	return kept, hidden
}

// closedVisibility - история закрыта целиком (позиция заведомо больше любой).
var closedVisibility = Visibility{From: 1 << 62}

// VisibilityStore - границы видимости по history_visibility.
type VisibilityStore interface {
	VisibilityCutoffs(ctx context.Context, userID string, roomIDs []string) (map[string]Visibility, error)
}

// VisibilityService - доменный сервис видимости истории.
type VisibilityService struct {
	store VisibilityStore
	// warn получает сбой хранилища (история тогда закрывается).
	warn func(ctx context.Context, err error)
}

// NewVisibilityService создает сервис; warn может быть nil.
func NewVisibilityService(store VisibilityStore, warn func(ctx context.Context, err error)) *VisibilityService {
	return &VisibilityService{store: store, warn: warn}
}

// Cutoffs - границы видимости комнат для пользователя; нет комнат - nil.
//
// Консервативно: при ошибке хранилища не отдаем ничего сверх уже разрешенного -
// вызывающий трактует отсутствие записи как «видно все», поэтому история
// закрывается целиком.
func (s *VisibilityService) Cutoffs(ctx context.Context, userID string, roomIDs []string) map[string]Visibility {
	if len(roomIDs) == 0 {
		return nil
	}

	vis, err := s.store.VisibilityCutoffs(ctx, userID, roomIDs)
	if err != nil {
		if s.warn != nil {
			s.warn(ctx, err)
		}

		out := make(map[string]Visibility, len(roomIDs))
		for _, roomID := range roomIDs {
			out[roomID] = closedVisibility
		}

		return out
	}

	return vis
}
