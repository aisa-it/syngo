package core

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/pushrules"
)

// Список уведомлений GET /notifications. Логика перенесена
// из HTTP-обработчика.

// Пределы одного запроса /notifications: события вычисляются на лету, поэтому
// скан ограничен; если предел достигнут, клиент продолжит по next_token.
const (
	notificationsBatch     = 200
	notificationsScanLimit = 1000
)

// NotificationsStore - кандидаты в уведомления и данные для push-правил.
type NotificationsStore interface {
	PushEvalStore

	// NotificationCandidates - события комнат пользователя после его входа, не
	// от него и не удаленные, с позицией меньше before (0 - без границы), от
	// новых к старым.
	NotificationCandidates(ctx context.Context, userID string, before int64, limit int) ([]Event, error)
}

// Notification - уведомление: событие, действия правил и прочитано ли оно.
type Notification struct {
	Event  Event
	Result pushrules.Result
	Read   bool
}

// NotificationsPage - страница уведомлений; Next - позиция продолжения, если
// Exhausted=false.
type NotificationsPage struct {
	Items     []Notification
	Next      int64
	Exhausted bool
}

// NotificationsService - доменный сервис /notifications.
type NotificationsService struct {
	store NotificationsStore
	reads *ReadStates
}

// NewNotificationsService создает сервис; reads - отметки прочтения (поле read).
func NewNotificationsService(store NotificationsStore, reads *ReadStates) *NotificationsService {
	return &NotificationsService{store: store, reads: reads}
}

// List - до limit уведомлений пользователя с позиции from (0 - с новейших).
// Уведомления - события, для которых push-правила пользователя дали notify
// (раньше - все чужие m.room.message, в том числе в заглушенных комнатах); идем
// от новых к старым, как Synapse. onlyHighlight - только с highlight.
func (s *NotificationsService) List(
	ctx context.Context, userID string, from int64, limit int, onlyHighlight bool,
) (NotificationsPage, error) {
	var (
		page    NotificationsPage
		scanned int
	)

	before := from

	for len(page.Items) < limit && scanned < notificationsScanLimit {
		batch, err := s.store.NotificationCandidates(ctx, userID, before, notificationsBatch)
		if err != nil {
			return NotificationsPage{}, err
		}

		if len(batch) == 0 {
			page.Exhausted = true

			break
		}

		rooms := map[string]bool{}
		roomIDs := []string{}

		for i := range batch {
			if !rooms[batch[i].RoomID] {
				rooms[batch[i].RoomID] = true
				roomIDs = append(roomIDs, batch[i].RoomID)
			}
		}

		evaluator, err := NewPushEvaluator(ctx, s.store, []string{userID}, roomIDs)
		if err != nil {
			return NotificationsPage{}, fmt.Errorf("notifications rules: %w", err)
		}

		for i := range batch {
			scanned++
			before = batch[i].StreamOrdering

			res := evaluator.Evaluate(userID, batch[i])
			if res.Notify && (!onlyHighlight || res.Highlight) {
				page.Items = append(page.Items, Notification{Event: batch[i], Result: res})
			}

			if len(page.Items) >= limit {
				break
			}
		}

		if len(batch) < notificationsBatch && len(page.Items) < limit {
			page.Exhausted = true

			break
		}
	}

	page.Next = before
	s.markRead(ctx, userID, page.Items)

	return page, nil
}

// markRead выставляет Read по отметкам прочтения комнат (раньше всегда false).
func (s *NotificationsService) markRead(ctx context.Context, userID string, items []Notification) {
	if len(items) == 0 || s.reads == nil {
		return
	}

	seen := map[string]bool{}
	roomIDs := []string{}

	for i := range items {
		if !seen[items[i].Event.RoomID] {
			seen[items[i].Event.RoomID] = true
			roomIDs = append(roomIDs, items[i].Event.RoomID)
		}
	}

	read := s.reads.For(ctx, userID, roomIDs)

	for i := range items {
		ev := items[i].Event
		state := read[ev.RoomID]
		items[i].Read = state != nil && state.IsRead(ev.StreamOrdering, ThreadRoot(ev.Content))
	}
}
