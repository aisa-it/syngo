package coretest

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// MemSSOStore - in-memory core.SSOStore: привязки внешних учеток и
// loginToken с той же семантикой, что у PostgreSQL (одноразовое погашение).
type MemSSOStore struct {
	mu     sync.Mutex
	links  map[string]string // provider|subject -> user_id
	tokens map[string]core.LoginToken
	used   map[string]bool
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemSSOStore создает пустое хранилище.
func NewMemSSOStore() *MemSSOStore {
	return &MemSSOStore{links: map[string]string{}, tokens: map[string]core.LoginToken{}, used: map[string]bool{}}
}

// UserIDByExternalID - учетка по привязке.
func (m *MemSSOStore) UserIDByExternalID(_ context.Context, provider, externalID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return "", m.Err
	}

	userID, ok := m.links[provider+"|"+externalID]
	if !ok {
		return "", core.ErrNotFound
	}

	return userID, nil
}

// AddExternalID привязывает внешний идентификатор.
func (m *MemSSOStore) AddExternalID(_ context.Context, provider, externalID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	key := provider + "|" + externalID
	if _, ok := m.links[key]; ok {
		return core.ErrExternalIDInUse
	}

	m.links[key] = userID

	return nil
}

// ExternalProviders - провайдеры учетки в алфавитном порядке.
func (m *MemSSOStore) ExternalProviders(_ context.Context, userID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []string

	for key, linked := range m.links {
		if linked != userID {
			continue
		}

		if provider, _, _ := strings.Cut(key, "|"); !slices.Contains(out, provider) {
			out = append(out, provider)
		}
	}

	slices.Sort(out)

	return out, nil
}

// CreateLoginToken сохраняет токен.
func (m *MemSSOStore) CreateLoginToken(_ context.Context, token core.LoginToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.tokens[hex.EncodeToString(token.TokenHash)] = token

	return nil
}

// ConsumeLoginToken гасит токен один раз.
func (m *MemSSOStore) ConsumeLoginToken(_ context.Context, tokenHash []byte, now time.Time) (core.LoginToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return core.LoginToken{}, m.Err
	}

	key := hex.EncodeToString(tokenHash)

	token, ok := m.tokens[key]
	if !ok || m.used[key] || !token.ExpiresAt.After(now) {
		return core.LoginToken{}, core.ErrNotFound
	}

	m.used[key] = true

	return token, nil
}
