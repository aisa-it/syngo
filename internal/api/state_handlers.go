package api

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/aisa-it/syngo/internal/core"
)

// handleRoomState — GET /rooms/{roomId}/state: всё состояние комнаты.
func (s *Server) handleRoomState(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	events, err := s.rooms.StateEvents(r.Context(), session.UserID, roomIDParam(r))
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	out := make([]map[string]any, 0, len(events))

	for i := range events {
		ev := events[i]

		item := map[string]any{
			"type":             ev.Type,
			"state_key":        stateKeyOf(ev),
			"event_id":         ev.EventID,
			"sender":           ev.Sender,
			"origin_server_ts": ev.OriginServerTS,
			"content":          ev.Content,
			"room_id":          ev.RoomID,
		}
		out = append(out, item)
	}

	writeJSON(w, http.StatusOK, out)
}

// handleRoomStateEvent — GET /rooms/{roomId}/state/{eventType}/{stateKey}.
func (s *Server) handleRoomStateEvent(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	// state_key может отсутствовать в пути (Element зовёт /state/m.room.encryption/):
	// chi отдаёт пустую строку — трактуем как state_key "".
	ev, err := s.rooms.StateEvent(r.Context(), session.UserID, roomIDParam(r),
		pathParamValue(r, "eventType"), pathParamValue(r, "stateKey"))
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// format=event - событие целиком, а не только content (спека v1.13, Complement
	// TestRoomState).
	if r.URL.Query().Get("format") == "event" {
		writeJSON(w, http.StatusOK, toEventViews([]core.Event{ev})[0])

		return
	}

	writeJSON(w, http.StatusOK, ev.Content)
}

// handleSendState — PUT /rooms/{roomId}/state/{eventType}/{stateKey}.
func (s *Server) handleSendState(w http.ResponseWriter, r *http.Request) {
	s.sendState(w, r, pathParamValue(r, "stateKey"))
}

// handleSendStateEmptyKey — PUT /rooms/{roomId}/state/{eventType} (пустой state_key).
func (s *Server) handleSendStateEmptyKey(w http.ResponseWriter, r *http.Request) {
	s.sendState(w, r, "")
}

// sendState — общая часть записи state-события.
func (s *Server) sendState(w http.ResponseWriter, r *http.Request, stateKey string) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&raw); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return
	}

	content, err := normalizeJSON(raw)
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON")

		return
	}

	roomID := roomIDParam(r)
	eventType := pathParamValue(r, "eventType")

	// txn id у state-эндпоинта не обязателен. По телу дедуплицируется только
	// значение, совпадающее с текущим состоянием; A -> B -> A - новое
	// событие.
	txnID := ""

	eventID, duplicate, err := s.rooms.SendState(r.Context(), session.UserID, session.DeviceID,
		roomID, eventType, stateKey, txnID, content)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// То же значение уже в состоянии: события нет, рассылать и будить некого.
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]string{"event_id": eventID})

		return
	}

	// State-события тоже проходят push-правила: например,
	// приглашение через PUT /state/m.room.member и tombstone.
	s.dispatchStoredPush(r.Context(), roomID, eventID)
	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
}

// moderationRequest — тело kick/ban/unban.
type moderationRequest struct {
	UserID string `json:"user_id"`
	Reason string `json:"reason"`
}

// handleKick — POST /rooms/{roomId}/kick.
func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	s.moderate(w, r, "leave")
}

// handleBan — POST /rooms/{roomId}/ban.
func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	s.moderate(w, r, "ban")
}

// handleUnban — POST /rooms/{roomId}/unban.
func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	s.moderate(w, r, "leave")
}

// moderate выполняет kick/ban/unban через m.room.member.
func (s *Server) moderate(w http.ResponseWriter, r *http.Request, membership string) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var req moderationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&req); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return
	}

	if !isValidUserID(req.UserID) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user_id")

		return
	}

	roomID := roomIDParam(r)

	txnID := "" // повторный kick/ban - новое действие, а не дубль

	if _, _, err := s.rooms.SendMembership(r.Context(), session.UserID, session.DeviceID,
		roomID, req.UserID, membership, req.Reason, txnID); err != nil {
		s.writeRoomError(w, err)

		return
	}

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{})
}

// handleRedact — PUT /rooms/{roomId}/redact/{eventId}/{txnId}.
func (s *Server) handleRedact(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var req struct {
		Reason string `json:"reason"`
	}

	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&req)

	roomID := roomIDParam(r)
	eventID, _, err := s.rooms.Redact(r.Context(), session.UserID, session.DeviceID,
		roomID, eventIDParam(r), req.Reason, pathParamValue(r, "txnId"))
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
}

// decodePathParam раскодирует percent-encoded параметр пути (Element шлёт
// state_key вида @user%3Aserver).
func decodePathParam(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}

	return raw
}

// stateKeyOf — state_key события (пустая строка, если nil).
func stateKeyOf(ev core.Event) string {
	if ev.StateKey == nil {
		return ""
	}

	return *ev.StateKey
}
