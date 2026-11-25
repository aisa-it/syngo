package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Федеративные запросы: GET /_matrix/federation/v1/query/profile и
// /query/directory отвечают за наших пользователей и алиасы. Без них Synapse при
// приглашении нашего пользователя получал M_UNRECOGNIZED и не отправлял
// приглашение.

// registerFederationQueryRoutes подключает /query/* (под requireFederation).
func (s *Server) registerFederationQueryRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/query/profile", s.handleFederationQueryProfile)
	r.Get("/_matrix/federation/v1/query/directory", s.handleFederationQueryDirectory)
}

// handleFederationQueryProfile - GET /_matrix/federation/v1/query/profile: профиль
// нашего пользователя, field ограничивает ответ одним полем (как on_profile_query
// Synapse: чужой пользователь - 400, несуществующий - 404 M_NOT_FOUND).
func (s *Server) handleFederationQueryProfile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	userID := query.Get("user_id")
	if userID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingParam, "Missing user_id")

		return
	}

	if !isValidUserID(userID) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user_id")

		return
	}

	if domainOf(userID) != s.cfg.Matrix.ServerName {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "User is not hosted on this homeserver")

		return
	}

	if !s.profileReady(w) {
		return
	}

	profile, err := s.profiles.Profile(r.Context(), userID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Profile was not found")

			return
		}

		s.writeRoomError(w, err)

		return
	}

	field := query.Get("field")
	resp := map[string]string{}

	if (field == "" || field == "displayname") && profile.DisplayName != "" {
		resp["displayname"] = profile.DisplayName
	}

	if (field == "" || field == "avatar_url") && profile.AvatarURL != "" {
		resp["avatar_url"] = profile.AvatarURL
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleFederationQueryDirectory - GET /_matrix/federation/v1/query/directory:
// room_id и servers нашего алиаса; чужой алиас - 400, неизвестный - 404 M_NOT_FOUND.
func (s *Server) handleFederationQueryDirectory(w http.ResponseWriter, r *http.Request) {
	alias := r.URL.Query().Get("room_alias")
	if alias == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingParam, "Missing room_alias")

		return
	}

	if !strings.HasPrefix(alias, "#") || domainOf(alias) == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room_alias")

		return
	}

	if domainOf(alias) != s.cfg.Matrix.ServerName {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Room alias is not hosted on this homeserver")

		return
	}

	roomID, err := s.rooms.ResolveAlias(r.Context(), alias)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Room alias not found")

			return
		}

		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "servers": []string{s.cfg.Matrix.ServerName}})
}
