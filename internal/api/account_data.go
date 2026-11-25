package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
)

// Account data: логика - в core.AccountDataService,
// здесь разбор запроса и ответ.

// parseAccountDataPath проверяет, что пользователь читает/пишет только свои данные.
func (s *Server) parseAccountDataPath(w http.ResponseWriter, r *http.Request, withRoom bool) (core.AccountDataKey, bool) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return core.AccountDataKey{}, false
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot access another user's account data")

		return core.AccountDataKey{}, false
	}

	key := core.AccountDataKey{UserID: userID, EventType: pathParamValue(r, "eventType")}

	if withRoom {
		key.RoomID = roomIDParam(r)
	}

	return key, true
}

// handleGetAccountData — GET /user/{userId}/account_data/{type}.
func (s *Server) handleGetAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, false)
	if !ok {
		return
	}

	s.writeAccountData(w, r, key)
}

// handlePutAccountData — PUT /user/{userId}/account_data/{type}.
func (s *Server) handlePutAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, false)
	if !ok {
		return
	}

	s.saveAccountData(w, r, key)
}

// handleGetRoomAccountData — GET /user/{userId}/rooms/{roomId}/account_data/{type}.
func (s *Server) handleGetRoomAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, true)
	if !ok {
		return
	}

	s.writeAccountData(w, r, key)
}

// handlePutRoomAccountData — PUT /user/{userId}/rooms/{roomId}/account_data/{type}.
func (s *Server) handlePutRoomAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, true)
	if !ok {
		return
	}

	s.saveAccountData(w, r, key)
}

// writeAccountData отдаёт content или 404, если записи нет.
func (s *Server) writeAccountData(w http.ResponseWriter, r *http.Request, key core.AccountDataKey) {
	if s.accountData == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	content, err := s.accountData.Get(r.Context(), key)
	if err != nil {
		s.writeAccountDataError(w, r, err)

		return
	}

	writeJSON(w, http.StatusOK, content)
}

// writeAccountDataError - отказ сервиса с текстом; нет записи: 404 M_NOT_FOUND;
// сбой хранилища: 500 (раньше любой сбой выглядел для клиента как отсутствие
// данных).
func (s *Server) writeAccountDataError(w http.ResponseWriter, r *http.Request, err error) {
	if writeRequestError(w, err) {
		return
	}

	if errors.Is(err, core.ErrNotFound) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Account data not found")

		return
	}

	s.log.ErrorContext(r.Context(), "api: account data failed", "error", err)
	writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
}

// saveAccountData разбирает тело и сохраняет content.
func (s *Server) saveAccountData(w http.ResponseWriter, r *http.Request, key core.AccountDataKey) {
	if s.accountData == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	// Серверный тип - 405 до разбора тела (как раньше).
	if writeRequestError(w, core.CheckWritable(key.EventType)) {
		return
	}

	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&raw); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return
	}

	if err := s.accountData.Set(r.Context(), key, raw); err != nil {
		if writeRequestError(w, err) {
			return
		}

		s.log.ErrorContext(r.Context(), "api: account data write failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")

		return
	}

	s.notifySyncUser(key.UserID) // account data выдаётся в /sync — будим его владельца

	writeJSON(w, http.StatusOK, map[string]any{})
}

// accountDataEvent — событие account data в /sync: только type и content (раньше
// отдавалось как событие комнаты с пустыми event_id, sender, room_id).
type accountDataEvent struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// accountDataEvents — раздел account_data в /sync.
type accountDataEvents struct {
	Events []accountDataEvent `json:"events"`
}

// fillAccountData добавляет account data в ответ /sync (см.
// core.AccountDataService.SyncChanges).
func (s *Server) fillAccountData(
	ctx context.Context, resp *syncResponse, userID string, since, upper int64, newlyJoined map[string]bool,
) {
	if s.accountData == nil {
		return
	}

	global, perRoom, err := s.accountData.SyncChanges(ctx, userID, since, upper, newlyJoined)
	if err != nil {
		s.log.WarnContext(ctx, "api: account data for sync failed", "error", err)

		return
	}

	toViews := func(events map[string][]byte) []accountDataEvent {
		out := make([]accountDataEvent, 0, len(events))
		for eventType, content := range events {
			out = append(out, accountDataEvent{Type: eventType, Content: json.RawMessage(content)})
		}

		return out
	}

	resp.AccountData.Events = toViews(global)

	for roomID, events := range perRoom {
		room, ok := resp.Rooms.Join[roomID]
		if !ok {
			continue
		}

		room.AccountData.Events = toViews(events)
		resp.Rooms.Join[roomID] = room
	}
}

// handleDeleteAccountData — DELETE /user/{userId}/account_data/{type}.
func (s *Server) handleDeleteAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, false)
	if !ok {
		return
	}

	s.deleteAccountData(w, r, key)
}

// handleDeleteRoomAccountData — DELETE /user/{userId}/rooms/{roomId}/account_data/{type}.
func (s *Server) handleDeleteRoomAccountData(w http.ResponseWriter, r *http.Request) {
	key, ok := s.parseAccountDataPath(w, r, true)
	if !ok {
		return
	}

	s.deleteAccountData(w, r, key)
}

// deleteAccountData удаляет запись; отсутствие — 404 M_NOT_FOUND, серверный тип - 405.
func (s *Server) deleteAccountData(w http.ResponseWriter, r *http.Request, key core.AccountDataKey) {
	if s.accountData == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	if err := s.accountData.Delete(r.Context(), key); err != nil {
		s.writeAccountDataError(w, r, err)

		return
	}

	s.notifySyncUser(key.UserID)

	writeJSON(w, http.StatusOK, map[string]any{})
}
