package core

import (
	"context"
	"fmt"
)

// Поиск сообщений (POST /search). Логика перенесена из HTTP-обработчика.

// SearchStore - полнотекстовый поиск m.room.message по body.
type SearchStore interface {
	// SearchMessagesPage ищет в комнатах rooms с учетом границ видимости: страница
	// результатов и общее число совпадений.
	SearchMessagesPage(ctx context.Context, rooms map[string]Visibility, term string, page SearchPage) ([]Event, int, error)
}

// SearchPage - страница поиска: размер, смещение и порядок.
type SearchPage struct {
	Limit  int
	Offset int
	// Recent - order_by=recent (сначала новые); иначе по релевантности (rank).
	Recent bool
}

// SearchQuery - параметры POST /search room_events.
type SearchQuery struct {
	Term string
	// Rooms - filter.rooms (сужает выборку в пределах комнат пользователя).
	Rooms []string
	Page  SearchPage
}

// SearchResult - найденные события и общее число совпадений (count).
type SearchResult struct {
	Events []Event
	Count  int
}

// SearchService - доменный сервис поиска.
type SearchService struct {
	store       SearchStore
	memberships MembershipStore
	visibility  *VisibilityService
}

// NewSearchService создает сервис; visibility nil - история не ограничивается.
func NewSearchService(store SearchStore, memberships MembershipStore, visibility *VisibilityService) *SearchService {
	return &SearchService{store: store, memberships: memberships, visibility: visibility}
}

// SearchRoomEvents ищет сообщения только в комнатах, где пользователь состоит;
// filterRooms сужает выборку в пределах этих комнат.
func (s *SearchService) SearchRoomEvents(
	ctx context.Context, userID, term string, filterRooms []string, limit int,
) ([]Event, error) {
	result, err := s.Search(ctx, userID, SearchQuery{Term: term, Rooms: filterRooms, Page: SearchPage{Limit: limit}})

	return result.Events, err
}

// Search - SearchRoomEvents со страницей и порядком (next_batch и order_by в POST
// /search).
func (s *SearchService) Search(ctx context.Context, userID string, q SearchQuery) (SearchResult, error) {
	if q.Term == "" {
		return SearchResult{}, requestError(ErrInvalidParam, "search_term is required")
	}

	memberships, err := s.memberships.MembershipRooms(ctx, userID)
	if err != nil {
		return SearchResult{}, fmt.Errorf("search rooms: %w", err)
	}

	joined := joinedFromMemberships(memberships)

	// filter.rooms может сузить выборку, но только в пределах joined.
	if len(q.Rooms) > 0 {
		allowed := make(map[string]struct{}, len(joined))
		for _, roomID := range joined {
			allowed[roomID] = struct{}{}
		}

		filtered := make([]string, 0, len(q.Rooms))

		for _, roomID := range q.Rooms {
			if _, ok := allowed[roomID]; ok {
				filtered = append(filtered, roomID)
			}
		}

		joined = filtered
	}

	// history_visibility: границы видимости уходят в SQL, иначе
	// отфильтрованная после LIMIT выдача была бы неполной.
	var vis map[string]Visibility
	if s.visibility != nil {
		vis = s.visibility.Cutoffs(ctx, userID, joined)
	}

	visible := make(map[string]Visibility, len(joined))
	for _, roomID := range joined {
		visible[roomID] = vis[roomID]
	}

	events, count, err := s.store.SearchMessagesPage(ctx, visible, q.Term, q.Page)
	if err != nil {
		return SearchResult{}, err
	}

	return SearchResult{Events: events, Count: count}, nil
}
