package coretest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
)

// MemAccountDataStore - in-memory core.AccountDataStore. Каждая
// запись помнит позицию потока; удаление оставляет content {} (MSC3391).
type MemAccountDataStore struct {
	mu      sync.Mutex
	records map[memADKey]memADRecord
	stream  int64
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

type memADKey struct{ user, room, typ string }

type memADRecord struct {
	content []byte
	pos     int64
}

// NewMemAccountDataStore создает пустое хранилище.
func NewMemAccountDataStore() *MemAccountDataStore {
	return &MemAccountDataStore{records: map[memADKey]memADRecord{}}
}

// Position - текущая позиция потока account data.
func (m *MemAccountDataStore) Position() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stream
}

func (m *MemAccountDataStore) put(key memADKey, content []byte) {
	m.stream++
	m.records[key] = memADRecord{content: content, pos: m.stream}
}

// AccountData - content записи; удаленная или отсутствующая - ErrNotFound.
func (m *MemAccountDataStore) AccountData(_ context.Context, userID, roomID, eventType string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	rec, ok := m.records[memADKey{userID, roomID, eventType}]
	if !ok || string(rec.content) == "{}" {
		return nil, fmt.Errorf("coretest: account data %s: %w", eventType, core.ErrNotFound)
	}

	return rec.content, nil
}

// SetAccountData сохраняет запись с новой позицией.
func (m *MemAccountDataStore) SetAccountData(_ context.Context, userID, roomID, eventType string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.put(memADKey{userID, roomID, eventType}, content)

	return nil
}

// DeleteAccountData помечает запись удаленной (content {}).
func (m *MemAccountDataStore) DeleteAccountData(_ context.Context, userID, roomID, eventType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	key := memADKey{userID, roomID, eventType}
	if rec, ok := m.records[key]; !ok || string(rec.content) == "{}" {
		return fmt.Errorf("coretest: account data %s: %w", eventType, core.ErrNotFound)
	}

	m.put(key, []byte(`{}`))

	return nil
}

// AccountDataChanges - записи из (since, upper]; при since == 0 и для fullRooms -
// все неудаленные до upper.
func (m *MemAccountDataStore) AccountDataChanges(
	_ context.Context, userID string, since, upper int64, fullRooms []string,
) (global map[string][]byte, perRoom map[string]map[string][]byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, nil, m.Err
	}

	global, perRoom = map[string][]byte{}, map[string]map[string][]byte{}

	for key, rec := range m.records {
		if key.user != userID || rec.pos > upper {
			continue
		}

		full := (since == 0 || slices.Contains(fullRooms, key.room)) && string(rec.content) != "{}"
		if changed := since > 0 && rec.pos > since; !changed && !full {
			continue
		}

		if key.room == "" {
			global[key.typ] = rec.content

			continue
		}

		if perRoom[key.room] == nil {
			perRoom[key.room] = map[string][]byte{}
		}

		perRoom[key.room][key.typ] = rec.content
	}

	return global, perRoom, nil
}

// SetRoomTag добавляет или заменяет тег в m.tag.
func (m *MemAccountDataStore) SetRoomTag(_ context.Context, userID, roomID, tag string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	key := memADKey{userID, roomID, core.AccountDataTag}
	tags := m.tags(key)
	tags[tag] = content

	return m.putTags(key, tags)
}

// DeleteRoomTag удаляет тег; отсутствующий тег - не ошибка.
func (m *MemAccountDataStore) DeleteRoomTag(_ context.Context, userID, roomID, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	key := memADKey{userID, roomID, core.AccountDataTag}

	tags := m.tags(key)
	if _, ok := tags[tag]; !ok {
		return nil
	}

	delete(tags, tag)

	return m.putTags(key, tags)
}

func (m *MemAccountDataStore) tags(key memADKey) map[string]json.RawMessage {
	var parsed struct {
		Tags map[string]json.RawMessage `json:"tags"`
	}

	if rec, ok := m.records[key]; ok {
		_ = json.Unmarshal(rec.content, &parsed)
	}

	if parsed.Tags == nil {
		parsed.Tags = map[string]json.RawMessage{}
	}

	return parsed.Tags
}

func (m *MemAccountDataStore) putTags(key memADKey, tags map[string]json.RawMessage) error {
	content, err := json.Marshal(map[string]any{"tags": tags})
	if err != nil {
		return err
	}

	m.put(key, content)

	return nil
}
