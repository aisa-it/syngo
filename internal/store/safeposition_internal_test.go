package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSafePositionConservativeOnError — при ошибке проверки EXISTS позиция
// должна оставаться консервативной (candidate−1), а не прыгать в maxPosition:
// иначе при сбое БД /sync теряет события.
func TestSafePositionConservativeOnError(t *testing.T) {
	t.Parallel()

	// Пул на заведомо недоступный порт: любой запрос вернёт ошибку, как при
	// недоступной БД.
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}

	t.Cleanup(pool.Close)

	db := &DB{pool: pool}
	db.registerPendingPosition(10)

	safe, err := db.safePosition(context.Background(), 20)
	if err != nil {
		t.Fatalf("safePosition: %v", err)
	}

	if safe != 9 {
		t.Fatalf("safe = %d, ожидали 9 (консервативно перед pending-номером)", safe)
	}
}
