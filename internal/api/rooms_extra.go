package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// pathParam достаёт параметр пути и декодирует percent-encoding.
//
// chi матчит по RawPath, поэтому клиент, закодировавший `!room:server` или `#alias:server`,
// иначе получил бы сырой «%21room…». Ошибка декодирования — 400 M_INVALID_PARAM.
func pathParam(r *http.Request, name string) (string, bool) {
	decoded, err := decodePathValue(chi.URLParam(r, name))
	if err != nil {
		return "", false
	}

	return decoded, true
}

// decodePathValue раскодирует percent-encoded значение пути (%24event%3Aserver).
func decodePathValue(raw string) (string, error) {
	return url.PathUnescape(raw)
}

// pathParamValue — единственная точка чтения параметра пути: chi матчит по RawPath
// и отдаёт сырое значение, поэтому любой параметр (user id, room id, tag, txnId…)
// обязан проходить через декодирование. На битом percent-encoding возвращаем сырое
// значение: формат всё равно проверит обработчик.
func pathParamValue(r *http.Request, name string) string {
	return decodePathParam(chi.URLParam(r, name))
}

// handleRoomMembers — GET /rooms/{roomId}/members.
func (s *Server) handleRoomMembers(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = parsed
	}

	// `at` по спеке — sync-токен: участники на эту позицию (состояние на позиции
	// stream_ordering, Complement TestGetRoomMembersAtPoint). Иначе
	// `at` - курсор пагинации по state_key.
	query := r.URL.Query()
	cursor := query.Get("at")

	// Токен Synapse, оставшийся у клиента после переезда (sync-токен или токен
	// пагинации): позицию старого сервера не перевести, отдаем текущих участников.
	// Раньше он читался как курсор по state_key, и список приходил пустым.
	if core.IsSynapseSyncToken(cursor) || core.IsSynapsePageToken(cursor) {
		cursor = ""
	}

	var (
		events []core.Event
		next   string
		err    error
	)

	if position, decodeErr := s.syncTokens.Decode(cursor); cursor != "" && decodeErr == nil {
		events, next, err = s.rooms.MembersAt(r.Context(), session.UserID, roomID, position)
	} else {
		events, next, err = s.rooms.MembersPage(r.Context(), session.UserID, roomID, cursor, limit)
	}

	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// membership / not_membership — фильтры по значению membership из content.
	want := query.Get("membership")
	exclude := query.Get("not_membership")

	if want != "" || exclude != "" {
		filtered := make([]core.Event, 0, len(events))

		for i := range events {
			membership := membershipOfEvent(events[i])

			if want != "" && membership != want {
				continue
			}

			if exclude != "" && membership == exclude {
				continue
			}

			filtered = append(filtered, events[i])
		}

		events = filtered
	}

	chunk := make([]eventView, 0, len(events))
	for i := range events {
		chunk = append(chunk, newEventView(events[i]))
	}

	s.attachRedactions(r.Context(), chunk)
	resp := map[string]any{"chunk": chunk}
	if next != "" {
		resp["next_batch"] = next
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleJoinedMembers — GET /rooms/{roomId}/joined_members.
func (s *Server) handleJoinedMembers(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	members, err := s.rooms.JoinedMembers(r.Context(), session.UserID, roomID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	out := make(map[string]map[string]string, len(members))
	for userID, profile := range members {
		entry := map[string]string{}
		if profile.DisplayName != "" {
			entry["display_name"] = profile.DisplayName
		}

		if profile.AvatarURL != "" {
			entry["avatar_url"] = profile.AvatarURL
		}

		out[userID] = entry
	}

	writeJSON(w, http.StatusOK, map[string]any{"joined": out})
}

// handleRoomAliases — GET /rooms/{roomId}/aliases.
func (s *Server) handleRoomAliases(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	aliases, err := s.rooms.AliasesByRoom(r.Context(), session.UserID, roomID)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	if aliases == nil {
		aliases = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
}

// handleDirectoryResolve — GET /directory/room/{alias} (без авторизации, как в спеке).
func (s *Server) handleDirectoryResolve(w http.ResponseWriter, r *http.Request) {
	alias, ok := pathParam(r, "alias")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid alias")

		return
	}

	// Алиас чужого сервера - через его /query/directory; сбой
	// федерации - 404, как неизвестный алиас.
	if s.isRemote(alias) {
		roomID, servers, err := s.remoteAlias(r.Context(), alias)
		if err != nil {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Room alias not found")

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "servers": servers})

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

// aliasCreateRequest — тело PUT /directory/room/{alias}.
type aliasCreateRequest struct {
	RoomID string `json:"room_id"`
}

// handleDirectoryCreate — PUT /directory/room/{alias}.
func (s *Server) handleDirectoryCreate(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	alias, ok := pathParam(r, "alias")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid alias")

		return
	}

	var req aliasCreateRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	if strings.TrimSpace(req.RoomID) == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "room_id is required")

		return
	}

	if err := s.rooms.SetAlias(r.Context(), session.UserID, req.RoomID, alias); err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleDirectoryDelete — DELETE /directory/room/{alias}.
func (s *Server) handleDirectoryDelete(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	alias, ok := pathParam(r, "alias")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid alias")

		return
	}

	if err := s.rooms.DeleteAlias(r.Context(), session.UserID, alias); err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// membershipOfEvent достаёт membership из content m.room.member.
func membershipOfEvent(event core.Event) string {
	var body struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(event.Content, &body); err != nil {
		return ""
	}

	return body.Membership
}

// eventIDParam — event_id из пути с декодированием (Element шлёт `%24event…`).
func eventIDParam(r *http.Request) string {
	value, _ := pathParam(r, "eventId")

	return value
}

// roomIDParam — room_id из пути с декодированием (Element шлёт `%21room%3A…`;
// без url.PathUnescape получаем 403/404).
func roomIDParam(r *http.Request) string {
	value, _ := pathParam(r, "roomId")

	return value
}
