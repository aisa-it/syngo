package core

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// UIASession — состояние интерактивной аутентификации (UIA) для одного запроса
// регистрации. Synapse хранит такие сессии в БД (`ui_auth_sessions`); на этапе MVP
// держим их в памяти процесса, пока регистрация одношаговая (m.login.dummy).
type UIASession struct {
	ID         string
	ClientData []byte
	Completed  []string
	CreatedAt  time.Time
}

// UIASessionStore хранит сессии UIA между запросами регистрации.
type UIASessionStore interface {
	Create(clientData []byte) (UIASession, error)
	Get(id string) (UIASession, bool)
	Delete(id string)
}

// MemoryUIASessions — потокобезопасное хранилище сессий UIA в памяти.
//
// Ограничения (осознанно для MVP): состояние теряется при перезапуске и не общее для
// нескольких инстансов. Когда появится горизонтальное масштабирование — таблица в БД
// (аналог ui_auth_sessions) без изменения контракта.
type MemoryUIASessions struct {
	mu       sync.Mutex
	sessions map[string]UIASession
	ttl      time.Duration
	max      int
	now      func() time.Time
}

// UIASessionTTL — сколько живёт сессия регистрации (как у Synapse: сутки на прохождение).
const UIASessionTTL = 24 * time.Hour

// maxUIASessions ограничивает память под незавершённые регистрации.
const maxUIASessions = 4096

// NewMemoryUIASessions создаёт хранилище сессий с TTL по умолчанию.
func NewMemoryUIASessions() *MemoryUIASessions {
	return &MemoryUIASessions{
		sessions: make(map[string]UIASession),
		ttl:      UIASessionTTL,
		max:      maxUIASessions,
		now:      time.Now,
	}
}

// WithClock подменяет время (используется в тестах).
func (m *MemoryUIASessions) WithClock(now func() time.Time) *MemoryUIASessions {
	m.now = now

	return m
}

// Create сохраняет новую сессию и возвращает её идентификатор.
func (m *MemoryUIASessions) Create(clientData []byte) (UIASession, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return UIASession{}, err
	}

	session := UIASession{
		ID:         base64.RawURLEncoding.EncodeToString(buf),
		ClientData: clientData,
		CreatedAt:  m.now(),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.sweepLocked()

	// При переполнении вытесняем самую старую сессию: регистрация — редкая операция,
	// отказ в обслуживании важнее точности вытеснения.
	if len(m.sessions) >= m.max {
		var oldestID string

		var oldest time.Time

		for id, candidate := range m.sessions {
			if oldestID == "" || candidate.CreatedAt.Before(oldest) {
				oldestID, oldest = id, candidate.CreatedAt
			}
		}

		delete(m.sessions, oldestID)
	}

	m.sessions[session.ID] = session

	return session, nil
}

// Get возвращает живую сессию по идентификатору.
func (m *MemoryUIASessions) Get(id string) (UIASession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return UIASession{}, false
	}

	if m.now().Sub(session.CreatedAt) > m.ttl {
		delete(m.sessions, id)

		return UIASession{}, false
	}

	return session, true
}

// Complete отмечает пройденную стадию (m.login.sso после возврата от провайдера);
// false - сессии нет или она истекла.
func (m *MemoryUIASessions) Complete(id, stage string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok || m.now().Sub(session.CreatedAt) > m.ttl {
		delete(m.sessions, id)

		return false
	}

	session.Completed = append(session.Completed, stage)
	m.sessions[id] = session

	return true
}

// Delete удаляет сессию (успешная регистрация или явный отказ).
func (m *MemoryUIASessions) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, id)
}

// sweepLocked вычищает просроченные сессии; вызывается под захваченным мьютексом.
func (m *MemoryUIASessions) sweepLocked() {
	now := m.now()

	for id, session := range m.sessions {
		if now.Sub(session.CreatedAt) > m.ttl {
			delete(m.sessions, id)
		}
	}
}
