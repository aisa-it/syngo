package api

import (
	"io"
	"net/http"
)

// Pushers: проверки и сохранение - в core.PushersService,
// здесь разбор запроса и ответ.

// ErrCodeResourceLimitExceeded - превышен лимит ресурса (pushers на пользователя).
const ErrCodeResourceLimitExceeded = "M_RESOURCE_LIMIT_EXCEEDED"

// handleGetPushers — GET /_matrix/client/v3/pushers.
func (s *Server) handleGetPushers(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.pushersService == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	list, err := s.pushersService.List(r.Context(), session.UserID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: pushers read failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"pushers": list})
}

// handleSetPusher — POST /_matrix/client/v3/pushers/set.
// kind=null удаляет pusher; иначе запись проверяется и сохраняется.
func (s *Server) handleSetPusher(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.pushersService == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRoomsBodySize))
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not read request body")

		return
	}

	if err := s.pushersService.Set(r.Context(), session.UserID, session.DeviceID, body); err != nil {
		if writeRequestError(w, err) {
			return
		}

		s.log.ErrorContext(r.Context(), "api: pusher set failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}
