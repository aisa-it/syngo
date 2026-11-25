package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// maxKeysBodySize ограничивает тело запросов ключей (device_keys с подписями бывают крупными).
const maxKeysBodySize = 512 << 10

// keysUploadRequest — тело POST /keys/upload.
type keysUploadRequest struct {
	DeviceKeys   json.RawMessage            `json:"device_keys"`
	OneTimeKeys  map[string]json.RawMessage `json:"one_time_keys"`
	FallbackKeys map[string]json.RawMessage `json:"fallback_keys"`
	// UnstableFallbackKeys - имя поля до стабилизации MSC2732; Synapse принимает оба.
	UnstableFallbackKeys map[string]json.RawMessage `json:"org.matrix.msc2732.fallback_keys"`
}

// keysUploadResponse — ответ /keys/upload (как в CS-API v1.11).
type keysUploadResponse struct {
	OneTimeKeyCounts map[string]int `json:"one_time_key_counts"`
}

// keysQueryRequest — тело POST /keys/query.
type keysQueryRequest struct {
	DeviceKeys map[string][]string `json:"device_keys"`
}

// keysQueryResponse — ответ /keys/query.
type keysQueryResponse struct {
	DeviceKeys      map[string]map[string]json.RawMessage `json:"device_keys"`
	MasterKeys      map[string]json.RawMessage            `json:"master_keys"`
	SelfSigningKeys map[string]json.RawMessage            `json:"self_signing_keys"`
	UserSigningKeys map[string]json.RawMessage            `json:"user_signing_keys"`
	Failures        map[string]any                        `json:"failures"`
}

// keysClaimRequest — тело POST /keys/claim.
type keysClaimRequest struct {
	OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
}

// keysClaimResponse — ответ /keys/claim.
type keysClaimResponse struct {
	OneTimeKeys map[string]map[string]map[string]json.RawMessage `json:"one_time_keys"`
	Failures    map[string]any                                   `json:"failures"`
}

// decodeKeysBody читает тело запросов ключей: не JSON - 400 M_NOT_JSON, JSON не
// той формы (device_keys объектом вместо списка и т.п.) - 400 M_BAD_JSON, как в
// Synapse (Complement TestKeysQueryWithDeviceIDAsObjectFails).
func decodeKeysBody(w http.ResponseWriter, r *http.Request, target any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKeysBodySize)).Decode(target)
	if err == nil {
		return true
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed request body")

		return false
	}

	writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

	return false
}

// handleKeysUpload обрабатывает POST /keys/upload.
func (s *Server) handleKeysUpload(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.keys == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	var req keysUploadRequest
	if !decodeKeysBody(w, r, &req) {
		return
	}

	fallback := req.FallbackKeys
	if len(fallback) == 0 {
		fallback = req.UnstableFallbackKeys
	}

	counts, err := s.keys.Upload(r.Context(), session.UserID, session.DeviceID, core.UploadRequest{
		DeviceKeys:   req.DeviceKeys,
		OneTimeKeys:  req.OneTimeKeys,
		FallbackKeys: fallback,
	})
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	// Новые ключи устройства - серверам общих комнат.
	if len(req.DeviceKeys) > 0 {
		s.sendDeviceListUpdate(r.Context(), session.UserID, []string{session.DeviceID}, false)
	}

	writeJSON(w, http.StatusOK, keysUploadResponse{OneTimeKeyCounts: counts})
}

// handleKeysQuery обрабатывает POST /keys/query.
func (s *Server) handleKeysQuery(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.keys == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	var req keysQueryRequest
	if !decodeKeysBody(w, r, &req) {
		return
	}

	keys, err := s.keys.Query(r.Context(), req.DeviceKeys)
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	if keys == nil {
		keys = map[string]map[string]json.RawMessage{}
	}

	// Пользователи других серверов - из кэша устройств или запросом к их
	// серверу; недоступный сервер - в failures. Раньше они молча
	// выбрасывались, и Element не мог зашифровать сообщение для них.
	remote, err := s.queryRemoteKeys(r.Context(), req.DeviceKeys)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: remote keys query failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to query remote keys")

		return
	}

	for userID, devices := range remote.DeviceKeys {
		keys[userID] = devices
	}

	users := make([]string, 0, len(req.DeviceKeys))
	for userID := range req.DeviceKeys {
		users = append(users, userID)

		// Локальный пользователь без ключей - пустой объект, а не отсутствие
		// ключа, как в Synapse (Complement TestUploadKey).
		if _, ok := keys[userID]; !ok && domainOfIdentifier(userID) == s.cfg.Matrix.ServerName {
			keys[userID] = map[string]json.RawMessage{}
		}
	}

	cross, err := s.crossSigningQuery(r.Context(), session.UserID, keys, users)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: cross-signing query failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read cross-signing keys")

		return
	}

	// Ключи кросс-подписи неотслеживаемых удаленных пользователей - как прислал их сервер.
	for userID, key := range remote.Master {
		if _, ok := cross.Master[userID]; !ok {
			cross.Master[userID] = key
		}
	}

	for userID, key := range remote.SelfSigning {
		if _, ok := cross.SelfSigning[userID]; !ok {
			cross.SelfSigning[userID] = key
		}
	}

	writeJSON(w, http.StatusOK, keysQueryResponse{
		DeviceKeys: keys, MasterKeys: cross.Master, SelfSigningKeys: cross.SelfSigning, UserSigningKeys: cross.UserSigning,
		Failures: remote.Failures,
	})
}

// handleKeysClaim обрабатывает POST /keys/claim.
func (s *Server) handleKeysClaim(w http.ResponseWriter, r *http.Request) {
	if _, ok := sessionFrom(r.Context()); !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.keys == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	var req keysClaimRequest
	if !decodeKeysBody(w, r, &req) {
		return
	}

	keys, err := s.keys.Claim(r.Context(), req.OneTimeKeys)
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	// Пустой ответ — это 200 с пустым объектом, а не 404 (спека /keys/claim):
	// клиент сам решает, что ключей у устройства нет.
	if keys == nil {
		keys = map[string]map[string]map[string]json.RawMessage{}
	}

	// One-time keys удаленных устройств выдает их сервер.
	failures := map[string]any{}

	if remote := s.remoteRequests(req.OneTimeKeys); len(remote) > 0 && s.remoteKeys != nil {
		remoteKeys, remoteFailures := s.remoteKeys.Claim(r.Context(), remote)

		for userID, devices := range remoteKeys {
			keys[userID] = devices
		}

		failures = remoteFailures
	}

	writeJSON(w, http.StatusOK, keysClaimResponse{OneTimeKeys: keys, Failures: failures})
}

// remoteRequests - запросы к пользователям других серверов.
func remoteRequestsOf[V any](requests map[string]V, serverName string) map[string]V {
	out := map[string]V{}

	for userID, value := range requests {
		if core.IsRemoteUser(userID, serverName) {
			out[userID] = value
		}
	}

	return out
}

// remoteRequests - claim-запросы к пользователям других серверов.
func (s *Server) remoteRequests(requests map[string]map[string]string) map[string]map[string]string {
	return remoteRequestsOf(requests, s.cfg.Matrix.ServerName)
}

// queryRemoteKeys - ключи пользователей других серверов для /keys/query.
func (s *Server) queryRemoteKeys(ctx context.Context, requests map[string][]string) (core.RemoteKeysResult, error) {
	remote := remoteRequestsOf(requests, s.cfg.Matrix.ServerName)

	if len(remote) == 0 || s.remoteKeys == nil {
		return core.RemoteKeysResult{
			DeviceKeys: map[string]map[string]json.RawMessage{}, Failures: map[string]any{},
		}, nil
	}

	return s.remoteKeys.Query(ctx, remote)
}

// writeKeysError переводит доменные ошибки ключей в коды Matrix.
func (s *Server) writeKeysError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrBadJSON):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid key data")
	default:
		s.log.Error("api: keys failure", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
	}
}

// registerKeysRoutes подключает эндпоинты E2EE-ключей (нужен access token).
func (s *Server) registerKeysRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/keys/upload", s.handleKeysUpload)
		r.Post("/keys/query", s.handleKeysQuery)
		r.Post("/keys/claim", s.handleKeysClaim)
		r.Get("/keys/changes", s.handleKeysChanges)
		r.Post("/keys/device_signing/upload", s.handleDeviceSigningUpload)
		r.Post("/keys/signatures/upload", s.handleSignaturesUpload)
	})
}
