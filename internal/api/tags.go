package api

import (
	"encoding/json"
	"net/http"
)

// Теги комнат: логика - в core.AccountDataService,
// здесь разбор запроса и ответ.

// tagsContent — content account data m.tag.
type tagsContent struct {
	Tags map[string]json.RawMessage `json:"tags"`
}

// handleGetTags — GET /user/{userId}/rooms/{roomId}/tags.
func (s *Server) handleGetTags(w http.ResponseWriter, r *http.Request) {
	userID, roomID, ok := s.tagsTarget(w, r)
	if !ok {
		return
	}

	tags, err := s.accountData.Tags(r.Context(), userID, roomID)
	if err != nil {
		s.writeAccountDataError(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, tagsContent{Tags: tags})
}

// handlePutTag — PUT /user/{userId}/rooms/{roomId}/tags/{tag}.
func (s *Server) handlePutTag(w http.ResponseWriter, r *http.Request) {
	userID, roomID, ok := s.tagsTarget(w, r)
	if !ok {
		return
	}

	tag := pathParamValue(r, "tag")
	if tag == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "tag is required")

		return
	}

	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&raw); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return
	}

	err := s.accountData.SetTag(r.Context(), userID, roomID, tag, raw)
	if writeRequestError(w, err) {
		return
	}

	s.writeTagsResult(w, r, userID, err)
}

// handleDeleteTag — DELETE /user/{userId}/rooms/{roomId}/tags/{tag}.
func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	userID, roomID, ok := s.tagsTarget(w, r)
	if !ok {
		return
	}

	s.writeTagsResult(w, r, userID, s.accountData.DeleteTag(r.Context(), userID, roomID, pathParamValue(r, "tag")))
}

// tagsTarget проверяет владельца и возвращает userID/roomID.
func (s *Server) tagsTarget(w http.ResponseWriter, r *http.Request) (userID, roomID string, ok bool) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return "", "", false
	}

	if s.accountData == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return "", "", false
	}

	userID = normalizeUserID(pathParamValue(r, "userId"))
	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot access another user's tags")

		return "", "", false
	}

	var found bool

	roomID, found = pathParam(r, "roomId")
	if !found {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return "", "", false
	}

	return userID, roomID, true
}

// writeTagsResult отвечает на изменение тега и будит /sync владельца.
func (s *Server) writeTagsResult(w http.ResponseWriter, r *http.Request, userID string, err error) {
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: tags write failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	s.notifySyncUser(userID) // m.tag выдается в /sync - будим его владельца

	writeJSON(w, http.StatusOK, map[string]any{})
}
