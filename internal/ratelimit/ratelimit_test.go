package ratelimit_test

import (
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/ratelimit"
)

// fakeClock — управляемые часы для проверки окна восстановления.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *fakeClock                   { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestBurstThenReject(t *testing.T) {
	t.Parallel()

	clock := newClock()
	l := ratelimit.NewWithClock(ratelimit.Config{PerSecond: 0.5, Burst: 3}, clock.Now)

	for i := range 3 {
		if d := l.Allow("ip-1"); !d.Allowed {
			t.Fatalf("запрос %d должен пройти (burst=3), получили отказ", i+1)
		}
	}

	d := l.Allow("ip-1")
	if d.Allowed {
		t.Fatal("4-й запрос должен быть отклонён")
	}

	if d.RetryAfter <= 0 {
		t.Fatalf("retry_after должен быть > 0, получили %s", d.RetryAfter)
	}

	// 0.5 запроса/с: на освобождение одного места нужно 2 с.
	if d.RetryAfter != 2*time.Second {
		t.Fatalf("retry_after = %s, ожидали 2s", d.RetryAfter)
	}
}

func TestWindowRestores(t *testing.T) {
	t.Parallel()

	clock := newClock()
	l := ratelimit.NewWithClock(ratelimit.Config{PerSecond: 1, Burst: 2}, clock.Now)

	for i := range 2 {
		if d := l.Allow("ip"); !d.Allowed {
			t.Fatalf("запрос %d должен пройти (burst=2)", i+1)
		}
	}

	if l.Allow("ip").Allowed {
		t.Fatal("третий запрос должен быть отклонён")
	}

	clock.Advance(time.Second) // протекло 1 запрос/с → одно место свободно

	if d := l.Allow("ip"); !d.Allowed {
		t.Fatalf("после окна запрос должен пройти, отказ с retry %s", d.RetryAfter)
	}

	if l.Allow("ip").Allowed {
		t.Fatal("после восстановления место снова занято — следующий запрос должен быть отклонён")
	}
}

func TestKeysIndependent(t *testing.T) {
	t.Parallel()

	clock := newClock()
	l := ratelimit.NewWithClock(ratelimit.Config{PerSecond: 0.01, Burst: 1}, clock.Now)

	if !l.Allow("ip-1").Allowed {
		t.Fatal("ip-1: первый запрос должен пройти")
	}

	if l.Allow("ip-1").Allowed {
		t.Fatal("ip-1: второй запрос должен быть отклонён")
	}

	if !l.Allow("ip-2").Allowed {
		t.Fatal("ip-2 не должен зависеть от лимита ip-1")
	}

	if l.Size() != 2 {
		t.Fatalf("ожидали 2 ведра, получили %d", l.Size())
	}
}

func TestDisabledConfigAllowsAll(t *testing.T) {
	t.Parallel()

	for _, cfg := range []ratelimit.Config{
		{},
		{PerSecond: 0, Burst: 5},
		{PerSecond: 1, Burst: 0},
	} {
		l := ratelimit.New(cfg)
		for range 100 {
			if !l.Allow("any").Allowed {
				t.Fatalf("выключенный лимит %+v не должен отклонять", cfg)
			}
		}
	}
}

func TestFloatingRate(t *testing.T) {
	t.Parallel()

	clock := newClock()
	// 0.003/с и burst 5 — дефолт Synapse для login по адресу.
	l := ratelimit.NewWithClock(ratelimit.Config{PerSecond: 0.003, Burst: 5}, clock.Now)

	for range 5 {
		if !l.Allow("ip").Allowed {
			t.Fatal("первые 5 логинов должны пройти")
		}
	}

	d := l.Allow("ip")
	if d.Allowed {
		t.Fatal("6-й логин должен быть отклонён")
	}

	// На один запрос при 0.003/с нужно ~334 с.
	if d.RetryAfter < 300*time.Second || d.RetryAfter > 400*time.Second {
		t.Fatalf("retry_after = %s, ожидали ~334s", d.RetryAfter)
	}
}
