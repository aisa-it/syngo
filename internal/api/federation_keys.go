package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Федеративные запросы ключей и устройств: эндпоинты для других
// серверов и клиент к ним. Раньше /_matrix/federation/v1/user/keys/* и
// user/devices не было, и сервер Synapse не мог получить ключи наших устройств.

// federationKeysTimeout - предел одного федеративного запроса ключей.
const federationKeysTimeout = 15 * time.Second

// federationKeysClient - core.FederationKeysClient поверх транспорта федерации.
// Транспорт подключается после New (SetFederationTransport), поэтому берется
// при каждом вызове.
type federationKeysClient struct{ s *Server }

var _ core.FederationKeysClient = federationKeysClient{}

// call отправляет запрос и разбирает JSON-ответ.
func (c federationKeysClient) call(ctx context.Context, server, method, path string, body, out any) error {
	client := c.s.federationTransport
	if client == nil || c.s.federationKey == nil {
		return core.ErrFederationUnavailable
	}

	var payload []byte

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}

		payload = raw
	}

	ctx, cancel := context.WithTimeout(ctx, federationKeysTimeout)
	defer cancel()

	resp, status, err := client.Send(ctx, server, method, path, payload)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return fmt.Errorf("api: %s %s: HTTP %d", server, path, status)
	}

	return json.Unmarshal(resp, out)
}

// QueryUserKeys - POST /_matrix/federation/v1/user/keys/query.
func (c federationKeysClient) QueryUserKeys(ctx context.Context, server string, requests map[string][]string) (
	core.RemoteKeysQuery, error,
) {
	var resp core.RemoteKeysQuery

	if err := c.call(ctx, server, http.MethodPost, "/_matrix/federation/v1/user/keys/query",
		map[string]any{"device_keys": nonNilDeviceLists(requests)}, &resp); err != nil {
		return core.RemoteKeysQuery{}, err
	}

	// Ключи устройства должны принадлежать тому пользователю и устройству, под
	// которыми пришли, и серверу, который ответил.
	for userID, devices := range resp.DeviceKeys {
		if domainOfID(userID) != server {
			delete(resp.DeviceKeys, userID)

			continue
		}

		for deviceID, keys := range devices {
			if !deviceKeysMatch(keys, userID, deviceID) {
				delete(devices, deviceID)
			}
		}
	}

	return resp, nil
}

// nonNilDeviceLists - пустой список устройств сериализуется как [], а не null.
func nonNilDeviceLists(requests map[string][]string) map[string][]string {
	out := make(map[string][]string, len(requests))

	for userID, devices := range requests {
		if devices == nil {
			devices = []string{}
		}

		out[userID] = devices
	}

	return out
}

// deviceKeysMatch - объект ключей объявляет те же user_id и device_id.
func deviceKeysMatch(raw json.RawMessage, userID, deviceID string) bool {
	var keys struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}

	return json.Unmarshal(raw, &keys) == nil && keys.UserID == userID && keys.DeviceID == deviceID
}

// ClaimUserKeys - POST /_matrix/federation/v1/user/keys/claim.
func (c federationKeysClient) ClaimUserKeys(ctx context.Context, server string, requests map[string]map[string]string) (
	map[string]map[string]map[string]json.RawMessage, error,
) {
	var resp struct {
		OneTimeKeys map[string]map[string]map[string]json.RawMessage `json:"one_time_keys"`
	}

	if err := c.call(ctx, server, http.MethodPost, "/_matrix/federation/v1/user/keys/claim",
		map[string]any{"one_time_keys": requests}, &resp); err != nil {
		return nil, err
	}

	for userID := range resp.OneTimeKeys {
		if domainOfID(userID) != server {
			delete(resp.OneTimeKeys, userID)
		}
	}

	return resp.OneTimeKeys, nil
}

// userDevicesResponse - тело /user/devices/{userId}.
type userDevicesResponse struct {
	UserID   string `json:"user_id"`
	StreamID int64  `json:"stream_id"`
	Devices  []struct {
		DeviceID    string          `json:"device_id"`
		DisplayName string          `json:"device_display_name,omitempty"`
		Keys        json.RawMessage `json:"keys,omitempty"`
	} `json:"devices"`
	MasterKey      json.RawMessage `json:"master_key,omitempty"`
	SelfSigningKey json.RawMessage `json:"self_signing_key,omitempty"`
}

// UserDevices - GET /_matrix/federation/v1/user/devices/{userId}.
func (c federationKeysClient) UserDevices(ctx context.Context, server, userID string) (core.RemoteDeviceList, error) {
	var resp userDevicesResponse

	if err := c.call(ctx, server, http.MethodGet,
		"/_matrix/federation/v1/user/devices/"+url.PathEscape(userID), nil, &resp); err != nil {
		return core.RemoteDeviceList{}, err
	}

	list := core.RemoteDeviceList{
		UserID: resp.UserID, StreamID: resp.StreamID, Master: resp.MasterKey, SelfSigning: resp.SelfSigningKey,
	}

	for _, device := range resp.Devices {
		keys := device.Keys
		if len(keys) > 0 && !deviceKeysMatch(keys, userID, device.DeviceID) {
			keys = nil
		}

		list.Devices = append(list.Devices, core.RemoteDevice{
			UserID: userID, DeviceID: device.DeviceID, DisplayName: device.DisplayName, Keys: keys,
		})
	}

	return list, nil
}

// handleFederationUserKeysQuery - POST /_matrix/federation/v1/user/keys/query:
// ключи устройств и кросс-подписи (master, self_signing) наших пользователей.
func (s *Server) handleFederationUserKeysQuery(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	var req keysQueryRequest
	if !decodeKeysBody(w, r, &req) {
		return
	}

	local := map[string][]string{}

	for userID, devices := range req.DeviceKeys {
		if domainOfID(userID) == s.cfg.Matrix.ServerName {
			local[userID] = devices
		}
	}

	keys, err := s.keys.Query(r.Context(), local)
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	if keys == nil {
		keys = map[string]map[string]json.RawMessage{}
	}

	users := make([]string, 0, len(local))

	for userID := range local {
		users = append(users, userID)

		if _, ok := keys[userID]; !ok {
			keys[userID] = map[string]json.RawMessage{}
		}
	}

	// Подписи видны как владельцу ключей: requester пуст, user_signing не отдается.
	cross, err := s.crossSigningQuery(r.Context(), "", keys, users)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: federation cross-signing query failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read cross-signing keys")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_keys": keys, "master_keys": cross.Master, "self_signing_keys": cross.SelfSigning,
	})
}

// handleFederationUserKeysClaim - POST /_matrix/federation/v1/user/keys/claim.
func (s *Server) handleFederationUserKeysClaim(w http.ResponseWriter, r *http.Request) {
	if s.keys == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	var req keysClaimRequest
	if !decodeKeysBody(w, r, &req) {
		return
	}

	local := map[string]map[string]string{}

	for userID, devices := range req.OneTimeKeys {
		if domainOfID(userID) == s.cfg.Matrix.ServerName {
			local[userID] = devices
		}
	}

	keys, err := s.keys.Claim(r.Context(), local)
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	if keys == nil {
		keys = map[string]map[string]map[string]json.RawMessage{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"one_time_keys": keys})
}

// handleFederationUserDevices - GET /_matrix/federation/v1/user/devices/{userId}:
// все устройства нашего пользователя с ключами, stream_id его device list и
// ключи кросс-подписи. Имена устройств по федерации не раскрываются (как
// allow_device_name_lookup_over_federation=false в Synapse).
func (s *Server) handleFederationUserDevices(w http.ResponseWriter, r *http.Request) {
	userID, ok := pathParam(r, "userId")
	if !ok || domainOfID(userID) != s.cfg.Matrix.ServerName {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "User is not local")

		return
	}

	if s.keys == nil || s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Keys storage is not configured")

		return
	}

	devices, err := s.db.LocalDevices(r.Context(), userID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: federation user devices failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read devices")

		return
	}

	keys, err := s.keys.Query(r.Context(), map[string][]string{userID: {}})
	if err != nil {
		s.writeKeysError(w, err)

		return
	}

	if keys == nil {
		keys = map[string]map[string]json.RawMessage{}
	}

	cross, err := s.crossSigningQuery(r.Context(), "", keys, []string{userID})
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: federation cross-signing query failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read cross-signing keys")

		return
	}

	stream, err := s.db.DeviceListStream(r.Context(), userID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: federation device stream failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read devices")

		return
	}

	type device struct {
		DeviceID string          `json:"device_id"`
		Keys     json.RawMessage `json:"keys,omitempty"`
	}

	list := make([]device, 0, len(devices))

	for _, deviceID := range sortedKeys(devices) {
		list = append(list, device{DeviceID: deviceID, Keys: keys[userID][deviceID]})
	}

	resp := map[string]any{"user_id": userID, "stream_id": stream, "devices": list}

	if key, ok := cross.Master[userID]; ok {
		resp["master_key"] = key
	}

	if key, ok := cross.SelfSigning[userID]; ok {
		resp["self_signing_key"] = key
	}

	writeJSON(w, http.StatusOK, resp)
}

// registerFederationKeysRoutes подключает федеративные эндпоинты ключей.
func (s *Server) registerFederationKeysRoutes(r chi.Router) {
	r.Post("/_matrix/federation/v1/user/keys/query", s.handleFederationUserKeysQuery)
	r.Post("/_matrix/federation/v1/user/keys/claim", s.handleFederationUserKeysClaim)
	r.Get("/_matrix/federation/v1/user/devices/{userId}", s.handleFederationUserDevices)
}
