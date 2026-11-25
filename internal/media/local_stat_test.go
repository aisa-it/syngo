package media_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/media"
)

// TestLocalStoreStat - размер объекта без чтения (повторный импорт медиа).
func TestLocalStoreStat(t *testing.T) {
	t.Parallel()

	store, err := media.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	ctx := context.Background()

	if _, err := store.Put(ctx, "local_content/ab/cd/efgh", bytes.NewReader([]byte("12345"))); err != nil {
		t.Fatalf("put: %v", err)
	}

	if size, err := store.Stat(ctx, "local_content/ab/cd/efgh"); err != nil || size != 5 {
		t.Fatalf("stat = %d, %v", size, err)
	}

	if _, err := store.Stat(ctx, "local_content/ab/cd/none"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("нет объекта: %v", err)
	}

	if _, err := store.Stat(ctx, "../escape"); err == nil {
		t.Fatal("ключ за пределами хранилища принят")
	}
}
