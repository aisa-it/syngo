package api

import (
	"encoding/json"
	"net/http"
)

// Element при старте создаёт sync-фильтр: POST /user/{userId}/filter.
// Без него клиент падает в «MatrixClient sync state => ERROR».
// Хранение - в core.FilterService.

// handleCreateFilter — POST /_matrix/client/v3/user/{userId}/filter.
func (s *Server) handleCreateFilter(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if userID := normalizeUserID(pathParamValue(r, "userId")); userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot create filters for another user")

		return
	}

	var body map[string]any
	if !decodeRoomBody(w, r, &body) {
		return
	}

	raw, err := json.Marshal(body)
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Invalid filter")

		return
	}

	filterID, err := s.filters.Create(r.Context(), session.UserID, raw)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to store filter")

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"filter_id": filterID})
}

// handleGetFilter — GET /_matrix/client/v3/user/{userId}/filter/{filterId}.
func (s *Server) handleGetFilter(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if userID := normalizeUserID(pathParamValue(r, "userId")); userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot read filters of another user")

		return
	}

	raw, err := s.filters.Get(r.Context(), session.UserID, pathParamValue(r, "filterId"))
	if err != nil {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Filter not found")

		return
	}

	// raw — JSON, сохранённый нами же в user_filters; проверяем валидность и
	// отдаём как JSON (gosec G705 требует явного обоснования, содержимое не HTML).
	if !json.Valid(raw) {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Stored filter is corrupted")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw) //nolint:gosec // JSON из БД, Content-Type: application/json
}
