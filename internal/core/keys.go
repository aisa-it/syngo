package core

import (
	"context"
	"encoding/json"
	"strings"
)

// KeysStore — хранение E2EE-ключей устройств. Интерфейс объявлен здесь, чтобы домен
// не знал про SQL, а стенд и тесты могли подставить in-memory реализацию.
type KeysStore interface {
	UpsertDeviceKeys(ctx context.Context, userID, deviceID string, keys json.RawMessage) error
	UploadOneTimeKeys(ctx context.Context, userID, deviceID string, keys map[string]json.RawMessage) error
	// UploadFallbackKeys сохраняет fallback keys устройства: по одному на алгоритм,
	// новый ключ заменяет прежний.
	UploadFallbackKeys(ctx context.Context, userID, deviceID string, keys map[string]json.RawMessage) error
	OneTimeKeyCounts(ctx context.Context, userID, deviceID string) (map[string]int, error)
	QueryDeviceKeys(ctx context.Context, requests map[string][]string) (map[string]map[string]json.RawMessage, error)
	// ClaimOneTimeKeys выдает one-time key, а если они кончились - fallback key.
	ClaimOneTimeKeys(ctx context.Context, requests map[string]map[string]string) (map[string]map[string]map[string]json.RawMessage, error)
}

// KeysService — доменный сервис E2EE-ключей: проверка форматов и приватность выдачи.
//
// Сервер хранит и раздаёт публичные ключи, но не имеет доступа к содержимому сообщений:
// приватные ключи и расшифровки к нам не попадают по модели Matrix.
type KeysService struct {
	store      KeysStore
	serverName string
	// deviceLists фиксирует изменение ключей устройства для /keys/changes и
	// device_lists в /sync; nil - не фиксировать.
	deviceLists   DeviceListRecorder
	onRecordError func(ctx context.Context, err error)
}

// NewKeysService создаёт сервис ключей.
func NewKeysService(store KeysStore, serverName string) *KeysService {
	return &KeysService{store: store, serverName: serverName}
}

// WithDeviceLists включает учет изменений ключей устройств. Сбой учета не влияет
// на ответ (ключи уже сохранены) и отдается onRecordError, если он задан.
func (s *KeysService) WithDeviceLists(rec DeviceListRecorder, onRecordError func(ctx context.Context, err error)) *KeysService {
	s.deviceLists, s.onRecordError = rec, onRecordError

	return s
}

// DeviceKeys — присланные ключи устройства (подмножество, которое нам нужно для проверок).
type DeviceKeys struct {
	UserID     string                       `json:"user_id"`
	DeviceID   string                       `json:"device_id"`
	Algorithms []string                     `json:"algorithms"`
	Keys       map[string]string            `json:"keys"`
	Signatures map[string]map[string]string `json:"signatures"`
}

// UploadRequest — тело /keys/upload.
type UploadRequest struct {
	DeviceKeys   json.RawMessage            `json:"device_keys"`
	OneTimeKeys  map[string]json.RawMessage `json:"one_time_keys"`
	FallbackKeys map[string]json.RawMessage `json:"fallback_keys"`
}

// Upload сохраняет ключи устройства и возвращает счётчики невыданных one-time keys.
func (s *KeysService) Upload(
	ctx context.Context, userID, deviceID string, req UploadRequest,
) (map[string]int, error) {
	if len(req.DeviceKeys) > 0 {
		var keys DeviceKeys
		if err := json.Unmarshal(req.DeviceKeys, &keys); err != nil {
			return nil, ErrBadJSON
		}

		// Ключи должны принадлежать тому же пользователю и устройству, что и токен:
		// иначе одно устройство могло бы подменить ключи другому.
		if keys.UserID != userID || keys.DeviceID != deviceID {
			return nil, ErrBadJSON
		}

		if err := s.store.UpsertDeviceKeys(ctx, userID, deviceID, req.DeviceKeys); err != nil {
			return nil, err
		}
	}

	if len(req.OneTimeKeys) > 0 {
		if err := s.store.UploadOneTimeKeys(ctx, userID, deviceID, req.OneTimeKeys); err != nil {
			return nil, err
		}
	}

	// Fallback keys раньше молча терялись: после исчерпания one-time keys
	// устройству нельзя было открыть Olm-сессию.
	if len(req.FallbackKeys) > 0 {
		if err := s.store.UploadFallbackKeys(ctx, userID, deviceID, req.FallbackKeys); err != nil {
			return nil, err
		}
	}

	counts, err := s.store.OneTimeKeyCounts(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}

	if s.deviceLists != nil && len(req.DeviceKeys) > 0 {
		if err := s.deviceLists.RecordDeviceKeysChange(ctx, userID, deviceID); err != nil && s.onRecordError != nil {
			s.onRecordError(ctx, err)
		}
	}

	return counts, nil
}

// Query отдаёт ключи запрошенных устройств. Запросы к чужим серверам отбрасываются:
// мы не имеем права раскрывать ключи пользователей других homeserver'ов.
func (s *KeysService) Query(
	ctx context.Context, requests map[string][]string,
) (map[string]map[string]json.RawMessage, error) {
	filtered := map[string][]string{}

	for userID, devices := range requests {
		if s.isLocal(userID) {
			filtered[userID] = devices
		}
	}

	return s.store.QueryDeviceKeys(ctx, filtered)
}

// Claim выдаёт по одному one-time key на (пользователь, устройство, алгоритм).
func (s *KeysService) Claim(
	ctx context.Context, requests map[string]map[string]string,
) (map[string]map[string]map[string]json.RawMessage, error) {
	filtered := map[string]map[string]string{}

	for userID, devices := range requests {
		if s.isLocal(userID) {
			filtered[userID] = devices
		}
	}

	return s.store.ClaimOneTimeKeys(ctx, filtered)
}

// isLocal сообщает, что user_id принадлежит нашему серверу.
func (s *KeysService) isLocal(userID string) bool {
	if !strings.HasPrefix(userID, "@") {
		return false
	}

	localpart, serverName, found := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	if !found || localpart == "" {
		return false
	}

	return serverName == s.serverName
}
