package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Значения по умолчанию отложенной записи отметок активности.
const (
	// DefaultActivityFlushInterval - как часто накопленные отметки пишутся в БД
	// (в Synapse client_ips сбрасываются раз в 5 с, last_access_ts медиа - раз в 60 с).
	DefaultActivityFlushInterval = 10 * time.Second
	// DefaultTokenActivityInterval - не чаще какого интервала обновляется
	// access_tokens.last_validated_at одного токена.
	DefaultTokenActivityInterval = 5 * time.Minute
	// defaultActivityMaxPending - предел накопленных ключей одной очереди: при
	// недоступной БД память не растет без границы, лишние отметки отбрасываются.
	defaultActivityMaxPending = 100_000
	// activityBatchSize - сколько строк пишет один UPDATE; больше - несколько.
	activityBatchSize = 5_000
)

// Пакетные UPDATE: одна инструкция на очередь за интервал вместо коммита (и
// fsync WAL) на каждый запрос. Условие «<» - метка не откатывается назад, если
// строку уже обновили более свежим значением.
const (
	flushTokensQuery = `
		UPDATE access_tokens t SET last_validated_at = v.ts
		FROM unnest($1::bytea[], $2::timestamptz[]) AS v(token_hash, ts)
		WHERE t.token_hash = v.token_hash
		  AND (t.last_validated_at IS NULL OR t.last_validated_at < v.ts)`
	flushMediaQuery = `
		UPDATE media m SET last_accessed_at = v.ts
		FROM unnest($1::text[], $2::timestamptz[]) AS v(content_uri, ts)
		WHERE m.content_uri = v.content_uri
		  AND (m.last_accessed_at IS NULL OR m.last_accessed_at < v.ts)`
)

// ActivityOptions - параметры отложенной записи отметок активности.
type ActivityOptions struct {
	// FlushInterval - период фонового сброса; <= 0 - DefaultActivityFlushInterval.
	FlushInterval time.Duration
	// TokenInterval - не чаще какого интервала обновлять last_validated_at
	// токена; <= 0 - DefaultTokenActivityInterval.
	TokenInterval time.Duration
	// MaxPending - предел ключей одной очереди; <= 0 - значение по умолчанию.
	MaxPending int
	// OnError - журнал ошибок фонового сброса (nil - молча, повтор на следующем тике).
	OnError func(err error)
	// Now - часы (для тестов); nil - time.Now.
	Now func() time.Time
}

// ActivityStats - счетчики для тестов и диагностики.
type ActivityStats struct {
	// Statements - выполненные пакетные UPDATE.
	Statements int64
	// PendingTokens, PendingMedia - накоплено и еще не записано.
	PendingTokens, PendingMedia int
	// Dropped - отметки, отброшенные из-за предела очереди.
	Dropped int64
}

// execFunc выполняет одну SQL-инструкцию (pool.Exec или подмена в тестах).
type execFunc func(ctx context.Context, sql string, args ...any) error

// Activity копит отметки активности - access_tokens.last_validated_at и
// media.last_accessed_at - в памяти и пишет их пачкой в фоне.
// Раньше каждая проверка токена (каждый /sync) и каждое скачивание медиа
// делали отдельный UPDATE с коммитом и fsync WAL: под нагрузкой все
// соединения пула стояли в WALWrite/WalSync. Ориентир - Synapse:
// _update_client_ips_batch и сброс last_access_ts медиа раз в N секунд.
//
// Обе метки ничем не читаются, кроме самого прореживания (истечение токена -
// по expires_at, отзыв - по revoked_at), поэтому запаздывание на интервал
// сброса не меняет поведения.
type Activity struct {
	exec  execFunc
	opts  ActivityOptions
	now   func() time.Time
	token *touchQueue
	media *touchQueue

	statements atomic.Int64
	// flushMu - один сброс за раз (фоновый тик и финальный при остановке).
	flushMu sync.Mutex

	startOnce sync.Once
	closeOnce sync.Once
	stop      chan struct{}
	done      chan struct{}
	started   atomic.Bool
}

// EnableActivity включает отложенную запись отметок активности для хранилищ
// этой БД (AuthStore, MediaStore). Фоновый сброс запускается Start, финальный -
// Close. Без вызова хранилища пишут метки синхронно, как раньше (утилиты, тесты).
func (db *DB) EnableActivity(opts ActivityOptions) *Activity {
	a := newActivity(func(ctx context.Context, sql string, args ...any) error {
		_, err := db.pool.Exec(ctx, sql, args...)

		return err //nolint:wrapcheck // оборачивается в flushQueue
	}, opts)

	db.activity.Store(a)

	return a
}

// newActivity собирает накопитель поверх произвольного исполнителя SQL.
func newActivity(exec execFunc, opts ActivityOptions) *Activity {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultActivityFlushInterval
	}

	if opts.TokenInterval <= 0 {
		opts.TokenInterval = DefaultTokenActivityInterval
	}

	if opts.MaxPending <= 0 {
		opts.MaxPending = defaultActivityMaxPending
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Activity{
		exec:  exec,
		opts:  opts,
		now:   now,
		token: newTouchQueue(opts.MaxPending),
		media: newTouchQueue(opts.MaxPending),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// TokenInterval - интервал прореживания last_validated_at.
func (a *Activity) TokenInterval() time.Duration {
	return a.opts.TokenInterval
}

// TouchToken запоминает проверку токена; запись - при следующем сбросе.
func (a *Activity) TouchToken(tokenHash []byte) {
	a.token.add(string(tokenHash), a.now())
}

// TouchMedia запоминает скачивание медиа; запись - при следующем сбросе.
func (a *Activity) TouchMedia(contentURI string) {
	a.media.add(contentURI, a.now())
}

// Start запускает фоновый сброс раз в FlushInterval. Повторный вызов ничего не делает.
func (a *Activity) Start() {
	a.startOnce.Do(func() {
		a.started.Store(true)

		go a.run()
	})
}

// run - цикл фонового сброса до Close.
func (a *Activity) run() {
	defer close(a.done)

	ticker := time.NewTicker(a.opts.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), a.opts.FlushInterval)
			err := a.Flush(ctx)

			cancel()

			if err != nil && a.opts.OnError != nil {
				a.opts.OnError(err)
			}
		}
	}
}

// Close останавливает фоновый сброс и пишет накопленное (graceful shutdown).
// Повторный вызов только повторяет сброс.
func (a *Activity) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		close(a.stop)

		if a.started.Load() {
			<-a.done
		}
	})

	return a.Flush(ctx)
}

// Flush пишет накопленные отметки: по одному UPDATE ... FROM unnest на очередь
// (больше - только сверх activityBatchSize строк). При ошибке незаписанное
// возвращается в очередь и уйдет следующим сбросом.
func (a *Activity) Flush(ctx context.Context) error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	return errors.Join(
		a.flushQueue(ctx, a.token, flushTokensQuery, func(keys []string) any {
			out := make([][]byte, len(keys))
			for i, key := range keys {
				out[i] = []byte(key)
			}

			return out
		}, "access_tokens.last_validated_at"),
		a.flushQueue(ctx, a.media, flushMediaQuery, func(keys []string) any { return keys }, "media.last_accessed_at"),
	)
}

// flushQueue забирает очередь и пишет ее пачками.
func (a *Activity) flushQueue(ctx context.Context, q *touchQueue, query string,
	keyArg func([]string) any, what string,
) error {
	keys, times := q.take()

	for start := 0; start < len(keys); start += activityBatchSize {
		end := min(start+activityBatchSize, len(keys))

		if err := a.exec(ctx, query, keyArg(keys[start:end]), times[start:end]); err != nil {
			q.restore(keys[start:], times[start:])

			return fmt.Errorf("store: flush %s: %w", what, err)
		}

		a.statements.Add(1)
	}

	return nil
}

// Stats - счетчики накопителя.
func (a *Activity) Stats() ActivityStats {
	return ActivityStats{
		Statements:    a.statements.Load(),
		PendingTokens: a.token.len(),
		PendingMedia:  a.media.len(),
		Dropped:       a.token.dropped.Load() + a.media.dropped.Load(),
	}
}

// touchQueue - множество ключей с самой поздней отметкой времени.
type touchQueue struct {
	mu      sync.Mutex
	pending map[string]time.Time
	max     int
	dropped atomic.Int64
}

func newTouchQueue(maxPending int) *touchQueue {
	return &touchQueue{pending: make(map[string]time.Time), max: maxPending}
}

// add запоминает отметку; повтор ключа только сдвигает время вперед.
func (q *touchQueue) add(key string, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.mergeLocked(key, at)
}

// mergeLocked - add под уже взятой блокировкой.
func (q *touchQueue) mergeLocked(key string, at time.Time) {
	prev, ok := q.pending[key]
	if ok {
		if at.After(prev) {
			q.pending[key] = at
		}

		return
	}

	if len(q.pending) >= q.max {
		q.dropped.Add(1)

		return
	}

	q.pending[key] = at
}

// take забирает накопленное, отсортированное по ключу: строки блокируются в
// одном порядке, и пачки не взаимоблокируются между собой.
func (q *touchQueue) take() ([]string, []time.Time) {
	q.mu.Lock()
	pending := q.pending
	q.pending = make(map[string]time.Time, len(pending))
	q.mu.Unlock()

	if len(pending) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	times := make([]time.Time, len(keys))
	for i, key := range keys {
		times[i] = pending[key]
	}

	return keys, times
}

// restore возвращает незаписанное в очередь (с учетом новых отметок).
func (q *touchQueue) restore(keys []string, times []time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, key := range keys {
		q.mergeLocked(key, times[i])
	}
}

func (q *touchQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.pending)
}
