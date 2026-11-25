package api

import "sync"

// keyedLock — мьютекс на ключ с подсчётом ссылок: после последнего ожидающего
// запись удаляется, поэтому map не растёт по числу уникальных ключей.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock захватывает мьютекс ключа; вернуть функцию нужно через defer.
func (k *keyedLock) Lock(key string) func() {
	k.mu.Lock()

	if k.entries == nil {
		k.entries = map[string]*keyedLockEntry{}
	}

	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedLockEntry{}
		k.entries[key] = entry
	}

	entry.refs++
	k.mu.Unlock()

	entry.mu.Lock()

	return func() {
		entry.mu.Unlock()

		k.mu.Lock()
		entry.refs--

		if entry.refs == 0 {
			delete(k.entries, key)
		}

		k.mu.Unlock()
	}
}

// Len — число активных ключей (для тестов).
func (k *keyedLock) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return len(k.entries)
}
