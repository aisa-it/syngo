package core_test

import (
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

func TestMemoryUIASessionsLifecycle(t *testing.T) {
	t.Parallel()

	sessions := core.NewMemoryUIASessions()

	session, err := sessions.Create([]byte(`{"username":"alice"}`))
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if session.ID == "" {
		t.Fatal("Create() вернул пустой идентификатор")
	}

	got, ok := sessions.Get(session.ID)
	if !ok {
		t.Fatal("Get() не нашёл созданную сессию")
	}

	if string(got.ClientData) != `{"username":"alice"}` {
		t.Fatalf("ClientData = %q", got.ClientData)
	}

	sessions.Delete(session.ID)

	if _, ok := sessions.Get(session.ID); ok {
		t.Fatal("Get() нашёл удалённую сессию")
	}
}

func TestMemoryUIASessionsExpire(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	sessions := core.NewMemoryUIASessions().WithClock(func() time.Time { return now })

	session, err := sessions.Create(nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if _, ok := sessions.Get(session.ID); !ok {
		t.Fatal("свежая сессия должна быть доступна")
	}

	now = now.Add(core.UIASessionTTL + time.Second)

	if _, ok := sessions.Get(session.ID); ok {
		t.Fatal("просроченная сессия должна исчезнуть")
	}
}

func TestMemoryUIASessionsUniqueIDs(t *testing.T) {
	t.Parallel()

	sessions := core.NewMemoryUIASessions()

	first, err := sessions.Create(nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	second, err := sessions.Create(nil)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	if first.ID == second.ID {
		t.Fatal("две сессии получили одинаковый идентификатор")
	}
}

// TestMemoryUIASessionsComplete - отметка стадии m.login.sso.
func TestMemoryUIASessionsComplete(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	sessions := core.NewMemoryUIASessions().WithClock(func() time.Time { return now })

	session, _ := sessions.Create(nil)

	if !sessions.Complete(session.ID, "m.login.sso") {
		t.Fatal("Complete() живой сессии")
	}

	if got, _ := sessions.Get(session.ID); len(got.Completed) != 1 || got.Completed[0] != "m.login.sso" {
		t.Fatalf("Completed = %v", got.Completed)
	}

	if sessions.Complete("unknown", "m.login.sso") {
		t.Fatal("Complete() неизвестной сессии")
	}

	now = now.Add(core.UIASessionTTL + time.Second)

	if sessions.Complete(session.ID, "m.login.sso") {
		t.Fatal("Complete() просроченной сессии")
	}
}
