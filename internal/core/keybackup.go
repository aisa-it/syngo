package core

import (
	"context"
	"encoding/json"
	"errors"
)

// Серверный бэкап ключей E2EE (/room_keys/*). Element через
// него восстанавливает историю на новом устройстве. Логика перенесена из
// HTTP-обработчиков.

// ErrRoomKeysNotFound - нет бэкапа ключей, версии или сессии (404 M_NOT_FOUND).
var ErrRoomKeysNotFound = errors.New("core: room keys not found")

// RoomKeyVersion - версия серверного бэкапа ключей.
type RoomKeyVersion struct {
	Version   string          `json:"version"`
	Algorithm string          `json:"algorithm"`
	AuthData  json.RawMessage `json:"auth_data"`
	Etag      string          `json:"etag"`
	Count     int             `json:"count"`
}

// RoomKeysStat - etag и число ключей версии (ответ PUT/DELETE /room_keys/keys).
type RoomKeysStat struct {
	Etag  string `json:"etag"`
	Count int    `json:"count"`
}

// RoomKeys - ключи бэкапа: room_id -> session_id -> KeyBackupData.
type RoomKeys = map[string]map[string]json.RawMessage

// KeyBackupStore - хранение бэкапа ключей. Отсутствие версии или ключей -
// ErrRoomKeysNotFound.
type KeyBackupStore interface {
	// CurrentRoomKeyVersion - последняя неудаленная версия пользователя.
	CurrentRoomKeyVersion(ctx context.Context, userID string) (RoomKeyVersion, error)
	// RoomKeyVersionByID - конкретная неудаленная версия.
	RoomKeyVersionByID(ctx context.Context, userID, version string) (RoomKeyVersion, error)
	// CreateRoomKeyVersion создает версию; она становится текущей.
	CreateRoomKeyVersion(ctx context.Context, userID, algorithm string, authData json.RawMessage) (RoomKeyVersion, error)
	UpdateRoomKeyVersion(ctx context.Context, userID, version, algorithm string, authData json.RawMessage) error
	// DeleteRoomKeyVersion помечает версию удаленной и стирает ее ключи.
	DeleteRoomKeyVersion(ctx context.Context, userID, version string) error
	// SaveRoomKeys сохраняет ключи, оставляя лучший ключ каждой сессии.
	SaveRoomKeys(ctx context.Context, userID, version string, keys RoomKeys) (RoomKeysStat, error)
	// DeleteRoomKeys удаляет ключи версии; roomID и sessionID сужают выборку.
	DeleteRoomKeys(ctx context.Context, userID, version, roomID, sessionID string) (RoomKeysStat, error)
	// RoomKeys возвращает ключи версии; roomID и sessionID сужают выборку.
	RoomKeys(ctx context.Context, userID, version, roomID, sessionID string) (RoomKeys, error)
}

// WrongBackupVersionError - запись не в текущую версию бэкапа (403
// M_WRONG_ROOM_KEYS_VERSION): клиент узнает, что бэкап сменили на другом устройстве.
type WrongBackupVersionError struct {
	// Current - текущая версия ("" - бэкапа нет).
	Current string
}

// Error реализует error.
func (e *WrongBackupVersionError) Error() string {
	return "core: wrong room keys version, current " + e.Current
}

// KeyBackupService - доменный сервис бэкапа ключей.
type KeyBackupService struct {
	store KeyBackupStore
}

// NewKeyBackupService создает сервис бэкапа ключей.
func NewKeyBackupService(store KeyBackupStore) *KeyBackupService {
	return &KeyBackupService{store: store}
}

// Version возвращает версию бэкапа; version == "" - текущую.
func (s *KeyBackupService) Version(ctx context.Context, userID, version string) (RoomKeyVersion, error) {
	if version != "" {
		return s.store.RoomKeyVersionByID(ctx, userID, version)
	}

	return s.store.CurrentRoomKeyVersion(ctx, userID)
}

// CreateVersion создает новую версию бэкапа и возвращает ее номер.
func (s *KeyBackupService) CreateVersion(
	ctx context.Context, userID, algorithm string, authData json.RawMessage,
) (string, error) {
	if algorithm == "" || !IsJSONObject(authData) {
		return "", requestError(ErrMissingParam, "algorithm and auth_data are required")
	}

	version, err := s.store.CreateRoomKeyVersion(ctx, userID, algorithm, authData)
	if err != nil {
		return "", err
	}

	return version.Version, nil
}

// VersionUpdate - тело PUT /room_keys/version/{version}.
type VersionUpdate struct {
	Algorithm string
	AuthData  json.RawMessage
	// Version - версия из тела; если указана, должна совпадать с версией из пути.
	Version string
}

// UpdateVersion меняет auth_data версии; algorithm и version менять нельзя.
func (s *KeyBackupService) UpdateVersion(ctx context.Context, userID, versionID string, upd VersionUpdate) error {
	current, err := s.store.RoomKeyVersionByID(ctx, userID, versionID)
	if err != nil {
		return err
	}

	if (upd.Version != "" && upd.Version != versionID) || (upd.Algorithm != "" && upd.Algorithm != current.Algorithm) {
		return requestError(ErrInvalidParam, "version and algorithm cannot be changed")
	}

	if !IsJSONObject(upd.AuthData) {
		return requestError(ErrMissingParam, "auth_data is required")
	}

	return s.store.UpdateRoomKeyVersion(ctx, userID, versionID, current.Algorithm, upd.AuthData)
}

// DeleteVersion удаляет версию бэкапа вместе с ключами.
func (s *KeyBackupService) DeleteVersion(ctx context.Context, userID, version string) error {
	return s.store.DeleteRoomKeyVersion(ctx, userID, version)
}

// PutKeys сохраняет ключи в версию version. Запись разрешена только в текущую
// версию: иначе WrongBackupVersionError, неизвестная версия - 404.
func (s *KeyBackupService) PutKeys(ctx context.Context, userID, version string, keys RoomKeys) (RoomKeysStat, error) {
	for _, sessions := range keys {
		for _, key := range sessions {
			if !ValidBackupKey(key) {
				return RoomKeysStat{}, requestError(ErrMalformed, "Invalid key data")
			}
		}
	}

	if version == "" {
		return RoomKeysStat{}, requestError(ErrMissingParam, "version is required")
	}

	current, err := s.store.CurrentRoomKeyVersion(ctx, userID)
	if err != nil && !errors.Is(err, ErrRoomKeysNotFound) {
		return RoomKeysStat{}, err
	}

	if current.Version != version {
		if _, verr := s.store.RoomKeyVersionByID(ctx, userID, version); errors.Is(verr, ErrRoomKeysNotFound) {
			return RoomKeysStat{}, requestError(ErrNotFound, "Unknown backup version")
		}

		return RoomKeysStat{}, &WrongBackupVersionError{Current: current.Version}
	}

	return s.store.SaveRoomKeys(ctx, userID, version, keys)
}

// Keys возвращает ключи версии (version == "" - текущей, для старых клиентов);
// roomID и sessionID сужают выборку. Несуществующая версия - ErrRoomKeysNotFound.
func (s *KeyBackupService) Keys(ctx context.Context, userID, version, roomID, sessionID string) (RoomKeys, error) {
	version, err := s.resolveVersion(ctx, userID, version)
	if err != nil {
		return nil, err
	}

	return s.store.RoomKeys(ctx, userID, version, roomID, sessionID)
}

// DeleteKeys удаляет ключи версии (version == "" - текущей).
func (s *KeyBackupService) DeleteKeys(
	ctx context.Context, userID, version, roomID, sessionID string,
) (RoomKeysStat, error) {
	version, err := s.resolveVersion(ctx, userID, version)
	if err != nil {
		return RoomKeysStat{}, err
	}

	return s.store.DeleteRoomKeys(ctx, userID, version, roomID, sessionID)
}

// resolveVersion - номер версии из запроса; без него - текущая, несуществующая - 404.
func (s *KeyBackupService) resolveVersion(ctx context.Context, userID, version string) (string, error) {
	if version == "" {
		current, err := s.store.CurrentRoomKeyVersion(ctx, userID)

		return current.Version, err
	}

	if _, err := s.store.RoomKeyVersionByID(ctx, userID, version); err != nil {
		return "", err
	}

	return version, nil
}

// ValidBackupKey - KeyBackupData: first_message_index, forwarded_count, is_verified,
// session_data (объект).
func ValidBackupKey(raw json.RawMessage) bool {
	var key struct {
		FirstMessageIndex *int64          `json:"first_message_index"`
		ForwardedCount    *int64          `json:"forwarded_count"`
		IsVerified        *bool           `json:"is_verified"`
		SessionData       json.RawMessage `json:"session_data"`
	}

	return json.Unmarshal(raw, &key) == nil && key.FirstMessageIndex != nil && key.ForwardedCount != nil &&
		key.IsVerified != nil && IsJSONObject(key.SessionData)
}

// IsJSONObject сообщает, что raw - JSON-объект (не null и не другой тип).
func IsJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage

	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
