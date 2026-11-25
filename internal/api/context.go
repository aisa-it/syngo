package api

import (
	"net/http"
	"strconv"

	"github.com/aisa-it/syngo/internal/core"
)

// handleEventContext — GET /rooms/{roomId}/context/{eventId}.
//
// Element запрашивает контекст при переходе к сообщению (permalink). Отдаём
// событие, по limit событий до/после и текущее состояние комнаты.
// Событие и окружение - с учетом history_visibility.
func (s *Server) handleEventContext(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	limit := 10

	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		if parsed > 100 {
			parsed = 100
		}

		limit = parsed
	}

	roomID := roomIDParam(r)
	eventID := eventIDParam(r)

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	event, err := s.rooms.Event(r.Context(), session.UserID, roomID, eventID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// history_visibility: скрытое событие - 404, как в /event.
	vis, filter := s.roomVisibility(r.Context(), session.UserID, roomID)
	if filter && !visibleTo(vis, session.UserID, &event) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found")

		return
	}

	// Курсор Messages эксклюзивный: позиция самого события дает
	// окружение без него. С +1/-1 событие повторялось в events_before и
	// events_after.
	before, err := s.rooms.Messages(r.Context(), session.UserID, roomID,
		formatContextToken(event.StreamOrdering), "b", limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	after, err := s.rooms.Messages(r.Context(), session.UserID, roomID,
		formatContextToken(event.StreamOrdering), "f", limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// Окружение фильтруется по той же видимости. state - текущее состояние
	// комнаты, которое участник и так получает через /state и /sync, поэтому
	// его не фильтруем: иначе пропали бы m.room.create и члены комнаты.
	if filter {
		before.Events, _ = filterVisible(vis, session.UserID, before.Events)
		after.Events, _ = filterVisible(vis, session.UserID, after.Events)
	}

	state, err := s.rooms.StateEvents(r.Context(), session.UserID, roomID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	beforeViews, afterViews := eventViews(before.Events), eventViews(after.Events)
	target := []eventView{newEventView(event)}
	s.attachRedactions(r.Context(), beforeViews)
	s.attachRedactions(r.Context(), afterViews)
	s.attachRedactions(r.Context(), target)
	s.attachTransactionIDs(r.Context(), session, beforeViews, target, afterViews)

	var roomVis map[string]core.Visibility
	if filter {
		roomVis = map[string]core.Visibility{roomID: vis}
	}

	// Агрегации связей - одним проходом на все три среза.
	s.attachAggregations(r.Context(), session.UserID, roomVis, beforeViews, target, afterViews)

	writeJSON(w, http.StatusOK, map[string]any{
		"start":         before.End,
		"end":           after.End,
		"events_before": beforeViews,
		"event":         target[0],
		"events_after":  afterViews,
		"state":         eventViews(state),
	})
}

// formatContextToken готовит токен пагинации для Messages.
func formatContextToken(stream int64) string {
	return "s" + strconv.FormatInt(stream, 10)
}

// eventViews превращает события в клиентское представление.
func eventViews(events []core.Event) []eventView {
	out := make([]eventView, 0, len(events))

	for i := range events {
		out = append(out, newEventView(events[i]))
	}

	return out
}
