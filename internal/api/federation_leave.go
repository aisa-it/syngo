package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/store"
)

// handleMakeLeave — GET /_matrix/federation/v1/make_leave/{roomId}/{userId}.
func (s *Server) handleMakeLeave(w http.ResponseWriter, r *http.Request) {
	s.writeMembershipTemplate(w, r, "leave", "make_leave reply")
}

// handleSendLeave — PUT /_matrix/federation/v1|v2/send_leave/{roomId}/{eventId}.
//
// Только member(leave) за себя от сервера отправителя с авторизацией по
// auth_events и текущему состоянию.
func (s *Server) handleSendLeave(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	var object map[string]any
	if !decodeFederationBody(w, r, &object) {
		return
	}

	incoming, ok := s.acceptIncomingJoinLeave(w, r, roomID, object, "leave")
	if !ok {
		return
	}

	if err := s.applyMembershipEvent(r.Context(), incoming.pdu, incoming.origin, incoming.version); err != nil {
		s.log.ErrorContext(r.Context(), "api: send_leave apply failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply leave")

		return
	}

	// Состояние на leave, forward extremities и room_state.
	if err := s.materializeRoomsState(r.Context(), []store.FedPDU{incoming.pdu}); err != nil {
		s.log.ErrorContext(r.Context(), "api: send_leave state failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply leave")

		return
	}

	// Выход по федерации меняет комнату — будим long-poll (как send_join).
	s.eventsWritten(r.Context())

	// v2 (room v3+) отвечает пустым объектом, v1 — [200, {}]. Synapse зовёт v2 и на 404
	// не откатывается.
	if strings.Contains(r.URL.Path, "/v2/send_leave/") {
		writeJSON(w, http.StatusOK, map[string]any{})

		return
	}

	writeJSON(w, http.StatusOK, []any{200, map[string]any{}})
}

// registerFederationLeaveRoutes подключает S2S leave.
func (s *Server) registerFederationLeaveRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/make_leave/{roomId}/{userId}", s.handleMakeLeave)
	r.Put("/_matrix/federation/v1/send_leave/{roomId}/{eventId}", s.handleSendLeave)
	r.Put("/_matrix/federation/v2/send_leave/{roomId}/{eventId}", s.handleSendLeave)
}
