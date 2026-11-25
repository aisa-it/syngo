package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRoomLocksSerializeRoom - запись в одну комнату идет по очереди, в разные -
// параллельно, ожидание уважает отмену и не оставляет записей.
func TestRoomLocksSerializeRoom(t *testing.T) {
	t.Parallel()

	var (
		locks   roomLocks
		inRoom  atomic.Int64
		peak    atomic.Int64
		wg      sync.WaitGroup
		started = time.Now()
	)

	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			unlock, err := locks.lock(context.Background(), "!a")
			if err != nil {
				t.Errorf("lock: %v", err)

				return
			}

			if now := inRoom.Add(1); now > peak.Load() {
				peak.Store(now)
			}

			time.Sleep(time.Millisecond)
			inRoom.Add(-1)
			unlock()
			unlock() // повторный вызов безопасен
		}()
	}

	wg.Wait()

	if peak.Load() != 1 {
		t.Fatalf("в комнате одновременно %d записей", peak.Load())
	}

	unlockA, err := locks.lock(context.Background(), "!a")
	if err != nil {
		t.Fatal(err)
	}

	// Другая комната не ждет.
	unlockB, err := locks.lock(context.Background(), "!b")
	if err != nil {
		t.Fatal(err)
	}

	unlockB()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := locks.lock(ctx, "!a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock занятой комнаты с отменой: %v", err)
	}

	unlockA()

	locks.mu.Lock()
	left := len(locks.entries)
	locks.mu.Unlock()

	if left != 0 {
		t.Fatalf("после разблокировки осталось %d записей", left)
	}

	if time.Since(started) > 10*time.Second {
		t.Fatal("слишком долго")
	}
}
