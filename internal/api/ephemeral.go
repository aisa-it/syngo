package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// presenceRequest — тело PUT /presence/{userId}/status.
type presenceRequest struct {
	Presence  string `json:"presence"`
	StatusMsg string `json:"status_msg"`
}

// handlePresence обрабатывает PUT/GET /presence/{userId}/status.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user id")

		return
	}

	if r.Method == http.MethodGet {
		event := s.ephemeral.Presence(userID)
		writeJSON(w, http.StatusOK, presenceView(event, s.ephemeral.Now().UnixMilli()).Content)

		return
	}

	// Менять можно только свой статус.
	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot set another user's presence")

		return
	}

	var req presenceRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if !core.ValidPresence(req.Presence) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "presence must be online, offline or unavailable")

		return
	}

	// Изменение статуса будит /sync участников общих комнат.
	if s.ephemeral.SetPresence(userID, req.Presence, req.StatusMsg) {
		s.notifyPresence(r.Context(), userID)
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// typingRequest — тело PUT /rooms/{roomId}/typing/{userId}.
type typingRequest struct {
	Typing  bool `json:"typing"`
	Timeout int  `json:"timeout"`
}

// handleTyping обрабатывает PUT /rooms/{roomId}/typing/{userId}.
func (s *Server) handleTyping(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot set another user's typing state")

		return
	}

	roomID := roomIDParam(r)

	// Права: печатать может только участник комнаты.
	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Room storage is not configured")

		return
	}

	// Event с пустым id вернёт ErrNotFound после проверки членства — значит, пользователь
	// в комнате; ErrForbidden означает «не участник».
	if _, err := s.rooms.Event(r.Context(), userID, roomID, ""); isForbidden(err) {
		s.writeRoomError(w, err)

		return
	}

	var req typingRequest
	if !decodeBody(w, r, &req) {
		return
	}

	s.ephemeral.SetTyping(roomID, userID, req.Typing)
	s.notifySync()
	s.sendTypingEDU(r.Context(), roomID, userID, req.Typing)

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleReceipt обрабатывает POST /rooms/{roomId}/receipt/{receiptType}/{eventId}.
func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID := roomIDParam(r)
	eventID := eventIDParam(r)
	receiptType := pathParamValue(r, "receiptType")

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Room storage is not configured")

		return
	}

	switch receiptType {
	case core.ReceiptRead, core.ReceiptReadPrivate, core.ReceiptFullyRead:
	default:
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Unknown receipt type")

		return
	}

	// Тело необязательно: {"thread_id": "..."} для отметки в треде.
	var body struct {
		ThreadID string `json:"thread_id"`
	}

	if raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10)); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if json.Unmarshal(raw, &body) != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON body")

			return
		}
	}

	// Права: receipt может ставить только участник комнаты (проверяем по событию).
	if _, err := s.rooms.Event(r.Context(), session.UserID, roomID, eventID); err != nil {
		s.writeRoomError(w, err)

		return
	}

	if !s.setReadMarker(w, r, session.UserID, roomID, receiptType, body.ThreadID, eventID) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// setReadMarker сохраняет отметку прочтения (core.ReadMarkersService):
// m.fully_read - в room account data (не receipt, по спецификации),
// m.read.private - видна только владельцу, m.read - всей комнате. false - ответ с
// ошибкой уже записан.
func (s *Server) setReadMarker(w http.ResponseWriter, r *http.Request, userID, roomID, markerType, threadID, eventID string) bool {
	if err := s.readMarkers.Set(r.Context(), userID, roomID, markerType, threadID, eventID); err != nil {
		s.log.ErrorContext(r.Context(), "api: fully_read write failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return false
	}

	// m.fully_read и m.read.private видны только владельцу, m.read - всей комнате
	// и серверам ее участников.
	if markerType == core.ReceiptFullyRead || markerType == core.ReceiptReadPrivate {
		s.notifySyncUser(userID)
	} else {
		s.notifySync()
		s.sendReceiptEDU(r.Context(), roomID, userID, eventID, threadID)
	}

	return true
}

// handleReadMarkers обрабатывает POST /rooms/{roomId}/read_markers (m.fully_read + receipt).
func (s *Server) handleReadMarkers(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID := roomIDParam(r)

	var req struct {
		FullyRead   string `json:"m.fully_read"`
		Read        string `json:"m.read"`
		ReadPrivate string `json:"m.read.private"`
	}

	if !decodeBody(w, r, &req) {
		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Room storage is not configured")

		return
	}

	markers := []struct{ kind, eventID string }{
		{core.ReceiptFullyRead, req.FullyRead},
		{core.ReceiptRead, req.Read},
		{core.ReceiptReadPrivate, req.ReadPrivate},
	}

	// Каждое событие проверяется отдельно: участник комнаты и событие этой комнаты.
	for _, m := range markers {
		if m.eventID == "" {
			continue
		}

		if _, err := s.rooms.Event(r.Context(), session.UserID, roomID, m.eventID); err != nil {
			s.writeRoomError(w, err)

			return
		}
	}

	for _, m := range markers {
		if m.eventID != "" && !s.setReadMarker(w, r, session.UserID, roomID, m.kind, "", m.eventID) {
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// registerEphemeralRoutes подключает presence/typing/receipts (нужен access token).
func (s *Server) registerEphemeralRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/presence/{userId}/status", s.handlePresence)
		r.Put("/presence/{userId}/status", s.handlePresence)
		r.Put("/rooms/{roomId}/typing/{userId}", s.handleTyping)
		r.Post("/rooms/{roomId}/receipt/{receiptType}/{eventId}", s.handleReceipt)
		r.Post("/rooms/{roomId}/read_markers", s.handleReadMarkers)
	})
}

// isForbidden сообщает, что ошибка — отказ по правам.
func isForbidden(err error) bool {
	return errors.Is(err, core.ErrForbidden)
}
