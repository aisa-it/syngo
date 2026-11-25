package coretest

import (
	"context"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
)

// MemVisibilityStore - in-memory core.VisibilityStore: заданные
// границы видимости по комнатам.
type MemVisibilityStore struct {
	mu sync.Mutex
	// Cutoffs - границы по комнатам (для всех пользователей).
	Cutoffs map[string]core.Visibility
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemVisibilityStore создает хранилище без ограничений видимости.
func NewMemVisibilityStore() *MemVisibilityStore {
	return &MemVisibilityStore{Cutoffs: map[string]core.Visibility{}}
}

// VisibilityCutoffs - границы запрошенных комнат; без записи - вся история.
func (m *MemVisibilityStore) VisibilityCutoffs(_ context.Context, _ string, roomIDs []string) (map[string]core.Visibility, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := make(map[string]core.Visibility, len(roomIDs))
	for _, roomID := range roomIDs {
		out[roomID] = m.Cutoffs[roomID]
	}

	return out, nil
}

// MemReadMarkerStore - in-memory core.ReadMarkerStore.
type MemReadMarkerStore struct {
	mu sync.Mutex
	// Receipts - сохраненные отметки по порядку записи.
	Receipts []core.ReceiptEvent
	// AccountData - content по user|room|type.
	AccountData map[string][]byte
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemReadMarkerStore создает пустое хранилище.
func NewMemReadMarkerStore() *MemReadMarkerStore {
	return &MemReadMarkerStore{AccountData: map[string][]byte{}}
}

// UpsertReceipt сохраняет отметку.
func (m *MemReadMarkerStore) UpsertReceipt(_ context.Context, receipt core.ReceiptEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.Receipts = append(m.Receipts, receipt)

	return nil
}

// AllReceipts - все сохраненные отметки.
func (m *MemReadMarkerStore) AllReceipts(context.Context) ([]core.ReceiptEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	return append([]core.ReceiptEvent(nil), m.Receipts...), nil
}

// SetAccountData сохраняет account data.
func (m *MemReadMarkerStore) SetAccountData(_ context.Context, userID, roomID, eventType string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.AccountData[userID+"|"+roomID+"|"+eventType] = content

	return nil
}
