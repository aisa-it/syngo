package api

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestKeyedLockSerializes — мьютекс на ключ не даёт конкурентного входа и
// удаляет запись после последнего ожидающего.
func TestKeyedLockSerializes(t *testing.T) {
	t.Parallel()

	var (
		k       keyedLock
		active  atomic.Int32
		maxSeen atomic.Int32
		wg      sync.WaitGroup
	)

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			unlock := k.Lock("user")
			defer unlock()

			cur := active.Add(1)
			if cur > maxSeen.Load() {
				maxSeen.Store(cur)
			}

			// имитация работы под замком
			for j := 0; j < 1000; j++ {
			}

			active.Add(-1)
		}()
	}

	wg.Wait()

	if maxSeen.Load() != 1 {
		t.Fatalf("внутри секции одновременно %d горутин, ожидали 1", maxSeen.Load())
	}

	if k.Len() != 0 {
		t.Fatalf("после освобождения осталось %d ключей", k.Len())
	}

	// Разные ключи не блокируют друг друга.
	unlockA := k.Lock("a")
	unlockB := k.Lock("b")
	unlockA()
	unlockB()

	if k.Len() != 0 {
		t.Fatalf("после разных ключей осталось %d", k.Len())
	}
}
