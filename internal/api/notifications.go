package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// handleNotifications — GET /_matrix/client/v3/notifications.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.notifications == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	query := r.URL.Query()

	since := int64(0)

	if raw := query.Get("from"); raw != "" {
		decoded, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid from")

			return
		}

		since = decoded
	}

	limit := 50

	if raw := query.Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	if limit <= 0 || limit > 100 {
		limit = 50
	}

	page, err := s.notifications.List(r.Context(), session.UserID, since, limit, query.Get("only") == "highlight")
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: notifications failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	notifications := make([]map[string]any, 0, len(page.Items))

	for i := range page.Items {
		ev := page.Items[i].Event

		notifications = append(notifications, map[string]any{
			"actions": page.Items[i].Result.Actions,
			"event":   newEventView(ev),
			"read":    page.Items[i].Read,
			"room_id": ev.RoomID,
			"ts":      ev.OriginServerTS,
		})
	}

	resp := map[string]any{"notifications": notifications}
	if !page.Exhausted {
		resp["next_token"] = strconv.FormatInt(page.Next, 10)
	}

	writeJSON(w, http.StatusOK, resp)
}

// registerNotificationsRoutes подключает /notifications.
func (s *Server) registerNotificationsRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/notifications", s.handleNotifications)
	})
}
