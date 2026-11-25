package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeExec запоминает выполненные инструкции вместо БД.
type fakeExec struct {
	mu    sync.Mutex
	calls []fakeCall
	fail  error
}

type fakeCall struct {
	sql   string
	keys  []string
	times []time.Time
}

func (f *fakeExec) exec(_ context.Context, sql string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail != nil {
		return f.fail
	}

	call := fakeCall{sql: sql, times: slices.Clone(args[1].([]time.Time))}

	switch keys := args[0].(type) {
	case []string:
		call.keys = slices.Clone(keys)
	case [][]byte:
		for _, key := range keys {
			call.keys = append(call.keys, string(key))
		}
	}

	f.calls = append(f.calls, call)

	return nil
}

func (f *fakeExec) snapshot() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

// fakeClock - управляемые часы.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestActivityTouchDoesNotExecute - отметка в пути запроса не
// выполняет SQL; запись - только при сбросе.
func TestActivityTouchDoesNotExecute(t *testing.T) {
	fake := &fakeExec{}
	a := newActivity(fake.exec, ActivityOptions{})

	for range 100 {
		a.TouchToken([]byte("tok"))
		a.TouchMedia("mxc://a/b")
	}

	if got := len(fake.snapshot()); got != 0 {
		t.Fatalf("отметки выполнили %d инструкций, ожидали 0", got)
	}

	if st := a.Stats(); st.PendingTokens != 1 || st.PendingMedia != 1 {
		t.Fatalf("накоплено %+v, ожидали по одному ключу (повторы схлопываются)", st)
	}
}

// TestActivityFlushOneStatementPerQueue - сброс пишет одну инструкцию на очередь,
// ключи отсортированы, у ключа самая поздняя отметка.
func TestActivityFlushOneStatementPerQueue(t *testing.T) {
	fake := &fakeExec{}
	clock := &fakeClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	a := newActivity(fake.exec, ActivityOptions{Now: clock.Now})

	start := clock.Now()

	for _, key := range []string{"c", "a", "b"} {
		a.TouchToken([]byte(key))
		a.TouchMedia("mxc://s/" + key)
	}

	clock.advance(time.Second)
	a.TouchToken([]byte("a"))

	if err := a.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	calls := fake.snapshot()
	if len(calls) != 2 {
		t.Fatalf("инструкций %d, ожидали 2 (токены и медиа)", len(calls))
	}

	if calls[0].sql != flushTokensQuery || !slices.Equal(calls[0].keys, []string{"a", "b", "c"}) {
		t.Fatalf("токены: %+v", calls[0])
	}

	if !calls[0].times[0].Equal(start.Add(time.Second)) || !calls[0].times[1].Equal(start) {
		t.Fatalf("отметки токенов %v: у a ожидали позднюю", calls[0].times)
	}

	if calls[1].sql != flushMediaQuery || !slices.Equal(calls[1].keys, []string{"mxc://s/a", "mxc://s/b", "mxc://s/c"}) {
		t.Fatalf("медиа: %+v", calls[1])
	}

	if st := a.Stats(); st.Statements != 2 || st.PendingTokens != 0 || st.PendingMedia != 0 {
		t.Fatalf("после сброса %+v", st)
	}

	// Пустой сброс ничего не выполняет.
	if err := a.Flush(context.Background()); err != nil || len(fake.snapshot()) != 2 {
		t.Fatalf("пустой сброс: err=%v, инструкций %d", err, len(fake.snapshot()))
	}
}

// TestActivityFlushChunks - больше activityBatchSize ключей - несколько инструкций.
func TestActivityFlushChunks(t *testing.T) {
	fake := &fakeExec{}
	a := newActivity(fake.exec, ActivityOptions{})

	for i := range activityBatchSize + 1 {
		a.TouchMedia(fmt.Sprintf("mxc://s/%06d", i))
	}

	if err := a.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	calls := fake.snapshot()
	if len(calls) != 2 || len(calls[0].keys) != activityBatchSize || len(calls[1].keys) != 1 {
		t.Fatalf("пачки: %d инструкций", len(calls))
	}
}

// TestActivityFlushErrorRequeues - при ошибке незаписанное остается в очереди.
func TestActivityFlushErrorRequeues(t *testing.T) {
	fake := &fakeExec{fail: errors.New("db down")}
	a := newActivity(fake.exec, ActivityOptions{})

	a.TouchToken([]byte("tok"))
	a.TouchMedia("mxc://a/b")

	if err := a.Flush(context.Background()); err == nil {
		t.Fatal("ожидали ошибку сброса")
	}

	if st := a.Stats(); st.PendingTokens != 1 || st.PendingMedia != 1 {
		t.Fatalf("после ошибки %+v, ожидали возврат в очередь", st)
	}

	fake.mu.Lock()
	fake.fail = nil
	fake.mu.Unlock()

	if err := a.Flush(context.Background()); err != nil {
		t.Fatalf("повторный сброс: %v", err)
	}

	if got := len(fake.snapshot()); got != 2 {
		t.Fatalf("повтор записал %d инструкций, ожидали 2", got)
	}
}

// TestActivityMaxPending - сверх предела новые ключи отбрасываются, известные обновляются.
func TestActivityMaxPending(t *testing.T) {
	fake := &fakeExec{}
	a := newActivity(fake.exec, ActivityOptions{MaxPending: 2})

	a.TouchMedia("a")
	a.TouchMedia("b")
	a.TouchMedia("c")
	a.TouchMedia("a")

	if st := a.Stats(); st.PendingMedia != 2 || st.Dropped != 1 {
		t.Fatalf("%+v, ожидали 2 ключа и 1 отброшенный", st)
	}
}

// TestActivityBackgroundAndClose - фоновый сброс по таймеру и финальный при Close.
func TestActivityBackgroundAndClose(t *testing.T) {
	fake := &fakeExec{}
	a := newActivity(fake.exec, ActivityOptions{FlushInterval: 10 * time.Millisecond})
	a.Start()
	a.Start()

	a.TouchToken([]byte("tok"))

	deadline := time.Now().Add(5 * time.Second)
	for len(fake.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("фоновый сброс не выполнился")
		}

		time.Sleep(5 * time.Millisecond)
	}

	a.TouchMedia("mxc://a/b")

	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	calls := fake.snapshot()
	if last := calls[len(calls)-1]; last.sql != flushMediaQuery {
		t.Fatalf("Close не сбросил медиа: %+v", calls)
	}

	if st := a.Stats(); st.PendingTokens != 0 || st.PendingMedia != 0 {
		t.Fatalf("после Close %+v", st)
	}

	// Повторный Close безопасен.
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("повторный close: %v", err)
	}
}

// TestActivityCloseWithoutStart - Close без Start пишет накопленное и не зависает.
func TestActivityCloseWithoutStart(t *testing.T) {
	fake := &fakeExec{}
	a := newActivity(fake.exec, ActivityOptions{})

	a.TouchToken([]byte("tok"))

	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := len(fake.snapshot()); got != 1 {
		t.Fatalf("инструкций %d, ожидали 1", got)
	}
}

// TestActivityDefaults - нулевые параметры заменяются значениями по умолчанию.
func TestActivityDefaults(t *testing.T) {
	a := newActivity((&fakeExec{}).exec, ActivityOptions{})

	if a.TokenInterval() != DefaultTokenActivityInterval || a.opts.FlushInterval != DefaultActivityFlushInterval ||
		a.opts.MaxPending != defaultActivityMaxPending {
		t.Fatalf("умолчания: %+v", a.opts)
	}
}
