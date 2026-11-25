package coretest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
)

// MemToDeviceStore - in-memory core.ToDeviceStore: очередь
// сообщений по устройствам и заданный список изменений ключей.
type MemToDeviceStore struct {
	mu   sync.Mutex
	txns map[string]bool
	// Queued - сообщения по устройствам получателей.
	Queued map[core.DeviceTarget][]json.RawMessage
	// Changed - что вернет DeviceListChangesFor (окно не учитывается).
	Changed []string
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemToDeviceStore создает пустое хранилище.
func NewMemToDeviceStore() *MemToDeviceStore {
	return &MemToDeviceStore{txns: map[string]bool{}, Queued: map[core.DeviceTarget][]json.RawMessage{}}
}

// SendToDevice ставит сообщения в очередь; повтор txn - без записи.
func (m *MemToDeviceStore) SendToDevice(
	_ context.Context,
	userID, deviceID, eventType, txnID string,
	messages map[core.DeviceTarget]json.RawMessage,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	key := userID + "|" + deviceID + "|" + eventType + "|" + txnID
	if m.txns[key] {
		return true, nil
	}

	m.txns[key] = true

	for target, content := range messages {
		m.Queued[target] = append(m.Queued[target], content)
	}

	return false, nil
}

// DeviceListChangesFor отдает Changed по порядку.
func (m *MemToDeviceStore) DeviceListChangesFor(_ context.Context, _ string, _, _ int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	if m.Changed == nil {
		return nil, nil
	}

	out := append([]string(nil), m.Changed...)
	sort.Strings(out)

	return out, nil
}

// MemFilterStore - in-memory core.FilterStore.
type MemFilterStore struct {
	mu      sync.Mutex
	filters map[string]json.RawMessage
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemFilterStore создает пустое хранилище.
func NewMemFilterStore() *MemFilterStore {
	return &MemFilterStore{filters: map[string]json.RawMessage{}}
}

// SaveUserFilter сохраняет фильтр.
func (m *MemFilterStore) SaveUserFilter(_ context.Context, userID, filterID string, filter json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.filters[userID+"|"+filterID] = filter

	return nil
}

// UserFilter - сохраненный фильтр; нет - core.ErrNotFound.
func (m *MemFilterStore) UserFilter(_ context.Context, userID, filterID string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	raw, ok := m.filters[userID+"|"+filterID]
	if !ok {
		return nil, core.ErrNotFound
	}

	return raw, nil
}
