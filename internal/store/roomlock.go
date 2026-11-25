package store

import (
	"context"
	"fmt"
	"sync"
)

// roomLocks - очередь записи локальных событий комнаты в памяти процесса.
// Запись события берет advisory-блокировку комнаты внутри
// транзакции, и до этого ждущий уже держит соединение пула: под нагрузкой
// десятки send в одну большую комнату занимали весь пул ожиданием блокировки
// (pg_advisory_xact_lock 17,9 мс в среднем на профиле M). Теперь они ждут
// своей очереди здесь, без соединения, и advisory-блокировку берут почти
// без ожидания; она остается - ее же берут разрешение входящих событий и
// другие пути записи состояния.
type roomLocks struct {
	mu      sync.Mutex
	entries map[string]*roomLockEntry
}

type roomLockEntry struct {
	slot chan struct{}
	refs int
}

// lock занимает очередь комнаты; unlock обязателен. Ошибка - отмена контекста
// до получения очереди.
func (l *roomLocks) lock(ctx context.Context, roomID string) (unlock func(), err error) {
	l.mu.Lock()

	if l.entries == nil {
		l.entries = map[string]*roomLockEntry{}
	}

	entry, ok := l.entries[roomID]
	if !ok {
		entry = &roomLockEntry{slot: make(chan struct{}, 1)}
		l.entries[roomID] = entry
	}

	entry.refs++
	l.mu.Unlock()

	release := func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, roomID)
		}
	}

	select {
	case entry.slot <- struct{}{}:
	case <-ctx.Done():
		release()

		return nil, fmt.Errorf("store: wait room %s: %w", roomID, ctx.Err())
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			<-entry.slot
			release()
		})
	}, nil
}
