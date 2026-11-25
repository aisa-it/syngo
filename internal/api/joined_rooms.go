package api

import "net/http"

// handleJoinedRooms — GET /_matrix/client/v3/joined_rooms.
//
// Клиенты (в том числе Element) запрашивают список комнат пользователя при старте.
// Спека: {"joined_rooms": [...]}. Берём только membership=join (выборка - в
// core.MembershipService).
func (s *Server) handleJoinedRooms(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.memberships == nil {
		writeJSON(w, http.StatusOK, map[string]any{"joined_rooms": []string{}})

		return
	}

	rooms, err := s.memberships.JoinedRooms(r.Context(), session.UserID)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read rooms")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"joined_rooms": rooms})
}
