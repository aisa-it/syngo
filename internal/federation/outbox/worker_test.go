package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/outbox"
	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/store"
)

// fakeItem — элемент очереди в памяти с состоянием ретраев.
type fakeItem struct {
	store.OutboxItem
	txnID       string
	nextAttempt time.Time
	isolate     bool
	leased      bool
}

// fakeStore — очередь в памяти с семантикой store.DB: пачка с txn_id
// повторяется целиком, isolate уходит по одному, аренда не дает взять
// destination дважды.
type fakeStore struct {
	mu      sync.Mutex
	items   []*fakeItem
	nextID  int64
	acked   []int64
	failed  []int64
	dead    map[int64]string
	retryAt time.Time
}

func newFakeStore() *fakeStore { return &fakeStore{dead: map[int64]string{}} }

func (f *fakeStore) add(destination string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	f.items = append(f.items, &fakeItem{OutboxItem: store.OutboxItem{
		ID: f.nextID, Destination: destination,
		PDU: []byte(`{"event_id":"$e` + strconv.FormatInt(f.nextID, 10) + `"}`),
	}})

	return f.nextID
}

func (f *fakeStore) left(destination string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, item := range f.items {
		if item.Destination == destination {
			n++
		}
	}

	return n
}

func (f *fakeStore) OutboxDestinations(_ context.Context, now time.Time, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	seen := map[string]bool{}

	var out []string

	for _, item := range f.items {
		if !item.nextAttempt.After(now) && !seen[item.Destination] {
			seen[item.Destination] = true
			out = append(out, item.Destination)
		}
	}

	return out, nil
}

func (f *fakeStore) ClaimOutboxBatch(_ context.Context, claim store.OutboxClaim) (store.OutboxBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var mine []*fakeItem

	for _, item := range f.items {
		if item.Destination == claim.Destination {
			if item.leased {
				return store.OutboxBatch{}, nil
			}

			mine = append(mine, item)
		}
	}

	// Повтор пачки с txn_id.
	for _, item := range mine {
		if item.txnID == "" {
			continue
		}

		if item.nextAttempt.After(claim.Now) {
			return store.OutboxBatch{}, nil
		}

		batch := store.OutboxBatch{TxnID: item.txnID, Retry: true}

		for _, other := range mine {
			if other.txnID == item.txnID {
				other.leased = true
				batch.Items = append(batch.Items, other.OutboxItem)
			}
		}

		return batch, nil
	}

	batch := store.OutboxBatch{TxnID: claim.NewTxnID}

	for _, item := range mine {
		if item.nextAttempt.After(claim.Now) || len(batch.Items) >= claim.BatchSize {
			continue
		}

		if item.isolate && len(batch.Items) > 0 {
			continue
		}

		item.txnID, item.leased = claim.NewTxnID, true
		batch.Items = append(batch.Items, item.OutboxItem)

		if item.isolate {
			break
		}
	}

	if len(batch.Items) == 0 {
		return store.OutboxBatch{}, nil
	}

	return batch, nil
}

func (f *fakeStore) remove(ids []int64) {
	drop := map[int64]bool{}
	for _, id := range ids {
		drop[id] = true
	}

	keep := f.items[:0]

	for _, item := range f.items {
		if !drop[item.ID] {
			keep = append(keep, item)
		}
	}

	f.items = keep
}

func (f *fakeStore) AckOutbox(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.acked = append(f.acked, ids...)
	f.remove(ids)

	return nil
}

func (f *fakeStore) FailOutbox(_ context.Context, failure store.OutboxFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failed = append(f.failed, failure.IDs...)
	f.retryAt = failure.RetryAt

	for _, item := range f.items {
		for _, id := range failure.IDs {
			if item.ID != id {
				continue
			}

			item.Attempts++
			item.nextAttempt = failure.RetryAt
			item.leased = false
			item.isolate = item.isolate || failure.Isolate

			if !failure.KeepTxn {
				item.txnID = ""
			}
		}
	}

	return nil
}

func (f *fakeStore) ReleaseOutbox(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, item := range f.items {
		for _, id := range ids {
			if item.ID == id {
				item.leased = false
			}
		}
	}

	return nil
}

func (f *fakeStore) DeadLetterOutbox(_ context.Context, dead []store.OutboxDead) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]int64, 0, len(dead))

	for _, item := range dead {
		f.dead[item.ID] = item.Reason
		ids = append(ids, item.ID)
	}

	f.remove(ids)

	return nil
}

func (f *fakeStore) OutboxMetrics(context.Context) (store.OutboxStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return store.OutboxStats{Depth: int64(len(f.items)), DeadLetters: int64(len(f.dead))}, nil
}

// fakeSender — отправка с заданным результатом.
type fakeSender struct {
	mu    sync.Mutex
	calls []string
	paths []string
	err   error
	code  int
	body  []byte
}

func (f *fakeSender) Send(_ context.Context, destination, _, path string, _ []byte) (body []byte, status int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, destination)
	f.paths = append(f.paths, path)

	return f.body, f.code, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// clock — управляемое время теста.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_700_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// TestWorkerDeliveryAndFailure — успех удаляет из очереди, ошибка инкрементит attempts и откладывает.
func TestWorkerDeliveryAndFailure(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return now }

	// Успешная отправка.
	okStore := newFakeStore()
	okStore.add("good.test")

	okSender := &fakeSender{code: 200}

	worker := outbox.New(okStore, okSender, outbox.Options{
		Origin: "ours.test", Now: nowFn, MinBackoff: time.Second, MaxBackoff: time.Minute, Logger: quiet(),
	})

	if _, err := worker.Tick(context.Background()); err != nil {
		t.Fatalf("tick ok: %v", err)
	}

	if len(okStore.acked) != 1 {
		t.Fatalf("успешная отправка: acked=%d, ожидали 1", len(okStore.acked))
	}

	// Ошибка: 500 → attempts++, next_attempt сдвинут на backoff.
	badStore := newFakeStore()
	badStore.add("bad.test")

	badWorker := outbox.New(badStore, &fakeSender{code: 500}, outbox.Options{
		Origin: "ours.test", Now: nowFn, MinBackoff: time.Second, MaxBackoff: time.Minute, Logger: quiet(),
	})

	_, _ = badWorker.Tick(context.Background())

	if len(badStore.failed) != 1 {
		t.Fatalf("неудачная отправка: failed=%d, ожидали 1", len(badStore.failed))
	}

	if badStore.retryAt.Before(now.Add(time.Second)) {
		t.Fatalf("next_attempt_at = %s, ожидали ≥ now+1s", badStore.retryAt)
	}

	// Сетевая ошибка тоже откладывает.
	netStore := newFakeStore()
	netStore.add("net.test")

	netWorker := outbox.New(netStore, &fakeSender{err: errors.New("dial timeout")}, outbox.Options{
		Origin: "ours.test", Now: nowFn, MinBackoff: time.Second, MaxBackoff: time.Minute, Logger: quiet(),
	})

	_, _ = netWorker.Tick(context.Background())

	if len(netStore.failed) != 1 {
		t.Fatalf("сетевая ошибка: failed=%d", len(netStore.failed))
	}
}

// TestWorkerTwoDestinationsIndependent — сбой одного destination не мешает другому.
func TestWorkerTwoDestinationsIndependent(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("a.test")
	st.add("b.test")

	sender := &fakeSender{code: 500}

	worker := outbox.New(st, sender, outbox.Options{Origin: "ours.test", Now: time.Now, Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	if len(sender.calls) != 2 {
		t.Fatalf("отправок %d, ожидали 2 (оба destination)", len(sender.calls))
	}

	if len(st.failed) != 2 {
		t.Fatalf("failed=%d, ожидали 2", len(st.failed))
	}
}

// TestDrain — при SIGTERM очередь доставляется до пустой, ничего не теряется.
func TestDrain(t *testing.T) {
	t.Parallel()

	st := newFakeStore()

	for i := 0; i < 3; i++ {
		st.add("b.test")
	}

	sender := &fakeSender{code: 200}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Logger: quiet()})

	stats, err := worker.Drain(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if stats.Depth != 0 {
		t.Fatalf("после дренажа depth = %d", stats.Depth)
	}

	if left := st.left("b.test"); left != 0 {
		t.Fatalf("в очереди осталось %d элементов", left)
	}
}

// TestDrainSeveralBatches — Drain доставляет очередь из нескольких пачек
// (раньше делался один
// Tick и при BatchSize=1 из трех элементов уходил один).
func TestDrainSeveralBatches(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	for i := 0; i < 3; i++ {
		st.add("b.test")
	}

	sender := &fakeSender{code: 200}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", BatchSize: 1, Logger: quiet()})

	stats, err := worker.Drain(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if stats.Depth != 0 || len(sender.calls) != 3 {
		t.Fatalf("после Drain depth=%d, отправок %d; ожидали 0 и 3", stats.Depth, len(sender.calls))
	}
}

// TestDrainStopsWhenNothingDelivered — недоступный получатель не держит Drain:
// проход без доставки завершает дренаж, элементы остаются в очереди.
func TestDrainStopsWhenNothingDelivered(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("down.test")

	worker := outbox.New(st, &fakeSender{err: errors.New("connection refused")},
		outbox.Options{Origin: "a.test", Logger: quiet()})

	stats, err := worker.Drain(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if stats.Depth != 1 {
		t.Fatalf("depth = %d, ожидали 1 (элемент ждет следующего старта)", stats.Depth)
	}
}

// TestRetryKeepsTxnID — повтор пачки после 5xx/сети идет с тем же txn_id
// (получатель мог
// принять транзакцию и дедуплицирует ее по txn_id).
func TestRetryKeepsTxnID(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("b.test")
	st.add("b.test")

	clk := newClock()
	sender := &fakeSender{code: 500}
	worker := outbox.New(st, sender, outbox.Options{
		Origin: "a.test:8448", Now: clk.Now, MinBackoff: time.Second, Logger: quiet(),
	})

	_, _ = worker.Tick(context.Background())

	// Новый элемент не должен попасть в повтор.
	st.add("b.test")
	clk.Advance(time.Minute)

	sender.err = errors.New("read: connection reset")
	sender.code = 0
	_, _ = worker.Tick(context.Background())

	if len(sender.paths) != 2 || sender.paths[0] != sender.paths[1] {
		t.Fatalf("пути %v: ожидали два одинаковых txn_id", sender.paths)
	}

	// Доставка повторной пачки, затем новая пачка с новым txn_id.
	clk.Advance(time.Hour)

	sender.err, sender.code = nil, 200
	_, _ = worker.Tick(context.Background())
	_, _ = worker.Tick(context.Background())

	if len(sender.paths) != 4 || sender.paths[3] == sender.paths[0] {
		t.Fatalf("пути %v: новая пачка должна получить новый txn_id", sender.paths)
	}

	if st.left("b.test") != 0 {
		t.Fatalf("в очереди осталось %d", st.left("b.test"))
	}
}

// TestRejectedTxnGetsNewTxnID — после 4xx получатель транзакцию не принял:
// txn_id снимается, повтор идет новой транзакцией.
func TestRejectedTxnGetsNewTxnID(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("b.test")

	clk := newClock()
	sender := &fakeSender{code: 400, err: errors.New("HTTP 400")}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Now: clk.Now, Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	clk.Advance(time.Hour)

	_, _ = worker.Tick(context.Background())

	if len(sender.paths) != 2 || sender.paths[0] == sender.paths[1] {
		t.Fatalf("пути %v: после 4xx ожидали новый txn_id", sender.paths)
	}
}

// TestDeadLetterAfterMaxAttempts — после MaxAttempts неудач элемент уходит в
// dead-letter и не блокирует очередь (предела попыток не было).
func TestDeadLetterAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	id := st.add("b.test")

	clk := newClock()
	sender := &fakeSender{code: 503, err: errors.New("HTTP 503")}
	worker := outbox.New(st, sender, outbox.Options{
		Origin: "a.test", Now: clk.Now, MaxAttempts: 3, MinBackoff: time.Second, MaxBackoff: time.Second,
		Logger: quiet(),
	})

	for i := 0; i < 5; i++ {
		_, _ = worker.Tick(context.Background())
		clk.Advance(time.Minute)
	}

	if len(sender.calls) != 3 {
		t.Fatalf("отправок %d, ожидали 3 (MaxAttempts)", len(sender.calls))
	}

	if reason, ok := st.dead[id]; !ok || !strings.Contains(reason, "3 attempts") {
		t.Fatalf("dead-letter = %v, ожидали элемент %d с причиной", st.dead, id)
	}

	if st.left("b.test") != 0 {
		t.Fatal("элемент остался в очереди")
	}
}

// TestExpiredItemDeadLettered — элемент старше MaxAge не отправляется.
func TestExpiredItemDeadLettered(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	id := st.add("b.test")

	clk := newClock()
	st.items[0].QueuedAt = clk.Now().Add(-48 * time.Hour)

	sender := &fakeSender{code: 200}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Now: clk.Now, MaxAge: 24 * time.Hour, Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	if len(sender.calls) != 0 {
		t.Fatalf("просроченный элемент отправлен: %v", sender.calls)
	}

	if _, ok := st.dead[id]; !ok {
		t.Fatalf("dead-letter = %v", st.dead)
	}
}

// TestPDUErrorsFromResponse — PDU с ошибкой в ответе /send уходит в
// dead-letter с причиной, остальные подтверждаются (раньше 200 подтверждался
// без разбора pdus).
func TestPDUErrorsFromResponse(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	okID := st.add("b.test")
	badID := st.add("b.test")

	body, _ := json.Marshal(map[string]any{"pdus": map[string]any{
		"$e" + strconv.FormatInt(okID, 10):  map[string]any{},
		"$e" + strconv.FormatInt(badID, 10): map[string]any{"error": "Event failed auth checks"},
	}})

	worker := outbox.New(st, &fakeSender{code: 200, body: body}, outbox.Options{Origin: "a.test", Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	if len(st.acked) != 1 || st.acked[0] != okID {
		t.Fatalf("acked = %v, ожидали [%d]", st.acked, okID)
	}

	if reason := st.dead[badID]; !strings.Contains(reason, "failed auth checks") {
		t.Fatalf("dead-letter = %v, ожидали %d с причиной получателя", st.dead, badID)
	}
}

// poisonSender отвергает 400 любую транзакцию с ядовитым PDU.
type poisonSender struct {
	mu     sync.Mutex
	poison string
	sent   map[string]int
}

func (p *poisonSender) Send(_ context.Context, _, _, _ string, body []byte) (resp []byte, status int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if strings.Contains(string(body), p.poison) {
		return nil, 400, errors.New("HTTP 400: M_BAD_JSON")
	}

	var txn struct {
		PDUs []struct {
			EventID string `json:"event_id"`
		} `json:"pdus"`
	}

	_ = json.Unmarshal(body, &txn)

	for _, pdu := range txn.PDUs {
		p.sent[pdu.EventID]++
	}

	return []byte(`{"pdus":{}}`), 200, nil
}

// TestPoisonItemDoesNotBlockDestination — ядовитый элемент в начале очереди
// не блокирует получателя: отвергнутая пачка разбирается по одному, остальные
// доставляются, ядовитый уходит в dead-letter.
func TestPoisonItemDoesNotBlockDestination(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	poisonID := st.add("b.test")

	for i := 0; i < 4; i++ {
		st.add("b.test")
	}

	clk := newClock()
	sender := &poisonSender{poison: fmt.Sprintf(`"$e%d"`, poisonID), sent: map[string]int{}}
	worker := outbox.New(st, sender, outbox.Options{
		Origin: "a.test", Now: clk.Now, MaxRejects: 2, MinBackoff: time.Second, MaxBackoff: time.Second,
		Logger: quiet(),
	})

	for i := 0; i < 10; i++ {
		_, _ = worker.Tick(context.Background())
		clk.Advance(time.Minute)
	}

	delivered := make([]string, 0, len(sender.sent))
	for id := range sender.sent {
		delivered = append(delivered, id)
	}

	sort.Strings(delivered)

	if len(delivered) != 4 {
		t.Fatalf("доставлено %v, ожидали 4 здоровых элемента", delivered)
	}

	if _, ok := st.dead[poisonID]; !ok {
		t.Fatalf("ядовитый элемент не в dead-letter: %v", st.dead)
	}

	if st.left("b.test") != 0 {
		t.Fatalf("в очереди осталось %d", st.left("b.test"))
	}
}

// TestPolicyDeniedDeadLettered — destination вне allow_hosts не ретраится вечно.
func TestPolicyDeniedDeadLettered(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	id := st.add("evil.test")

	sender := &fakeSender{err: fmt.Errorf("%w: evil.test is not in allow_hosts", transport.ErrDeniedByPolicy)}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	if _, ok := st.dead[id]; !ok {
		t.Fatalf("dead-letter = %v", st.dead)
	}
}

// TestBackoffDoesNotCountAttempt — получатель в backoff транспорта: попытки не
// было, attempts не растет, пачка сохраняет txn_id.
func TestBackoffDoesNotCountAttempt(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("b.test")

	sender := &fakeSender{err: fmt.Errorf("%w: b.test", transport.ErrBackoff)}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Logger: quiet()})

	_, _ = worker.Tick(context.Background())

	if len(st.failed) != 0 || st.items[0].Attempts != 0 || st.items[0].leased {
		t.Fatalf("backoff: failed=%v attempts=%d leased=%v", st.failed, st.items[0].Attempts, st.items[0].leased)
	}
}

// slowSender держит отправку на slow.test до закрытия release.
type slowSender struct {
	release chan struct{}
	mu      sync.Mutex
	done    []string
}

func (s *slowSender) Send(ctx context.Context, destination, _, _ string, _ []byte) (resp []byte, status int, err error) {
	if destination == "slow.test" {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}

	s.mu.Lock()
	s.done = append(s.done, destination)
	s.mu.Unlock()

	return nil, 200, nil
}

func (s *slowSender) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.done...)
}

// TestSlowDestinationDoesNotStarveOthers — медленный получатель не задерживает
// остальных: отправка по destination параллельна (раньше последовательно, с
// таймаутом 30 с на каждого).
func TestSlowDestinationDoesNotStarveOthers(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("slow.test")
	st.add("fast1.test")
	st.add("fast2.test")

	sender := &slowSender{release: make(chan struct{})}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Concurrency: 4, Logger: quiet()})

	done := make(chan struct{})

	go func() {
		_, _ = worker.Tick(context.Background())

		close(done)
	}()

	deadline := time.After(5 * time.Second)

	for len(sender.delivered()) < 2 {
		select {
		case <-deadline:
			close(sender.release)
			t.Fatalf("быстрые получатели ждут медленного: доставлено %v", sender.delivered())
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(sender.release)
	<-done

	if got := sender.delivered(); len(got) != 3 || got[2] != "slow.test" {
		t.Fatalf("порядок доставки %v, ожидали медленного последним", got)
	}
}

// TestBackgroundRunDrained — отправка из Run не обрывается остановкой Run,
// Drain дожидается ее (graceful shutdown).
func TestBackgroundRunDrained(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("slow.test")

	sender := &slowSender{release: make(chan struct{})}
	worker := outbox.New(st, sender, outbox.Options{Origin: "a.test", Logger: quiet()})

	runCtx, stop := context.WithCancel(context.Background())
	go worker.Run(runCtx, time.Millisecond)

	// Ждем, пока Run возьмет пачку (элемент в аренде).
	deadline := time.After(5 * time.Second)

	for {
		st.mu.Lock()
		leased := len(st.items) == 1 && st.items[0].leased
		st.mu.Unlock()

		if leased {
			break
		}

		select {
		case <-deadline:
			t.Fatal("Run не взял пачку")
		case <-time.After(time.Millisecond):
		}
	}

	stop()

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(sender.release)
	}()

	stats, err := worker.Drain(context.Background(), time.Millisecond)
	if err != nil || stats.Depth != 0 {
		t.Fatalf("Drain: depth=%d err=%v; отправка Run должна была завершиться", stats.Depth, err)
	}
}

// TestEventItemsBuiltBySource — элементы-ссылки на событие собираются PDUSource
// при отправке; событие, которого нет, уходит в dead-letter.
func TestEventItemsBuiltBySource(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.items = append(st.items,
		&fakeItem{OutboxItem: store.OutboxItem{ID: 10, Destination: "b.test", EventID: "$live"}},
		&fakeItem{OutboxItem: store.OutboxItem{ID: 11, Destination: "b.test", EventID: "$gone"}})

	var body []byte

	sender := senderFunc(func(_ context.Context, _, _, _ string, b []byte) ([]byte, int, error) {
		body = b

		return nil, 200, nil
	})

	worker := outbox.New(st, sender, outbox.Options{
		Origin: "a.test", Logger: quiet(),
		PDUs: func(_ context.Context, ids []string) (map[string]json.RawMessage, error) {
			out := map[string]json.RawMessage{}

			for _, id := range ids {
				if id == "$live" {
					out[id] = json.RawMessage(`{"type":"m.room.message","signed":true}`)
				}
			}

			return out, nil
		},
	})

	_, _ = worker.Tick(context.Background())

	if !strings.Contains(string(body), `"signed":true`) || !strings.Contains(string(body), `"origin_server_ts"`) {
		t.Fatalf("тело транзакции: %s", body)
	}

	if _, ok := st.dead[11]; !ok || len(st.acked) != 1 || st.acked[0] != 10 {
		t.Fatalf("acked=%v dead=%v", st.acked, st.dead)
	}
}

// senderFunc — Sender из функции.
type senderFunc func(ctx context.Context, destination, method, path string, body []byte) ([]byte, int, error)

func (f senderFunc) Send(ctx context.Context, destination, method, path string, body []byte) (resp []byte, status int, err error) {
	return f(ctx, destination, method, path, body)
}
