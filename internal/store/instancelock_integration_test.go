//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// TestInstanceLockSecondInstance - второй экземпляр на той же базе
// не стартует и в ошибке видит держателя; после остановки первого - стартует.
func TestInstanceLockSecondInstance(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	first, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=first", 0)
	if err != nil {
		t.Fatalf("первый экземпляр: %v", err)
	}

	t.Cleanup(func() { first.Release(ctx) })

	if !first.Held() {
		t.Fatal("первый экземпляр: Held() = false")
	}

	started := time.Now()

	_, err = store.AcquireInstanceLock(ctx, dsn, "host=b pid=1 id=second", 300*time.Millisecond)
	if !errors.Is(err, store.ErrInstanceLocked) {
		t.Fatalf("второй экземпляр: %v, ожидали ErrInstanceLocked", err)
	}

	if !strings.Contains(err.Error(), "host=a pid=1 id=first") {
		t.Fatalf("в ошибке нет держателя: %v", err)
	}

	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Fatalf("второй экземпляр не ждал освобождения: %v", waited)
	}

	// Рестарт: старый экземпляр завершается, пока новый ждет блокировку.
	go func() {
		time.Sleep(200 * time.Millisecond)
		first.Release(ctx)
	}()

	second, err := store.AcquireInstanceLock(ctx, dsn, "host=b pid=1 id=second", 5*time.Second)
	if err != nil {
		t.Fatalf("после остановки первого: %v", err)
	}

	second.Release(ctx)
}

// TestInstanceLockReacquireAfterDisconnect - обрыв соединения с
// блокировкой (рестарт PostgreSQL, сеть) - экземпляр берет ее заново; если за
// это время базу занял другой - Check сообщает ErrInstanceLocked.
func TestInstanceLockReacquireAfterDisconnect(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	lock, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=reconnect", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { lock.Release(ctx) })

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = admin.Close(ctx) })

	terminate := func() {
		t.Helper()

		if _, err := admin.Exec(ctx, `
			SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
			WHERE application_name = 'syngod-instance host=a pid=1 id=reconnect'`); err != nil {
			t.Fatal(err)
		}
	}

	terminate()

	if err := lock.Check(ctx); err != nil {
		t.Fatalf("переподключение: %v", err)
	}

	if !lock.Held() {
		t.Fatal("после переподключения Held() = false")
	}

	terminate()

	other, err := store.AcquireInstanceLock(ctx, dsn, "host=b pid=1 id=intruder", 0)
	if err != nil {
		t.Fatalf("второй экземпляр после обрыва первого: %v", err)
	}

	t.Cleanup(func() { other.Release(ctx) })

	if err := lock.Check(ctx); !errors.Is(err, store.ErrInstanceLocked) {
		t.Fatalf("база занята другим: %v, ожидали ErrInstanceLocked", err)
	}

	if lock.Held() {
		t.Fatal("блокировка потеряна, а Held() = true")
	}
}

// TestInstanceLockWatchLost - Watch сообщает о потере блокировки один раз.
func TestInstanceLockWatchLost(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lock, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=watch", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { lock.Release(context.Background()) })

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
		WHERE application_name = 'syngod-instance host=a pid=1 id=watch'`); err != nil {
		t.Fatal(err)
	}

	var ok bool
	if err := admin.QueryRow(ctx, `SELECT pg_try_advisory_lock(1398361671, 1)`).Scan(&ok); err != nil || !ok {
		t.Fatalf("захват блокировки чужим соединением: %v %v", ok, err)
	}

	lost := make(chan error, 2)

	go lock.Watch(ctx, 100*time.Millisecond, nil, func(err error) { lost <- err })

	select {
	case err := <-lost:
		if !errors.Is(err, store.ErrInstanceLocked) {
			t.Fatalf("onLost: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Watch не сообщил о потере блокировки")
	}
}

// TestInstanceLockStaleOwnBackend - соединение закрыто только на стороне
// клиента, а backend с блокировкой еще жив: экземпляр узнает свой backend по
// application_name, завершает его и берет блокировку, а не падает.
func TestInstanceLockStaleOwnBackend(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	stale, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=stale", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { stale.Release(ctx) })

	fresh, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=stale", 0)
	if err != nil {
		t.Fatalf("свой зависший backend: %v", err)
	}

	t.Cleanup(func() { fresh.Release(ctx) })

	if !fresh.Held() {
		t.Fatal("Held() = false")
	}
}

// TestInstanceLockHolderSameDatabaseOnly - advisory-блокировка действует в пределах
// базы, а pg_locks показывает весь кластер: держатель той же блокировки в другой
// базе того же сервера (например, syngod стенда) не должен считаться держателем
// этой базы.
func TestInstanceLockHolderSameDatabaseOnly(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()

	// «Чужой» экземпляр в другой базе кластера держит ту же блокировку раньше всех.
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Database = "postgres"
	cfg.RuntimeParams["application_name"] = "foreign-db-holder"

	foreign, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("нет доступа к базе postgres: %v", err)
	}

	t.Cleanup(func() { _ = foreign.Close(ctx) })

	if _, err := foreign.Exec(ctx, `SELECT pg_advisory_lock(1398361671, 1)`); err != nil {
		t.Fatalf("чужая блокировка: %v", err)
	}

	first, err := store.AcquireInstanceLock(ctx, dsn, "host=a pid=1 id=first-db", 0)
	if err != nil {
		t.Fatalf("блокировка в своей базе не должна зависеть от чужой: %v", err)
	}

	t.Cleanup(func() { first.Release(ctx) })

	for range 20 {
		_, err = store.AcquireInstanceLock(ctx, dsn, "host=b pid=1 id=second-db", 10*time.Millisecond)
		if !errors.Is(err, store.ErrInstanceLocked) {
			t.Fatalf("второй экземпляр: %v, ожидали ErrInstanceLocked", err)
		}

		if strings.Contains(err.Error(), "foreign-db-holder") || !strings.Contains(err.Error(), "id=first-db") {
			t.Fatalf("держатель взят из чужой базы: %v", err)
		}
	}
}
