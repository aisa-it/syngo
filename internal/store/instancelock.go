package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Блокировка экземпляра (ADR-0009): поддерживается один процесс
// syngod на базу. Состояние /sync, UIA, лимитеры, очереди push и outbox живут в
// памяти процесса, второй экземпляр на той же базе теряет события и шлет дубли.
// Защита - сессионная advisory-блокировка PostgreSQL на отдельном соединении:
// она держится, пока жив процесс, и снимается сервером при обрыве соединения.

// Ключи advisory-блокировки (двухключевая форма, чтобы не пересечься с
// golang-migrate, который берет одноключевую): "SYNG" и номер блокировки.
const (
	instanceLockClass int32 = 0x53594e47
	instanceLockObj   int32 = 1
)

// instanceLockAppPrefix - префикс application_name соединения с блокировкой:
// по нему второй экземпляр показывает, кто держит базу.
const instanceLockAppPrefix = "syngod-instance "

// ErrInstanceLocked - базу уже обслуживает другой экземпляр syngod.
var ErrInstanceLocked = errors.New("store: database is already used by another syngod instance")

// InstanceLock - удерживаемая блокировка экземпляра.
type InstanceLock struct {
	cfg   *pgx.ConnConfig
	ident string

	mu   sync.Mutex
	conn *pgx.Conn
	held atomic.Bool
}

// NewInstanceIdent - метка экземпляра для application_name: хост, pid и
// случайный суффикс (pid в контейнере всегда 1).
func NewInstanceIdent() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	var nonce [12]byte

	_, _ = rand.Read(nonce[:])

	return fmt.Sprintf("host=%s pid=%d id=%s", host, os.Getpid(), hex.EncodeToString(nonce[:]))
}

// PostgreSQL хранит в application_name максимум 63 байта и экранирует непечатные
// и не-ASCII символы. Сохраняем читаемое начало и хеш полной метки, если имя меняется.
func instanceApplicationName(ident string) string {
	const maxBytes = 63

	name := instanceLockAppPrefix + ident
	readable := strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return '_'
		}

		return r
	}, name)
	if readable == name && len(name) <= maxBytes {
		return name
	}

	digest := sha256.Sum256([]byte(ident))
	suffix := "~" + hex.EncodeToString(digest[:12])

	return readable[:min(len(readable), maxBytes-len(suffix))] + suffix
}

// AcquireInstanceLock берет блокировку экземпляра. Если ее держит другой
// процесс, ждет до wait (перекрытие при рестарте: старый экземпляр еще
// завершается) и возвращает ErrInstanceLocked с описанием держателя.
func AcquireInstanceLock(ctx context.Context, dsn, ident string, wait time.Duration) (*InstanceLock, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: instance lock: parse dsn: %w", err)
	}

	appName := instanceApplicationName(ident)
	cfg.RuntimeParams["application_name"] = appName

	lock := &InstanceLock{cfg: cfg, ident: appName}
	deadline := time.Now().Add(wait)

	for {
		holder, err := lock.tryAcquire(ctx)
		if err != nil {
			return nil, err
		}

		if holder == "" {
			return lock, nil
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w (%s); запуск второго экземпляра на той же базе не поддерживается (ADR-0009)",
				ErrInstanceLocked, holder)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(500*time.Millisecond, time.Until(deadline)+time.Millisecond)):
		}
	}
}

// tryAcquire - одна попытка: "" - блокировка взята, иначе описание держателя.
func (l *InstanceLock) tryAcquire(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.conn == nil || l.conn.IsClosed() {
		conn, err := pgx.ConnectConfig(ctx, l.cfg)
		if err != nil {
			return "", fmt.Errorf("store: instance lock: connect: %w", err)
		}

		l.conn = conn
	}

	ok, err := l.tryLockLocked(ctx)
	if err != nil {
		return "", err
	}

	if ok {
		return "", nil
	}

	holder, ownPID, err := l.holderLocked(ctx)
	if err != nil {
		return "", err
	}

	// Держатель - наш же прежний backend (соединение оборвано только с нашей
	// стороны, сервер его еще не закрыл): завершаем его и пробуем снова.
	if ownPID != 0 {
		_, _ = l.conn.Exec(ctx, `SELECT pg_terminate_backend($1, 5000)`, ownPID)

		if ok, err := l.tryLockLocked(ctx); err != nil || ok {
			return "", err
		}
	}

	return holder, nil
}

// tryLockLocked - pg_try_advisory_lock на соединении блокировки.
func (l *InstanceLock) tryLockLocked(ctx context.Context) (bool, error) {
	var ok bool

	if err := l.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`,
		instanceLockClass, instanceLockObj).Scan(&ok); err != nil {
		l.closeConnLocked(ctx)

		return false, fmt.Errorf("store: instance lock: %w", err)
	}

	if ok {
		l.held.Store(true)
	}

	return ok, nil
}

// holderLocked описывает держателя и возвращает PID только для собственной метки.
func (l *InstanceLock) holderLocked(ctx context.Context) (holder string, ownPID int32, err error) {
	var (
		pid         int32
		app, client string
		started     time.Time
	)

	err = l.conn.QueryRow(ctx, `
		SELECT a.pid, coalesce(a.application_name, ''), coalesce(host(a.client_addr), 'local'), a.backend_start
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted
		  AND l.classid = $1::int::oid AND l.objid = $2::int::oid AND l.objsubid = 2
		  -- advisory-блокировка действует в пределах базы, а pg_locks показывает весь
		  -- кластер: без фильтра держателем считался syngod другой базы того же сервера.
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		LIMIT 1`, instanceLockClass, instanceLockObj).Scan(&pid, &app, &client, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		// Держатель успел отпустить блокировку между запросами.
		return "holder unknown", 0, nil
	}

	if err != nil {
		return "", 0, fmt.Errorf("store: instance lock holder: %w", err)
	}

	holder = fmt.Sprintf("application_name=%s client=%s backend_pid=%d since=%s",
		app, client, pid, started.UTC().Format(time.RFC3339))
	if app != l.ident {
		return holder, 0, nil
	}

	return holder, pid, nil
}

// ApplicationName возвращает метку соединения в pg_stat_activity.
func (l *InstanceLock) ApplicationName() string { return l.ident }

// Held сообщает, удерживается ли блокировка сейчас (readiness, метрика).
func (l *InstanceLock) Held() bool {
	if l == nil {
		return false
	}

	return l.held.Load()
}

// Check проверяет соединение с блокировкой и при обрыве берет ее заново.
// ErrInstanceLocked - за время обрыва базу занял другой экземпляр: процесс
// должен завершиться. Другие ошибки (база недоступна) - временные.
func (l *InstanceLock) Check(ctx context.Context) error {
	l.mu.Lock()

	if l.conn != nil && !l.conn.IsClosed() {
		err := l.conn.Ping(ctx)
		if err == nil {
			l.mu.Unlock()

			return nil
		}

		l.closeConnLocked(ctx)
	}

	l.mu.Unlock()

	holder, err := l.tryAcquire(ctx)
	if err != nil {
		return err
	}

	if holder != "" {
		return fmt.Errorf("%w (%s)", ErrInstanceLocked, holder)
	}

	return nil
}

// Watch проверяет блокировку каждые interval до отмены ctx. onLost
// вызывается один раз, если базу занял другой экземпляр.
func (l *InstanceLock) Watch(ctx context.Context, interval time.Duration, onError, onLost func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		checkCtx, cancel := context.WithTimeout(ctx, max(interval, time.Second))
		err := l.Check(checkCtx)

		cancel()

		switch {
		case err == nil:
		case errors.Is(err, ErrInstanceLocked):
			onLost(err)

			return
		case ctx.Err() == nil && onError != nil:
			onError(err)
		}
	}
}

// closeConnLocked закрывает соединение: блокировка считается потерянной.
func (l *InstanceLock) closeConnLocked(ctx context.Context) {
	l.held.Store(false)

	if l.conn != nil {
		// Закрываем и при отмененном ctx (обрыв, завершение процесса).
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		_ = l.conn.Close(closeCtx)

		cancel()
	}

	l.conn = nil
}

// Release снимает блокировку и закрывает соединение.
func (l *InstanceLock) Release(ctx context.Context) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.conn != nil && !l.conn.IsClosed() {
		_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, instanceLockClass, instanceLockObj)
	}

	l.closeConnLocked(ctx)
}
