package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Ключи и устройства пользователей других серверов. Поведение -
// как в Synapse (DeviceListUpdater, E2eKeysHandler):
//
//   - пользователь, делящий комнату с нашими, отслеживается: его устройства и
//     ключи кросс-подписи лежат в кэше, сервер-владелец шлет нам
//     m.device_list_update / m.signing_key_update;
//   - дыра в stream_id (prev_id не совпал с последним примененным) - полный
//     ресинк через GET /_matrix/federation/v1/user/devices/{userId};
//   - /keys/query для отслеживаемого - из кэша, для остальных - запрос
//     /user/keys/query к их серверу; /keys/claim - всегда к серверу владельца;
//   - недоступный сервер попадает в failures, а не роняет весь ответ.

// RemoteDevice - устройство пользователя другого сервера.
type RemoteDevice struct {
	UserID      string
	DeviceID    string
	DisplayName string
	// Keys - device_keys устройства как прислал сервер (с подписями); nil - ключей нет.
	Keys json.RawMessage
}

// RemoteDeviceList - полный список устройств удаленного пользователя (ответ /user/devices).
type RemoteDeviceList struct {
	UserID   string
	StreamID int64
	Devices  []RemoteDevice
	// Master, SelfSigning - ключи кросс-подписи (nil - нет).
	Master      json.RawMessage
	SelfSigning json.RawMessage
}

// RemoteDeviceUpdate - одно изменение из m.device_list_update.
type RemoteDeviceUpdate struct {
	UserID      string
	DeviceID    string
	DisplayName string
	StreamID    int64
	PrevIDs     []int64
	Deleted     bool
	Keys        json.RawMessage
}

// RemoteListState - состояние кэша удаленного пользователя.
type RemoteListState struct {
	StreamID int64
	Stale    bool
}

// RemoteKeysQuery - ответ /user/keys/query сервера-владельца.
type RemoteKeysQuery struct {
	DeviceKeys      map[string]map[string]json.RawMessage `json:"device_keys"`
	MasterKeys      map[string]json.RawMessage            `json:"master_keys"`
	SelfSigningKeys map[string]json.RawMessage            `json:"self_signing_keys"`
}

// RemoteKeysStore - кэш удаленных устройств (реализуется store.DB).
type RemoteKeysStore interface {
	// RemoteDeviceListStates - отслеживаемые пользователи из userIDs и их состояние.
	RemoteDeviceListStates(ctx context.Context, userIDs []string) (map[string]RemoteListState, error)
	// ReplaceRemoteDeviceList заменяет кэш пользователя целиком (ресинк).
	ReplaceRemoteDeviceList(ctx context.Context, list RemoteDeviceList) error
	// ApplyRemoteDeviceUpdate применяет одно изменение устройства и stream_id.
	ApplyRemoteDeviceUpdate(ctx context.Context, update RemoteDeviceUpdate) error
	// MarkRemoteDeviceListStale помечает кэш пользователя для ресинка.
	MarkRemoteDeviceListStale(ctx context.Context, userID string) error
	// ApplyRemoteSigningKeys обновляет ключи кросс-подписи отслеживаемого пользователя.
	ApplyRemoteSigningKeys(ctx context.Context, userID string, master, selfSigning json.RawMessage) error
	// RemoteDeviceKeys - ключи устройств из кэша (пустой список устройств - все).
	RemoteDeviceKeys(ctx context.Context, requests map[string][]string) (map[string]map[string]json.RawMessage, error)
	// RemoteUsersSharingRoom - кто из userIDs делит комнату с пользователями ownServer.
	RemoteUsersSharingRoom(ctx context.Context, userIDs []string, ownServer string) ([]string, error)
}

// FederationKeysClient - федеративные запросы ключей (реализуется в api поверх транспорта).
type FederationKeysClient interface {
	QueryUserKeys(ctx context.Context, server string, requests map[string][]string) (RemoteKeysQuery, error)
	ClaimUserKeys(ctx context.Context, server string, requests map[string]map[string]string) (
		map[string]map[string]map[string]json.RawMessage, error)
	UserDevices(ctx context.Context, server, userID string) (RemoteDeviceList, error)
}

// RemoteKeysResult - ключи удаленных пользователей для /keys/query.
type RemoteKeysResult struct {
	DeviceKeys  map[string]map[string]json.RawMessage
	Master      map[string]json.RawMessage
	SelfSigning map[string]json.RawMessage
	// Failures - сервер -> причина (как failures в ответе /keys/query).
	Failures map[string]any
}

// RemoteKeysService - ключи и устройства пользователей других серверов.
type RemoteKeysService struct {
	store      RemoteKeysStore
	client     FederationKeysClient
	serverName string
	// changed вызывается после изменения кэша пользователя (будит /sync).
	changed func(ctx context.Context, userID string)
	warn    func(ctx context.Context, msg string, err error, args ...any)
}

// NewRemoteKeysService создает сервис. client может быть nil (федерация выключена):
// тогда удаленные серверы попадают в failures.
func NewRemoteKeysService(
	store RemoteKeysStore, client FederationKeysClient, serverName string,
	changed func(ctx context.Context, userID string),
	warn func(ctx context.Context, msg string, err error, args ...any),
) *RemoteKeysService {
	if changed == nil {
		changed = func(context.Context, string) {}
	}

	if warn == nil {
		warn = func(context.Context, string, error, ...any) {}
	}

	return &RemoteKeysService{store: store, client: client, serverName: serverName, changed: changed, warn: warn}
}

// ErrFederationUnavailable - исходящая федерация не настроена.
var ErrFederationUnavailable = errors.New("core: federation is not available")

// IsRemoteUser - user_id принадлежит другому серверу.
func IsRemoteUser(userID, serverName string) bool {
	domain := userDomain(userID)

	return domain != "" && domain != serverName
}

// userDomain - домен @user:domain ("" - не user_id).
func userDomain(userID string) string {
	if !strings.HasPrefix(userID, "@") {
		return ""
	}

	_, domain, found := strings.Cut(userID, ":")
	if !found {
		return ""
	}

	return domain
}

// failure - запись failures для недоступного сервера (как в Synapse: status + message).
func failure(err error) map[string]any {
	if errors.Is(err, ErrFederationUnavailable) {
		return map[string]any{"status": 503, "message": "Federation is not available"}
	}

	return map[string]any{"status": 503, "message": "Not ready for retry"}
}

// Query отдает ключи устройств и кросс-подписи удаленных пользователей.
func (s *RemoteKeysService) Query(ctx context.Context, requests map[string][]string) (RemoteKeysResult, error) {
	result := RemoteKeysResult{
		DeviceKeys: map[string]map[string]json.RawMessage{}, Master: map[string]json.RawMessage{},
		SelfSigning: map[string]json.RawMessage{}, Failures: map[string]any{},
	}

	if len(requests) == 0 {
		return result, nil
	}

	users := make([]string, 0, len(requests))
	for userID := range requests {
		users = append(users, userID)
	}

	sort.Strings(users)

	states, err := s.store.RemoteDeviceListStates(ctx, users)
	if err != nil {
		return result, err
	}

	sharing, err := s.store.RemoteUsersSharingRoom(ctx, users, s.serverName)
	if err != nil {
		return result, err
	}

	shared := make(map[string]bool, len(sharing))
	for _, userID := range sharing {
		shared[userID] = true
	}

	fromCache := map[string][]string{}
	byServer := map[string]map[string][]string{}

	for _, userID := range users {
		state, tracked := states[userID]

		// Кэш авторитетен, только пока сервер шлет нам изменения (общая комната).
		if shared[userID] && (!tracked || state.Stale) {
			if err := s.Resync(ctx, userID); err == nil {
				tracked, state.Stale = true, false
			} else {
				s.warn(ctx, "remote device list resync failed", err, "user_id", userID)
			}
		}

		if shared[userID] && tracked && !state.Stale {
			fromCache[userID] = requests[userID]

			continue
		}

		server := userDomain(userID)
		if byServer[server] == nil {
			byServer[server] = map[string][]string{}
		}

		byServer[server][userID] = requests[userID]
	}

	if len(fromCache) > 0 {
		cached, err := s.store.RemoteDeviceKeys(ctx, fromCache)
		if err != nil {
			return result, err
		}

		for userID := range fromCache {
			result.DeviceKeys[userID] = cached[userID]
			if result.DeviceKeys[userID] == nil {
				result.DeviceKeys[userID] = map[string]json.RawMessage{}
			}
		}
	}

	servers := make([]string, 0, len(byServer))
	for server := range byServer {
		servers = append(servers, server)
	}

	sort.Strings(servers)

	for _, server := range servers {
		if s.client == nil {
			result.Failures[server] = failure(ErrFederationUnavailable)

			continue
		}

		resp, err := s.client.QueryUserKeys(ctx, server, byServer[server])
		if err != nil {
			s.warn(ctx, "remote keys query failed", err, "server", server)
			result.Failures[server] = failure(err)

			continue
		}

		for userID := range byServer[server] {
			// Сервер отвечает только за своих пользователей.
			if devices, ok := resp.DeviceKeys[userID]; ok && devices != nil {
				result.DeviceKeys[userID] = devices
			}

			if key, ok := resp.MasterKeys[userID]; ok && len(key) > 0 {
				result.Master[userID] = key
			}

			if key, ok := resp.SelfSigningKeys[userID]; ok && len(key) > 0 {
				result.SelfSigning[userID] = key
			}
		}
	}

	return result, nil
}

// Claim выдает one-time keys удаленных устройств: запрос к серверу-владельцу.
func (s *RemoteKeysService) Claim(ctx context.Context, requests map[string]map[string]string) (
	keys map[string]map[string]map[string]json.RawMessage, failures map[string]any,
) {
	keys = map[string]map[string]map[string]json.RawMessage{}
	failures = map[string]any{}

	byServer := map[string]map[string]map[string]string{}

	for userID, devices := range requests {
		server := userDomain(userID)
		if byServer[server] == nil {
			byServer[server] = map[string]map[string]string{}
		}

		byServer[server][userID] = devices
	}

	for server, req := range byServer {
		if s.client == nil {
			failures[server] = failure(ErrFederationUnavailable)

			continue
		}

		resp, err := s.client.ClaimUserKeys(ctx, server, req)
		if err != nil {
			s.warn(ctx, "remote keys claim failed", err, "server", server)
			failures[server] = failure(err)

			continue
		}

		for userID, devices := range resp {
			if _, asked := req[userID]; asked {
				keys[userID] = devices
			}
		}
	}

	return keys, failures
}

// Resync заменяет кэш устройств пользователя ответом /user/devices его сервера.
func (s *RemoteKeysService) Resync(ctx context.Context, userID string) error {
	if s.client == nil {
		return ErrFederationUnavailable
	}

	list, err := s.client.UserDevices(ctx, userDomain(userID), userID)
	if err != nil {
		if markErr := s.store.MarkRemoteDeviceListStale(ctx, userID); markErr != nil {
			s.warn(ctx, "mark device list stale failed", markErr, "user_id", userID)
		}

		return fmt.Errorf("core: resync %s: %w", userID, err)
	}

	if list.UserID != userID {
		return fmt.Errorf("core: resync %s: response for %q", userID, list.UserID)
	}

	devices := list.Devices[:0]

	for _, device := range list.Devices {
		if device.DeviceID != "" {
			device.UserID = userID
			devices = append(devices, device)
		}
	}

	list.Devices = devices

	if err := s.store.ReplaceRemoteDeviceList(ctx, list); err != nil {
		return err
	}

	s.changed(ctx, userID)

	return nil
}

// HandleDeviceListUpdate применяет m.device_list_update от origin. Пользователь
// без общей с нашими комнаты не отслеживается - изменение игнорируется (как в
// Synapse). Порядок как в DeviceListUpdater: нет кэша или prev_id не совпал с
// последним примененным stream_id - ресинк; stream_id не новее - повтор.
func (s *RemoteKeysService) HandleDeviceListUpdate(ctx context.Context, origin string, update RemoteDeviceUpdate) error {
	if userDomain(update.UserID) != origin || update.DeviceID == "" {
		return fmt.Errorf("core: device list update for %q from %s", update.UserID, origin)
	}

	sharing, err := s.store.RemoteUsersSharingRoom(ctx, []string{update.UserID}, s.serverName)
	if err != nil {
		return err
	}

	if len(sharing) == 0 {
		return nil
	}

	states, err := s.store.RemoteDeviceListStates(ctx, []string{update.UserID})
	if err != nil {
		return err
	}

	state, tracked := states[update.UserID]

	if tracked && !state.Stale && update.StreamID <= state.StreamID {
		return nil
	}

	if !tracked || state.Stale || !prevKnown(update.PrevIDs, state.StreamID) {
		return s.Resync(ctx, update.UserID)
	}

	if err := s.store.ApplyRemoteDeviceUpdate(ctx, update); err != nil {
		return err
	}

	s.changed(ctx, update.UserID)

	return nil
}

// prevKnown - все prev_id изменения уже применены (совпадают с последним stream_id).
func prevKnown(prevIDs []int64, current int64) bool {
	for _, prev := range prevIDs {
		if prev != current {
			return false
		}
	}

	return true
}

// HandleSigningKeyUpdate применяет m.signing_key_update от origin (только для
// отслеживаемых пользователей).
func (s *RemoteKeysService) HandleSigningKeyUpdate(
	ctx context.Context, origin, userID string, master, selfSigning json.RawMessage,
) error {
	if userDomain(userID) != origin {
		return fmt.Errorf("core: signing key update for %q from %s", userID, origin)
	}

	states, err := s.store.RemoteDeviceListStates(ctx, []string{userID})
	if err != nil {
		return err
	}

	if _, tracked := states[userID]; !tracked {
		return nil
	}

	if err := s.store.ApplyRemoteSigningKeys(ctx, userID, master, selfSigning); err != nil {
		return err
	}

	s.changed(ctx, userID)

	return nil
}

// CrossSigningKeyID - открытый ключ из объекта ключа кросс-подписи
// ({"keys": {"ed25519:<pub>": "<pub>"}}): им ключ идентифицируется в хранилище.
func CrossSigningKeyID(raw json.RawMessage) string {
	var key struct {
		Keys map[string]string `json:"keys"`
	}

	if json.Unmarshal(raw, &key) != nil {
		return ""
	}

	for id, pub := range key.Keys {
		if strings.HasPrefix(id, "ed25519:") {
			return pub
		}
	}

	return ""
}
