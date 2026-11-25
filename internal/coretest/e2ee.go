package coretest

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
)

// MemKeyBackupStore - in-memory core.KeyBackupStore. Лучший ключ
// сессии не выбирается: новый ключ заменяет прежний (выбор проверяется на живой
// БД в store).
type MemKeyBackupStore struct {
	mu       sync.Mutex
	versions map[string][]memBackupVersion // user -> версии в порядке создания
	keys     map[string]core.RoomKeys      // user|version -> ключи
}

type memBackupVersion struct {
	core.RoomKeyVersion
	deleted bool
}

// NewMemKeyBackupStore создает пустое хранилище.
func NewMemKeyBackupStore() *MemKeyBackupStore {
	return &MemKeyBackupStore{versions: map[string][]memBackupVersion{}, keys: map[string]core.RoomKeys{}}
}

func (m *MemKeyBackupStore) version(userID, version string) (core.RoomKeyVersion, bool) {
	list := m.versions[userID]
	for i := len(list) - 1; i >= 0; i-- {
		if !list[i].deleted && (version == "" || list[i].Version == version) {
			out := list[i].RoomKeyVersion
			out.Count = m.count(userID, out.Version)

			return out, true
		}
	}

	return core.RoomKeyVersion{}, false
}

func (m *MemKeyBackupStore) count(userID, version string) int {
	n := 0
	for _, sessions := range m.keys[userID+"|"+version] {
		n += len(sessions)
	}

	return n
}

// CurrentRoomKeyVersion - последняя неудаленная версия.
func (m *MemKeyBackupStore) CurrentRoomKeyVersion(_ context.Context, userID string) (core.RoomKeyVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if v, ok := m.version(userID, ""); ok {
		return v, nil
	}

	return core.RoomKeyVersion{}, core.ErrRoomKeysNotFound
}

// RoomKeyVersionByID - конкретная неудаленная версия.
func (m *MemKeyBackupStore) RoomKeyVersionByID(_ context.Context, userID, version string) (core.RoomKeyVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if version == "" {
		return core.RoomKeyVersion{}, core.ErrRoomKeysNotFound
	}

	if v, ok := m.version(userID, version); ok {
		return v, nil
	}

	return core.RoomKeyVersion{}, core.ErrRoomKeysNotFound
}

// CreateRoomKeyVersion создает версию с номером на единицу больше последней.
func (m *MemKeyBackupStore) CreateRoomKeyVersion(
	_ context.Context, userID, algorithm string, authData json.RawMessage,
) (core.RoomKeyVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	v := core.RoomKeyVersion{
		Version: strconv.Itoa(len(m.versions[userID]) + 1), Algorithm: algorithm, AuthData: authData, Etag: "0",
	}
	m.versions[userID] = append(m.versions[userID], memBackupVersion{RoomKeyVersion: v})

	return v, nil
}

// UpdateRoomKeyVersion меняет algorithm и auth_data версии.
func (m *MemKeyBackupStore) UpdateRoomKeyVersion(
	_ context.Context, userID, version, algorithm string, authData json.RawMessage,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.versions[userID] {
		v := &m.versions[userID][i]
		if v.Version == version && !v.deleted {
			v.Algorithm, v.AuthData = algorithm, authData

			return nil
		}
	}

	return core.ErrRoomKeysNotFound
}

// DeleteRoomKeyVersion помечает версию удаленной и стирает ее ключи.
func (m *MemKeyBackupStore) DeleteRoomKeyVersion(_ context.Context, userID, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.versions[userID] {
		v := &m.versions[userID][i]
		if v.Version == version && !v.deleted {
			v.deleted = true
			delete(m.keys, userID+"|"+version)

			return nil
		}
	}

	return core.ErrRoomKeysNotFound
}

// SaveRoomKeys сохраняет ключи в существующую версию.
func (m *MemKeyBackupStore) SaveRoomKeys(
	_ context.Context, userID, version string, keys core.RoomKeys,
) (core.RoomKeysStat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.version(userID, version); !ok {
		return core.RoomKeysStat{}, core.ErrRoomKeysNotFound
	}

	k := userID + "|" + version
	if m.keys[k] == nil {
		m.keys[k] = core.RoomKeys{}
	}

	for roomID, sessions := range keys {
		if m.keys[k][roomID] == nil {
			m.keys[k][roomID] = map[string]json.RawMessage{}
		}

		for sessionID, key := range sessions {
			m.keys[k][roomID][sessionID] = key
		}
	}

	return core.RoomKeysStat{Etag: "1", Count: m.count(userID, version)}, nil
}

// DeleteRoomKeys удаляет ключи версии; roomID и sessionID сужают выборку.
func (m *MemKeyBackupStore) DeleteRoomKeys(
	_ context.Context, userID, version, roomID, sessionID string,
) (core.RoomKeysStat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.version(userID, version); !ok {
		return core.RoomKeysStat{}, core.ErrRoomKeysNotFound
	}

	for rid, sessions := range m.keys[userID+"|"+version] {
		if roomID != "" && rid != roomID {
			continue
		}

		for sid := range sessions {
			if sessionID == "" || sid == sessionID {
				delete(sessions, sid)
			}
		}
	}

	return core.RoomKeysStat{Etag: "2", Count: m.count(userID, version)}, nil
}

// RoomKeys возвращает ключи версии; roomID и sessionID сужают выборку.
func (m *MemKeyBackupStore) RoomKeys(_ context.Context, userID, version, roomID, sessionID string) (core.RoomKeys, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := core.RoomKeys{}

	for rid, sessions := range m.keys[userID+"|"+version] {
		for sid, key := range sessions {
			if (roomID == "" || rid == roomID) && (sessionID == "" || sid == sessionID) {
				if out[rid] == nil {
					out[rid] = map[string]json.RawMessage{}
				}

				out[rid][sid] = key
			}
		}
	}

	return out, nil
}

// MemKeysStore - in-memory core.KeysStore: ключи устройств и one-time keys.
type MemKeysStore struct {
	mu      sync.Mutex
	devices map[string]map[string]json.RawMessage // user -> device -> device_keys
	otk     map[string]map[string]json.RawMessage // user|device -> key_id -> key
}

// NewMemKeysStore создает пустое хранилище.
func NewMemKeysStore() *MemKeysStore {
	return &MemKeysStore{devices: map[string]map[string]json.RawMessage{}, otk: map[string]map[string]json.RawMessage{}}
}

// UpsertDeviceKeys сохраняет device_keys.
func (m *MemKeysStore) UpsertDeviceKeys(_ context.Context, userID, deviceID string, keys json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.devices[userID] == nil {
		m.devices[userID] = map[string]json.RawMessage{}
	}

	m.devices[userID][deviceID] = keys

	return nil
}

// UploadOneTimeKeys добавляет one-time keys.
func (m *MemKeysStore) UploadOneTimeKeys(_ context.Context, userID, deviceID string, keys map[string]json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := userID + "|" + deviceID
	if m.otk[k] == nil {
		m.otk[k] = map[string]json.RawMessage{}
	}

	for keyID, key := range keys {
		m.otk[k][keyID] = key
	}

	return nil
}

// UploadFallbackKeys - fallback keys не хранятся.
func (m *MemKeysStore) UploadFallbackKeys(context.Context, string, string, map[string]json.RawMessage) error {
	return nil
}

// OneTimeKeyCounts - число one-time keys по алгоритмам.
func (m *MemKeysStore) OneTimeKeyCounts(_ context.Context, userID, deviceID string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	counts := map[string]int{}
	for keyID := range m.otk[userID+"|"+deviceID] {
		algorithm, _, _ := strings.Cut(keyID, ":")
		counts[algorithm]++
	}

	return counts, nil
}

// QueryDeviceKeys - ключи запрошенных устройств (пустой список - все устройства).
func (m *MemKeysStore) QueryDeviceKeys(
	_ context.Context, requests map[string][]string,
) (map[string]map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]map[string]json.RawMessage{}

	for userID, devices := range requests {
		for deviceID, keys := range m.devices[userID] {
			if len(devices) > 0 && !slices.Contains(devices, deviceID) {
				continue
			}

			if out[userID] == nil {
				out[userID] = map[string]json.RawMessage{}
			}

			out[userID][deviceID] = keys
		}
	}

	return out, nil
}

// ClaimOneTimeKeys выдает и удаляет по одному one-time key.
func (m *MemKeysStore) ClaimOneTimeKeys(
	_ context.Context, requests map[string]map[string]string,
) (map[string]map[string]map[string]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]map[string]map[string]json.RawMessage{}

	for userID, devices := range requests {
		for deviceID, algorithm := range devices {
			pool := m.otk[userID+"|"+deviceID]

			ids := make([]string, 0, len(pool))
			for keyID := range pool {
				ids = append(ids, keyID)
			}

			sort.Strings(ids)

			for _, keyID := range ids {
				if strings.HasPrefix(keyID, algorithm+":") {
					if out[userID] == nil {
						out[userID] = map[string]map[string]json.RawMessage{}
					}

					out[userID][deviceID] = map[string]json.RawMessage{keyID: pool[keyID]}
					delete(pool, keyID)

					break
				}
			}
		}
	}

	return out, nil
}

// MemCrossSigningStore - in-memory core.CrossSigningStore.
type MemCrossSigningStore struct {
	mu         sync.Mutex
	keys       map[string]map[string]core.CrossSigningKey
	signatures []core.Signature
	// Changes - зафиксированные изменения device-листов (user|device).
	Changes []string
	// RecordErr - ошибка, которую вернет RecordDeviceKeysChange.
	RecordErr error
}

// NewMemCrossSigningStore создает пустое хранилище.
func NewMemCrossSigningStore() *MemCrossSigningStore {
	return &MemCrossSigningStore{keys: map[string]map[string]core.CrossSigningKey{}}
}

// CrossSigningKeys - ключи пользователей: user -> тип -> ключ.
func (m *MemCrossSigningStore) CrossSigningKeys(
	_ context.Context, userIDs []string,
) (map[string]map[string]core.CrossSigningKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]map[string]core.CrossSigningKey{}

	for _, userID := range userIDs {
		for keyType, key := range m.keys[userID] {
			if out[userID] == nil {
				out[userID] = map[string]core.CrossSigningKey{}
			}

			out[userID][keyType] = key
		}
	}

	return out, nil
}

// SaveCrossSigningKeys сохраняет ключи (новый ключ типа заменяет прежний).
func (m *MemCrossSigningStore) SaveCrossSigningKeys(_ context.Context, keys []core.CrossSigningKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range keys {
		if m.keys[key.UserID] == nil {
			m.keys[key.UserID] = map[string]core.CrossSigningKey{}
		}

		m.keys[key.UserID][key.KeyType] = key
	}

	return nil
}

// SaveSignatures сохраняет подписи (повторная подпись тем же ключом заменяет прежнюю).
func (m *MemCrossSigningStore) SaveSignatures(_ context.Context, signatures []core.Signature) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sig := range signatures {
		replaced := false

		for i, old := range m.signatures {
			if old.TargetUserID == sig.TargetUserID && old.TargetKeyID == sig.TargetKeyID &&
				old.SignerUserID == sig.SignerUserID && old.SignerKeyID == sig.SignerKeyID {
				m.signatures[i] = sig
				replaced = true
			}
		}

		if !replaced {
			m.signatures = append(m.signatures, sig)
		}
	}

	return nil
}

// SignaturesFor - подписи над ключами пользователей.
func (m *MemCrossSigningStore) SignaturesFor(_ context.Context, targetUserIDs []string) ([]core.Signature, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.Signature

	for _, sig := range m.signatures {
		if slices.Contains(targetUserIDs, sig.TargetUserID) {
			out = append(out, sig)
		}
	}

	return out, nil
}

// RecordDeviceKeysChange запоминает изменение или возвращает RecordErr.
func (m *MemCrossSigningStore) RecordDeviceKeysChange(_ context.Context, userID, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.RecordErr != nil {
		return m.RecordErr
	}

	m.Changes = append(m.Changes, userID+"|"+deviceID)

	return nil
}
