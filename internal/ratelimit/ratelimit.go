// Package ratelimit реализует лимитер в стиле Synapse: «leaky bucket as meter».
//
// Модель (как в synapse/api/ratelimiting.py): для каждого ключа есть ведро
// накопленных «израсходованных» запросов. Ведро «протекает» со скоростью
// PerSecond, а запрос добавляет единицу; если после добавления единицы объём
// превышает Burst — запрос отклоняется и клиенту сообщается, через сколько
// миллисекунд повторить.
//
// Хранилище — память процесса; интерфейс совместим с будущим Redis-хранилищем
// (Allow получает только ключ и время).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Config — параметры одного лимита.
type Config struct {
	// PerSecond — скорость «протечки» ведра (устойчивая частота запросов).
	PerSecond float64 `yaml:"per_second"`
	// Burst — ёмкость ведра: сколько запросов можно сделать залпом.
	Burst int `yaml:"burst"`
}

// Enabled сообщает, что лимит активен: нулевая скорость или нулевой burst означают
// «выключено» (так проще конфигурировать: не задал — не ограничиваем).
func (c Config) Enabled() bool {
	return c.PerSecond > 0 && c.Burst > 0
}

// Decision — результат проверки.
type Decision struct {
	// Allowed — запрос можно выполнять.
	Allowed bool
	// RetryAfter — сколько ждать до следующей попытки (только при отказе).
	RetryAfter time.Duration
}

// sweepEvery — как часто (в операциях) вычищать просроченные ведра.
const sweepEvery = 1024

// Limiter — потокобезопасный лимитер по произвольным ключам.
type Limiter struct {
	cfg Config

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	ops     int
}

type bucket struct {
	used float64
	last time.Time
}

// New создаёт лимитер с системными часами.
func New(cfg Config) *Limiter {
	return NewWithClock(cfg, time.Now)
}

// NewWithClock создаёт лимитер с подменяемыми часами (для тестов).
func NewWithClock(cfg Config, now func() time.Time) *Limiter {
	return &Limiter{
		cfg:     cfg,
		buckets: make(map[string]*bucket),
		now:     now,
	}
}

// Config возвращает конфигурацию лимитера.
func (l *Limiter) Config() Config {
	return l.cfg
}

// Peek проверяет состояние ведра, не расходуя его.
//
// Нужен для лимитов, которые считают не все запросы, а только неудачи
// (rc_login.account в Synapse): перед попыткой логина мы смотрим, не заблокирован
// ли аккаунт, а расходуем ведро только при неверных учётных данных.
func (l *Limiter) Peek(key string) Decision {
	if !l.cfg.Enabled() {
		return Decision{Allowed: true}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket := l.leakLocked(key, now)

	// Peek отвечает на вопрос «пройдёт ли следующий запрос», поэтому проверяем
	// used+1 — так же, как это делает Allow.
	if bucket.used+1 > float64(l.cfg.Burst) {
		return Decision{Allowed: false, RetryAfter: l.retryAfterLocked(bucket, 1)}
	}

	return Decision{Allowed: true}
}

// Record расходует одну единицу ведра и сообщает, уложились ли в лимит.
// Используется для учёта неудачных попыток (login по аккаунту).
func (l *Limiter) Record(key string) Decision {
	return l.Allow(key)
}

// Allow проверяет и учитывает запрос по ключу.
//
// Если лимит выключен, всегда разрешает. Пустой ключ трактуется как общий ключ "".
func (l *Limiter) Allow(key string) Decision {
	if !l.cfg.Enabled() {
		return Decision{Allowed: true}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	// Периодическая уборка, чтобы карта не росла бесконечно (боты с рандомными IP).
	l.ops++
	if l.ops%sweepEvery == 0 {
		l.sweepLocked(now)
	}

	b := l.leakLocked(key, now)

	if b.used+1 > float64(l.cfg.Burst) {
		return Decision{Allowed: false, RetryAfter: l.retryAfterLocked(b, 1)}
	}

	b.used++

	return Decision{Allowed: true}
}

// leakLocked возвращает ведро ключа, применив протечку до момента now.
func (l *Limiter) leakLocked(key string, now time.Time) *bucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{last: now}
		l.buckets[key] = b

		return b
	}

	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.used = math.Max(0, b.used-elapsed*l.cfg.PerSecond)
		b.last = now
	}

	return b
}

// retryAfterLocked оценивает, через сколько ведро освободит место для extra единиц.
func (l *Limiter) retryAfterLocked(b *bucket, extra float64) time.Duration {
	missing := b.used + extra - float64(l.cfg.Burst)
	if missing <= 0 {
		return 0
	}

	wait := time.Duration(math.Ceil(missing/l.cfg.PerSecond*1000)) * time.Millisecond
	if wait < time.Millisecond {
		wait = time.Millisecond
	}

	return wait
}

// sweepLocked удаляет ведра, которые полностью «протекли». Вызывается под mu.
func (l *Limiter) sweepLocked(now time.Time) {
	if l.cfg.PerSecond <= 0 {
		return
	}

	// Ведро пусто, если с последнего обращения прошло больше времени, чем нужно
	// на протечку burst запросов. Даём запас ×2.
	idle := time.Duration(float64(l.cfg.Burst) / l.cfg.PerSecond * 2 * float64(time.Second))

	for key, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, key)
		}
	}
}

// Size возвращает число отслеживаемых ключей (для метрик и тестов).
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.buckets)
}
