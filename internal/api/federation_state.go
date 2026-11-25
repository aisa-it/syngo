package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// handleFederationEvent — GET /_matrix/federation/v1/event/{eventId}.
//
// Только серверу с участником комнаты события; невидимое ему по
// history_visibility событие отдается отредактированным.
func (s *Server) handleFederationEvent(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	eventID, ok := pathParam(r, "eventId")
	if !ok || eventID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid event id")

		return
	}

	pdu, err := s.db.EventPDU(r.Context(), eventID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found")

			return
		}

		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read event")

		return
	}

	origin, ok := s.requireServerInRoom(w, r, pdu.RoomID)
	if !ok {
		return
	}

	pdus, err := s.visiblePDUs(r.Context(), pdu.RoomID, origin, []string{eventID})
	if err != nil || len(pdus) == 0 {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found")

		return
	}

	// DEBUG: точный JSON отдаваемого PDU — для сверки с проверкой подписи в Synapse.
	s.log.DebugContext(r.Context(), "api: federation /event reply",
		"event_id", eventID, "b64", base64.RawStdEncoding.EncodeToString(pdus[0]))

	// GET /_matrix/federation/v1/event/{eventId} отдаёт обычный JSON-объект
	// (Transaction: origin, origin_server_ts, pdus) — обёртка [200, …] есть только у
	// send_join/send_leave v1. Synapse читает transaction_data["pdus"]; на массиве PDU
	// приходит битым и проверка подписи падает.
	writeFederationJSON(w, s.federationTransaction(pdus))
}

// federationTransaction - ответ в формате Transaction спеки (/event, /backfill).
func (s *Server) federationTransaction(pdus []json.RawMessage) map[string]any {
	return map[string]any{
		"origin":           s.cfg.Matrix.ServerName,
		"origin_server_ts": time.Now().UnixMilli(),
		"pdus":             pdus,
	}
}

// handleFederationState — GET /_matrix/federation/v1/state/{roomId} (v1-формат).
func (s *Server) handleFederationState(w http.ResponseWriter, r *http.Request) {
	s.writeFederationState(w, r, false)
}

// handleFederationStateIDs — GET /state_ids/{roomId}: только идентификаторы.
func (s *Server) handleFederationStateIDs(w http.ResponseWriter, r *http.Request) {
	s.writeFederationState(w, r, true)
}

// writeFederationState отдаёт состояние комнаты (события или только id).
//
// С event_id - состояние до этого события (как Synapse: само событие не входит),
// без него - текущее (совместимость, по спеке параметр обязателен). auth_chain -
// рекурсивная цепочка всех событий состояния.
func (s *Server) writeFederationState(w http.ResponseWriter, r *http.Request, idsOnly bool) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok || roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	if _, ok := s.requireServerInRoom(w, r, roomID); !ok {
		return
	}

	eventIDs, found, err := s.federationStateIDs(r.Context(), roomID, r.URL.Query().Get("event_id"))
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read state")

		return
	}

	if !found {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found in room")

		return
	}

	authIDs, err := s.db.AuthChainIDs(r.Context(), eventIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	// ВАЖНО: /state и /state_ids отдают обычный JSON-объект, без v1-обёртки [200, …]
	// (обёртка нужна только send_join/send_leave v1). Synapse на массиве
	// отвечает «Failed to parse response» и зависает в resync.
	if idsOnly {
		writeJSON(w, http.StatusOK, map[string]any{
			"pdu_ids":        eventIDs,
			"auth_chain_ids": authIDs,
		})

		return
	}

	pdus, err := s.federationPDUs(r.Context(), eventIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read state")

		return
	}

	authChain, err := s.federationPDUs(r.Context(), authIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	writeFederationJSON(w, map[string]any{
		"origin":     s.cfg.Matrix.ServerName,
		"pdus":       pdus,
		"auth_chain": authChain,
	})
}

// federationStateIDs - состояние комнаты до события eventID или, без него,
// текущее (room_state). found=false - события в комнате нет.
func (s *Server) federationStateIDs(ctx context.Context, roomID, eventID string) (ids []string, found bool, err error) {
	if eventID != "" {
		return s.db.StateIDsBefore(ctx, roomID, eventID)
	}

	rows, err := s.db.Pool().Query(ctx, `
		SELECT event_id FROM room_state WHERE room_id = $1 ORDER BY type, state_key`, roomID)
	if err != nil {
		return nil, false, err
	}

	defer rows.Close()

	ids = []string{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}

		ids = append(ids, id)
	}

	return ids, true, rows.Err()
}

// handleFederationEventAuth — GET /_matrix/federation/v1/event_auth/{roomId}/{eventId}:
// рекурсивная auth-цепочка события (пир догружает ее для событий из /backfill).
func (s *Server) handleFederationEventAuth(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, okRoom := pathParam(r, "roomId")
	eventID, okEvent := pathParam(r, "eventId")

	if !okRoom || !okEvent || roomID == "" || eventID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room or event id")

		return
	}

	if _, ok := s.requireServerInRoom(w, r, roomID); !ok {
		return
	}

	nodes, err := s.db.DAGEvents(r.Context(), roomID, []string{eventID})
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read event")

		return
	}

	if len(nodes) == 0 {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found in room")

		return
	}

	authIDs, err := s.db.AuthChainIDs(r.Context(), []string{eventID})
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	authChain, err := s.federationPDUs(r.Context(), authIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	writeFederationJSON(w, map[string]any{"auth_chain": authChain})
}

// handleGetMissingEvents — POST /_matrix/federation/v1/get_missing_events/{roomId}.
func (s *Server) handleGetMissingEvents(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok || roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	origin, ok := s.requireServerInRoom(w, r, roomID)
	if !ok {
		return
	}

	var req struct {
		EarliestEvents []string `json:"earliest_events"`
		LatestEvents   []string `json:"latest_events"`
		Limit          int      `json:"limit"`
		MinDepth       int64    `json:"min_depth"`
	}

	if !decodeRoomBody(w, r, &req) {
		return
	}

	// Спека: limit по умолчанию 10; больше 100 не отдаем.
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	if limit > 100 {
		limit = 100
	}

	ids, err := s.getMissingEventIDs(r.Context(), roomID, req.EarliestEvents, req.LatestEvents, req.MinDepth, limit)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read events")

		return
	}

	events, err := s.visiblePDUs(r.Context(), roomID, origin, ids)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read events")

		return
	}

	// DEBUG: точный JSON ответа — Synapse на нём отвечает «Invalid event JSON»,
	// без дампа не видно, какое поле не нравится.
	if raw, mErr := json.Marshal(map[string]any{"events": events}); mErr == nil {
		s.log.DebugContext(r.Context(), "api: get_missing_events reply",
			"room_id", roomID, "b64", base64.RawStdEncoding.EncodeToString(raw))
	}

	writeFederationJSON(w, map[string]any{"events": events})
}

// handleBackfill — GET /_matrix/federation/v1/backfill/{roomId}?v=&limit=.
//
// По DAG от событий v назад (сами v включаются, как в Synapse), не больше limit.
// Ответ - Transaction спеки; auth-цепочки пир берет из /event_auth.
func (s *Server) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok || roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	origin, ok := s.requireServerInRoom(w, r, roomID)
	if !ok {
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = min(parsed, 100)
		}
	}

	ids, err := s.backfillIDs(r.Context(), roomID, r.URL.Query()["v"], limit)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read events")

		return
	}

	pdus, err := s.visiblePDUs(r.Context(), roomID, origin, ids)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read events")

		return
	}

	// Спека: backfill отдаёт обычный объект, обёртку [200, …] Synapse не парсит.
	writeFederationJSON(w, s.federationTransaction(pdus))
}

// handleFederationInvite — PUT /_matrix/federation/v1|v2/invite/{roomId}/{eventId}.
//
// Принимается только m.room.member invite нашему пользователю от сервера
// отправителя; в комнате, где мы участник, - с правами отправителя по текущему
// состоянию. Событие подписывается нашим ключом и возвращается.
func (s *Server) handleFederationInvite(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok || roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	var body map[string]any
	if !decodeFederationBody(w, r, &body) {
		return
	}

	// Спека CS-S2S: v2 - тело {room_version, event, invite_room_state}; v1 - голое
	// событие (и совместимость с прежними нашими серверами).
	object := body
	version := 0

	if wrapped, ok := body["event"].(map[string]any); ok {
		object = wrapped

		if raw, ok := body["room_version"].(string); ok && raw != "" {
			// Версии вне поддерживаемых по федерации (10..12) - отказ.
			if err := parseRoomVersion(raw, &version); err != nil || version > core.FederationMaxRoomVersion {
				writeMatrixError(w, http.StatusBadRequest, ErrCodeUnsupportedRoomVersion, "Unsupported room version")

				return
			}
		}
	}

	if version == 0 {
		version = s.versionForObject(r.Context(), object)
	}

	incoming, rejected := s.parseIncomingMembership(r, object, roomID, "invite", version)
	if rejected == nil {
		rejected = s.checkIncomingInvite(r.Context(), incoming)
	}

	if rejected != nil {
		s.log.WarnContext(r.Context(), "api: invite rejected", "room_id", roomID, "reason", rejected.message)
		rejected.write(w)

		return
	}

	if !s.requireServerACL(w, r, roomID, incoming.origin) {
		return
	}

	if s.verifyFederationJSONVersion(r.Context(), object, incoming.origin, incoming.version) != nil {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Invalid event signature")

		return
	}

	if rejected := s.authorizeIncomingInvite(r.Context(), incoming.pdu); rejected != nil {
		s.log.WarnContext(r.Context(), "api: invite not authorized", "room_id", roomID, "reason", rejected.message)
		rejected.write(w)

		return
	}

	// Спека: приглашаемый сервер подписывает событие и возвращает его; храним
	// тоже с нашей подписью (как Synapse).
	signed, err := s.addOurSignature(object, version)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: sign invite failed", "room_id", roomID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to sign invite")

		return
	}

	pdu := incoming.pdu
	pdu.PDU = rawPDU(signed)

	if raw, err := json.Marshal(signed["signatures"]); err == nil {
		pdu.Signatures = raw
	}

	if err := s.applyMembershipEvent(r.Context(), pdu, incoming.origin, incoming.version); err != nil {
		s.log.ErrorContext(r.Context(), "api: invite apply failed", "room_id", roomID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to store invite")

		return
	}

	// Возвращаем событие без event_id (для room v3+ явный event_id запрещён, иначе
	// Synapse отвечает «Invalid event JSON»). v2 - {"event": ...}, v1 -
	// [200, {"event": ...}].
	event := withoutEventID(signed)

	if strings.Contains(r.URL.Path, "/v2/invite/") {
		writeFederationJSON(w, map[string]any{"event": event})

		return
	}

	writeFederationJSON(w, []any{200, map[string]any{"event": event}})
}

// checkIncomingInvite - приглашение только нашему пользователю и не в
// заблокированную комнату.
func (s *Server) checkIncomingInvite(ctx context.Context, in incomingMembership) *membershipError {
	if domainOf(*in.pdu.StateKey) != s.cfg.Matrix.ServerName {
		return badMembership("The invite event must be for this server")
	}

	blocked, err := s.db.RoomBlocked(ctx, in.pdu.RoomID)
	if err != nil {
		return &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to read room"}
	}

	if blocked {
		return forbiddenMembership("This room has been blocked on this server")
	}

	return nil
}

// authorizeIncomingInvite - в комнате, где у нас есть участник, приглашение
// проверяется auth rules по текущему состоянию: отправитель joined и с уровнем
// invite, приглашенный не забанен и не в комнате. Комнату, в которой нас нет,
// проверить не по чему - приглашение принимается как out-of-band (Synapse).
func (s *Server) authorizeIncomingInvite(ctx context.Context, pdu store.FedPDU) *membershipError {
	resident, err := s.db.HostInRoom(ctx, pdu.RoomID, s.cfg.Matrix.ServerName)
	if err != nil {
		return &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to read room"}
	}

	if !resident {
		return nil
	}

	if err := s.authorizeByCurrentState(ctx, pdu); err != nil {
		return forbiddenMembership("Invite is not authorized by room state")
	}

	return nil
}

// registerFederationStateRoutes подключает send/state/backfill/invite маршруты.
func (s *Server) registerFederationStateRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/event/{eventId}", s.handleFederationEvent)
	r.Get("/_matrix/federation/v1/state/{roomId}", s.handleFederationState)
	r.Get("/_matrix/federation/v1/state_ids/{roomId}", s.handleFederationStateIDs)
	r.Post("/_matrix/federation/v1/get_missing_events/{roomId}", s.handleGetMissingEvents)
	r.Get("/_matrix/federation/v1/backfill/{roomId}", s.handleBackfill)
	r.Get("/_matrix/federation/v1/event_auth/{roomId}/{eventId}", s.handleFederationEventAuth)
	r.Put("/_matrix/federation/v1/invite/{roomId}/{eventId}", s.handleFederationInvite)
	// v2: то же тело + invite_room_state (MSC4311). Synapse 1.165 шлёт invite в v2 и,
	// получив 404 на нью-формате события, отказывается создавать комнату с нашим
	// пользователем («User's homeserver does not support this room version»).
	r.Put("/_matrix/federation/v2/invite/{roomId}/{eventId}", s.handleFederationInvite)
}

// domainOfID возвращает домен Matrix-идентификатора (@user:domain).
func domainOfID(id string) string {
	return domainOf(id)
}
