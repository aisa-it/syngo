package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// To-device и /keys/changes: очередь и выборка - в core.ToDeviceService,
// здесь разбор запроса и ответ.

// sendToDeviceRequest — тело PUT /sendToDevice/{eventType}/{txnId}.
type sendToDeviceRequest struct {
	Messages map[string]map[string]json.RawMessage `json:"messages"`
}

// handleSendToDevice — PUT /sendToDevice/{eventType}/{txnId}: to-device сообщение устройствам.
func (s *Server) handleSendToDevice(w http.ResponseWriter, r *http.Request) {
	if s.toDevice == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	eventType, ok := pathParam(r, "eventType")
	if !ok || eventType == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid event type")

		return
	}

	txnID, ok := pathParam(r, "txnId")
	if !ok || txnID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid transaction id")

		return
	}

	var req sendToDeviceRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	queued, err := s.toDevice.Send(r.Context(), session.UserID, session.DeviceID, eventType, txnID, req.Messages)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: sendToDevice failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to queue to-device messages")

		return
	}

	// to-device тоже будит long-poll: иначе получатель увидит его только по
	// таймауту.
	if queued {
		s.notifySync()
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// keysChangesResponse — тело GET /keys/changes.
type keysChangesResponse struct {
	Changed []string `json:"changed"`
	Left    []string `json:"left"`
}

// handleKeysChanges — GET /keys/changes?from=&to=: у кого менялись ключи устройств.
func (s *Server) handleKeysChanges(w http.ResponseWriter, r *http.Request) {
	if s.toDevice == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	var since int64

	// from Synapse (sync-токен до переезда) позицию не переводит: отдаем изменения
	// с начала, клиент перезапросит ключи. Раньше - 400 M_UNKNOWN_POS.
	// to Synapse не разбирается и ниже не ограничивает окно.
	if raw := r.URL.Query().Get("from"); raw != "" && !core.IsSynapseSyncToken(raw) && !core.IsSynapsePageToken(raw) {
		decoded, err := s.syncTokens.Decode(raw)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknownPos, "Unknown sync position")

			return
		}

		since = decoded
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	// Верхняя граница - to из запроса (спецификация), иначе текущая позиция.
	upper := s.syncPosition(r.Context())

	if raw := r.URL.Query().Get("to"); raw != "" {
		if decoded, err := s.syncTokens.Decode(raw); err == nil && decoded < upper {
			upper = decoded
		}
	}

	// Только пользователи с общими комнатами, как device_lists в /sync: раньше
	// здесь отдавались все, у кого менялись ключи на сервере.
	changed, err := s.toDevice.KeyChanges(r.Context(), session.UserID, since, upper)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: keys/changes failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read device list changes")

		return
	}

	writeJSON(w, http.StatusOK, keysChangesResponse{Changed: changed, Left: []string{}})
}

// registerToDeviceRoutes подключает /sendToDevice (нужен токен).
func (s *Server) registerToDeviceRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Put("/sendToDevice/{eventType}/{txnId}", s.handleSendToDevice)
	})
}
