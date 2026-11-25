package api

import (
	"encoding/json"
	"net/http"
)

// upgradeRequest — тело POST /rooms/{roomId}/upgrade.
type upgradeRequest struct {
	NewVersion string `json:"new_version"`
}

// upgradeResponse — тело успешного upgrade: id новой комнаты.
type upgradeResponse struct {
	ReplacementRoom string `json:"replacement_room"`
}

// handleUpgrade обрабатывает POST /rooms/{roomId}/upgrade.
//
// Логика - core.UpgradeRoom (порядок Synapse: права на tombstone до создания
// новой комнаты). Здесь - разбор запроса, лог некритичных сбоев и приглашения
// локальных участников в новую комнату, если она не публичная.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID := roomIDParam(r)
	if roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	var req upgradeRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	// new_version обязателен по спецификации (раньше пустой молча давал v10);
	// неизвестная версия - 400 M_UNSUPPORTED_ROOM_VERSION до проверок комнаты.
	if req.NewVersion == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingParam, "Missing new_version")

		return
	}

	version := 0
	if err := parseRoomVersion(req.NewVersion, &version); err != nil {
		s.writeRoomError(w, err)

		return
	}

	result, err := s.rooms.UpgradeRoom(r.Context(), session.UserID, session.DeviceID, roomID, version)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	for _, warnErr := range result.Warnings {
		s.log.WarnContext(r.Context(), "api: upgrade step failed",
			"room_id", roomID, "replacement_room", result.ReplacementRoom, "error", warnErr)
	}

	if !result.AlreadyUpgraded {
		s.inviteUpgradeMembers(r, session.UserID, roomID, result.ReplacementRoom)
	}

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, upgradeResponse{ReplacementRoom: result.ReplacementRoom})
}

// inviteUpgradeMembers приглашает локальных участников старой комнаты в новую,
// если в нее нельзя войти без приглашения (join_rule не public): иначе после
// upgrade invite-only комнаты участники не могут перейти по tombstone. Удаленные
// участники не приглашаются: федеративный invite при upgrade вне скоупа.
func (s *Server) inviteUpgradeMembers(r *http.Request, sender, oldRoomID, newRoomID string) {
	if joinRule, err := s.rooms.StateEvent(r.Context(), sender, newRoomID, "m.room.join_rules", ""); err == nil {
		var body struct {
			JoinRule string `json:"join_rule"`
		}

		if json.Unmarshal(joinRule.Content, &body) == nil && body.JoinRule == "public" {
			return
		}
	}

	members, err := s.rooms.JoinedMembers(r.Context(), sender, oldRoomID)
	if err != nil {
		return
	}

	for userID := range members {
		if userID == sender || inviteDestination(userID) != s.cfg.Matrix.ServerName {
			continue
		}

		if _, inviteErr := s.rooms.Invite(r.Context(), sender, newRoomID, userID); inviteErr != nil {
			s.log.WarnContext(r.Context(), "api: upgrade invite failed",
				"room_id", newRoomID, "user_id", userID, "error", inviteErr)
		}
	}
}
