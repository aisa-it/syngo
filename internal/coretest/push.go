package coretest

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushers"
)

// MemPushRulesStore - in-memory core.PushRulesStore: правила
// пользователя в порядке приоритета и записанные снимки account data.
type MemPushRulesStore struct {
	mu    sync.Mutex
	rules map[string][]core.PushRule
	// AccountData - последний content по user|room|type.
	AccountData map[string][]byte
	// Err - ошибка, которую вернут все методы (сбой хранилища).
	Err error
}

// NewMemPushRulesStore создает пустое хранилище.
func NewMemPushRulesStore() *MemPushRulesStore {
	return &MemPushRulesStore{rules: map[string][]core.PushRule{}, AccountData: map[string][]byte{}}
}

// PushRules - правила пользователя (scope "" - все).
func (m *MemPushRulesStore) PushRules(_ context.Context, userID, scope string) ([]core.PushRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.PushRule

	for i := range m.rules[userID] {
		if rule := &m.rules[userID][i]; scope == "" || rule.Scope == scope {
			out = append(out, *rule)
		}
	}

	return out, nil
}

func (m *MemPushRulesStore) find(userID, scope, kind, ruleID string) int {
	return slices.IndexFunc(m.rules[userID], func(r core.PushRule) bool {
		return r.Scope == scope && r.Kind == kind && r.RuleID == ruleID
	})
}

// PutUserPushRule ставит правило по before/after, без них - первым в виде.
func (m *MemPushRulesStore) PutUserPushRule(_ context.Context, userID string, rule core.PushRule, before, after string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	if i := m.find(userID, rule.Scope, rule.Kind, rule.RuleID); i >= 0 {
		m.rules[userID] = slices.Delete(m.rules[userID], i, i+1)
	}

	pos := slices.IndexFunc(m.rules[userID], func(r core.PushRule) bool { return r.Kind == rule.Kind })
	if pos < 0 {
		pos = len(m.rules[userID])
	}

	if anchor := before + after; anchor != "" {
		i := m.find(userID, rule.Scope, rule.Kind, anchor)
		if i < 0 {
			return core.ErrPushRuleAnchorNotFound
		}

		pos = i
		if after != "" {
			pos = i + 1
		}
	}

	m.rules[userID] = slices.Insert(m.rules[userID], pos, rule)

	return nil
}

// SetUserPushRuleAttr меняет enabled/actions правила; false - правила нет.
func (m *MemPushRulesStore) SetUserPushRuleAttr(
	_ context.Context, userID, scope, kind, ruleID string, enabled *bool, actions json.RawMessage,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	i := m.find(userID, scope, kind, ruleID)
	if i < 0 {
		return false, nil
	}

	rule := &m.rules[userID][i]
	if enabled != nil {
		rule.Enabled = *enabled
	}

	if actions != nil {
		rule.Actions = actions
	}

	return true, nil
}

// SetDefaultPushRuleAttr сохраняет переопределение серверного правила.
func (m *MemPushRulesStore) SetDefaultPushRuleAttr(
	_ context.Context, userID, scope, kind, ruleID string, defaultEnabled bool, enabled *bool, actions json.RawMessage,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	i := m.find(userID, scope, kind, ruleID)
	if i < 0 {
		m.rules[userID] = append(m.rules[userID], core.PushRule{
			Scope: scope, Kind: kind, RuleID: ruleID, Enabled: defaultEnabled,
			Conditions: json.RawMessage(`null`), Actions: json.RawMessage(`null`),
		})
		i = len(m.rules[userID]) - 1
	}

	rule := &m.rules[userID][i]
	if enabled != nil {
		rule.Enabled = *enabled
	}

	if actions != nil {
		rule.Actions = actions
	}

	return nil
}

// DeletePushRule удаляет правило; false - его не было.
func (m *MemPushRulesStore) DeletePushRule(_ context.Context, userID, scope, kind, ruleID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	i := m.find(userID, scope, kind, ruleID)
	if i < 0 {
		return false, nil
	}

	m.rules[userID] = slices.Delete(m.rules[userID], i, i+1)

	return true, nil
}

// SetAccountData запоминает content.
func (m *MemPushRulesStore) SetAccountData(_ context.Context, userID, roomID, eventType string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.AccountData[userID+"|"+roomID+"|"+eventType] = content

	return nil
}

// MemPushersStore - in-memory core.PushersStore.
type MemPushersStore struct {
	mu   sync.Mutex
	list map[string][]pushers.Pusher
}

// NewMemPushersStore создает пустое хранилище.
func NewMemPushersStore() *MemPushersStore {
	return &MemPushersStore{list: map[string][]pushers.Pusher{}}
}

// UpsertPusher добавляет или заменяет pusher (app_id+pushkey) с учетом лимита.
func (m *MemPushersStore) UpsertPusher(_ context.Context, pusher pushers.Pusher, maxPerUser int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := m.list[pusher.UserID]
	i := slices.IndexFunc(list, func(p pushers.Pusher) bool { return p.AppID == pusher.AppID && p.Pushkey == pusher.Pushkey })

	if i >= 0 {
		list[i] = pusher

		return nil
	}

	if maxPerUser > 0 && len(list) >= maxPerUser {
		return core.ErrTooManyPushers
	}

	m.list[pusher.UserID] = append(list, pusher)

	return nil
}

// DeletePusher удаляет pusher.
func (m *MemPushersStore) DeletePusher(_ context.Context, userID, appID, pushkey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.list[userID] = slices.DeleteFunc(m.list[userID], func(p pushers.Pusher) bool {
		return p.AppID == appID && p.Pushkey == pushkey
	})

	return nil
}

// Pushers - pushers пользователя (nil, если их нет).
func (m *MemPushersStore) Pushers(_ context.Context, userID string) ([]pushers.Pusher, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Clone(m.list[userID]), nil
}
