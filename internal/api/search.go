package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// searchRequest — тело POST /search (подмножество CS-API v1.11).
type searchRequest struct {
	SearchCategories struct {
		RoomEvents struct {
			SearchTerm string `json:"search_term"`
			OrderBy    string `json:"order_by"`
			Filter     struct {
				Rooms []string `json:"rooms"`
				Limit int      `json:"limit"`
			} `json:"filter"`
			// EventContext - окружение результатов (before_limit/after_limit).
			EventContext *struct {
				BeforeLimit *int `json:"before_limit"`
				AfterLimit  *int `json:"after_limit"`
			} `json:"event_context"`
		} `json:"room_events"`
	} `json:"search_categories"`
}

// searchResult — один результат поиска.
type searchResult struct {
	Rank    int            `json:"rank"`
	Result  eventView      `json:"result"`
	Context *searchContext `json:"context,omitempty"`
}

// searchContext — окружение результата (event_context в запросе).
type searchContext struct {
	EventsBefore []eventView `json:"events_before"`
	EventsAfter  []eventView `json:"events_after"`
	Start        string      `json:"start,omitempty"`
	End          string      `json:"end,omitempty"`
}

// searchResponse — ответ POST /search.
type searchResponse struct {
	SearchCategories struct {
		RoomEvents struct {
			Count      int            `json:"count"`
			Highlights []string       `json:"highlights"`
			Results    []searchResult `json:"results"`
			NextBatch  string         `json:"next_batch,omitempty"`
		} `json:"room_events"`
	} `json:"search_categories"`
}

// handleSearch — POST /_matrix/client/v3/search.
//
// Ищет m.room.message по body только в комнатах, где пользователь состоит
// (core.SearchService).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.search == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var req searchRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	roomEvents := req.SearchCategories.RoomEvents
	term := roomEvents.SearchTerm

	// next_batch - смещение в выдаче (непрозрачно для клиента); order_by=recent -
	// сначала новые, иначе по релевантности (Complement TestSearch).
	offset := 0

	if raw := r.URL.Query().Get("next_batch"); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid next_batch")

			return
		}

		offset = parsed
	}

	limit := roomEvents.Filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}

	result, err := s.search.Search(r.Context(), session.UserID, core.SearchQuery{
		Term: term, Rooms: roomEvents.Filter.Rooms,
		Page: core.SearchPage{Limit: limit, Offset: offset, Recent: roomEvents.OrderBy == "recent"},
	})
	events := result.Events

	if err != nil {
		if writeRequestError(w, err) {
			return
		}

		s.log.ErrorContext(r.Context(), "api: search failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	resp := searchResponse{}
	resp.SearchCategories.RoomEvents.Results = make([]searchResult, 0, len(events))
	resp.SearchCategories.RoomEvents.Highlights = []string{term}

	// Агрегации связей (m.thread, m.replace, m.reference) - как в остальной выдаче
	// событий.
	views := eventViews(events)

	roomIDs := make([]string, 0, len(events))
	seen := make(map[string]bool, len(events))

	for i := range events {
		if !seen[events[i].RoomID] {
			seen[events[i].RoomID] = true
			roomIDs = append(roomIDs, events[i].RoomID)
		}
	}

	s.attachAggregations(r.Context(), session.UserID, s.visibility(r.Context(), session.UserID, roomIDs), views)

	for i := range views {
		item := searchResult{Rank: i, Result: views[i]}

		if roomEvents.EventContext != nil {
			item.Context = s.searchEventContext(r.Context(), session.UserID, events[i],
				contextLimit(roomEvents.EventContext.BeforeLimit), contextLimit(roomEvents.EventContext.AfterLimit))
		}

		resp.SearchCategories.RoomEvents.Results = append(resp.SearchCategories.RoomEvents.Results, item)
	}

	resp.SearchCategories.RoomEvents.Count = result.Count

	// next_batch - пока страница полная, как в Synapse (клиент узнает конец по
	// неполной странице), и пока есть еще совпадения по count.
	if next := offset + len(events); len(events) > 0 && (len(events) >= limit || next < result.Count) {
		resp.SearchCategories.RoomEvents.NextBatch = strconv.Itoa(next)
	}

	writeJSON(w, http.StatusOK, resp)
}

// contextLimit - before_limit/after_limit event_context: по умолчанию 5, как в
// спеке, не больше 20.
func contextLimit(raw *int) int {
	if raw == nil {
		return 5
	}

	return min(max(*raw, 0), 20)
}

// searchEventContext - события до и после результата поиска с учетом видимости.
func (s *Server) searchEventContext(ctx context.Context, userID string, event core.Event, before, after int) *searchContext {
	out := &searchContext{EventsBefore: []eventView{}, EventsAfter: []eventView{}}
	token := formatContextToken(event.StreamOrdering)
	vis, filter := s.roomVisibility(ctx, userID, event.RoomID)

	page := func(dir string, limit int) []eventView {
		if limit == 0 {
			return []eventView{}
		}

		res, err := s.rooms.Messages(ctx, userID, event.RoomID, token, dir, limit)
		if err != nil {
			s.log.WarnContext(ctx, "api: search context failed", "room_id", event.RoomID, "error", err)

			return []eventView{}
		}

		if dir == "b" {
			out.Start = res.End
		} else {
			out.End = res.End
		}

		if filter {
			res.Events, _ = filterVisible(vis, userID, res.Events)
		}

		return eventViews(res.Events)
	}

	out.EventsBefore = page("b", before)
	out.EventsAfter = page("f", after)

	return out
}

// registerSearchRoutes подключает поиск.
func (s *Server) registerSearchRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/search", s.handleSearch)
	})
}
