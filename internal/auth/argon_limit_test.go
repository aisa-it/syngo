package auth

import (
	"sync"
	"testing"
)

// TestArgonConcurrencyLimited — 32 параллельные проверки пароля считаются не больше
// чем по maxConcurrentArgon одновременно: пик памяти argon2id - 4 x 64 МиБ, а не
// 32 x 64 МиБ (лимит контейнера 1 ГБ).
func TestArgonConcurrencyLimited(t *testing.T) {
	hasher := NewArgon2Hasher()

	hash, err := hasher.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	argonPeak.Store(0)

	var wg sync.WaitGroup

	for range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if ok, err := hasher.Verify(hash, "correct horse battery"); err != nil || !ok {
				t.Errorf("Verify: ok=%v err=%v", ok, err)
			}
		}()
	}

	wg.Wait()

	if peak := argonPeak.Load(); peak > maxConcurrentArgon || peak == 0 {
		t.Fatalf("пик одновременных argon2id = %d, ожидали 1..%d", peak, maxConcurrentArgon)
	}
}
