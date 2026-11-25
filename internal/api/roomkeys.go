package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
)

// Серверный бэкап ключей E2EE (/room_keys/*). Element через
// него восстанавливает историю на новом устройстве и докачивает ключ отдельной
// сессии (GET /room_keys/keys/{roomId}/{sessionId}), когда сообщение не
// расшифровывается. Логика - в core.KeyBackupService, здесь разбор
// запроса и ответ.

const errCodeWrongRoomKeysVersion = "M_WRONG_ROOM_KEYS_VERSION"

// roomKeysSession - сессия запроса к бэкапу; false - ответ с ошибкой уже записан.
func (s *Server) roomKeysSession(w http.ResponseWriter, r *http.Request) (core.Session, bool) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return core.Session{}, false
	}

	if s.keyBackup == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return core.Session{}, false
	}

	return session, true
}

// handleGetRoomKeysVersion — GET /room_keys/version и /room_keys/version/{version}.
func (s *Server) handleGetRoomKeysVersion(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	version, err := s.keyBackup.Version(r.Context(), session.UserID, pathParamValue(r, "version"))
	if s.roomKeysError(w, err) {
		return
	}

	writeJSON(w, http.StatusOK, version)
}

// handleCreateRoomKeysVersion — POST /room_keys/version.
func (s *Server) handleCreateRoomKeysVersion(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	var body struct {
		Algorithm string          `json:"algorithm"`
		AuthData  json.RawMessage `json:"auth_data"`
	}

	if !decodeRoomBody(w, r, &body) {
		return
	}

	version, err := s.keyBackup.CreateVersion(r.Context(), session.UserID, body.Algorithm, body.AuthData)
	if s.roomKeysError(w, err) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"version": version})
}

// handleUpdateRoomKeysVersion — PUT /room_keys/version/{version}: меняется только
// auth_data, algorithm должен совпадать с текущим.
func (s *Server) handleUpdateRoomKeysVersion(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	var body struct {
		Algorithm string          `json:"algorithm"`
		AuthData  json.RawMessage `json:"auth_data"`
		Version   string          `json:"version"`
	}

	if !decodeRoomBody(w, r, &body) {
		return
	}

	err := s.keyBackup.UpdateVersion(r.Context(), session.UserID, pathParamValue(r, "version"), core.VersionUpdate{
		Algorithm: body.Algorithm, AuthData: body.AuthData, Version: body.Version,
	})
	if s.roomKeysError(w, err) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleDeleteRoomKeysVersion — DELETE /room_keys/version/{version}.
func (s *Server) handleDeleteRoomKeysVersion(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	if s.roomKeysError(w, s.keyBackup.DeleteVersion(r.Context(), session.UserID, pathParamValue(r, "version"))) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handlePutRoomKeys — PUT /room_keys/keys[/{roomId}[/{sessionId}]]?version=.
// Запись разрешена только в текущую версию: иначе 403 M_WRONG_ROOM_KEYS_VERSION,
// по которому клиент узнает, что бэкап сменили на другом устройстве.
func (s *Server) handlePutRoomKeys(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	roomID, sessionID := pathParamValue(r, "roomId"), pathParamValue(r, "sessionId")
	keys := core.RoomKeys{}

	switch {
	case sessionID != "":
		var key json.RawMessage
		if !decodeRoomBody(w, r, &key) {
			return
		}

		keys[roomID] = map[string]json.RawMessage{sessionID: key}
	case roomID != "":
		var body struct {
			Sessions map[string]json.RawMessage `json:"sessions"`
		}

		if !decodeRoomBody(w, r, &body) {
			return
		}

		keys[roomID] = body.Sessions
	default:
		var body struct {
			Rooms map[string]struct {
				Sessions map[string]json.RawMessage `json:"sessions"`
			} `json:"rooms"`
		}

		if !decodeRoomBody(w, r, &body) {
			return
		}

		for rid, room := range body.Rooms {
			keys[rid] = room.Sessions
		}
	}

	stat, err := s.keyBackup.PutKeys(r.Context(), session.UserID, r.URL.Query().Get("version"), keys)

	var wrong *core.WrongBackupVersionError
	if errors.As(err, &wrong) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"errcode": errCodeWrongRoomKeysVersion, "error": "Wrong backup version",
			"current_version": wrong.Current,
		})

		return
	}

	if s.roomKeysError(w, err) {
		return
	}

	writeJSON(w, http.StatusOK, stat)
}

// handleGetRoomKeys — GET /room_keys/keys[/{roomId}[/{sessionId}]]?version=.
func (s *Server) handleGetRoomKeys(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	roomID, sessionID := pathParamValue(r, "roomId"), pathParamValue(r, "sessionId")

	keys, err := s.keyBackup.Keys(r.Context(), session.UserID, r.URL.Query().Get("version"), roomID, sessionID)
	if s.roomKeysError(w, err) {
		return
	}

	switch {
	case sessionID != "":
		key, found := keys[roomID][sessionID]
		if !found {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "No key found for session")

			return
		}

		writeJSON(w, http.StatusOK, key)
	case roomID != "":
		sessions := keys[roomID]
		if sessions == nil {
			sessions = map[string]json.RawMessage{}
		}

		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
	default:
		rooms := map[string]any{}
		for rid, sessions := range keys {
			rooms[rid] = map[string]any{"sessions": sessions}
		}

		writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
	}
}

// handleDeleteRoomKeys — DELETE /room_keys/keys[/{roomId}[/{sessionId}]]?version=.
func (s *Server) handleDeleteRoomKeys(w http.ResponseWriter, r *http.Request) {
	session, ok := s.roomKeysSession(w, r)
	if !ok {
		return
	}

	stat, err := s.keyBackup.DeleteKeys(r.Context(), session.UserID, r.URL.Query().Get("version"),
		pathParamValue(r, "roomId"), pathParamValue(r, "sessionId"))
	if s.roomKeysError(w, err) {
		return
	}

	writeJSON(w, http.StatusOK, stat)
}

// roomKeysError пишет ответ на ошибку сервиса бэкапа; true - ответ записан.
func (s *Server) roomKeysError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case writeRequestError(w, err):
	case errors.Is(err, core.ErrRoomKeysNotFound):
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "No backup found")
	default:
		s.log.Error("api: room keys", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
	}

	return true
}
