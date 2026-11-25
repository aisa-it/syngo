//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/federation/outbox"
	"github.com/aisa-it/syngo/internal/store"
)

// freshOutboxDB — чистая схема и DB.
func freshOutboxDB(t *testing.T) *store.DB {
	t.Helper()

	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	return openDB(t)
}

// countingSender считает доставку каждого event_id и txn_id.
type countingSender struct {
	mu     sync.Mutex
	events map[string]int
	txns   map[string]int
}

func (c *countingSender) Send(_ context.Context, _, _, path string, body []byte) (resp []byte, status int, err error) {
	// Пауза держит пачку «в полете», пока второй воркер пытается ее взять.
	time.Sleep(5 * time.Millisecond)

	var txn struct {
		PDUs []struct {
			EventID string `json:"event_id"`
		} `json:"pdus"`
	}

	if err := json.Unmarshal(body, &txn); err != nil {
		return nil, 400, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.txns[path]++

	for _, pdu := range txn.PDUs {
		c.events[pdu.EventID]++
	}

	return []byte(`{"pdus":{}}`), 200, nil
}

// TestOutboxTwoWorkersNoDoubleSend — два воркера на одной БД не отправляют
// пачку дважды: раньше ClaimOutboxBatch был обычным SELECT, и
// каждый инстанс слал одни и те же элементы.
func TestOutboxTwoWorkersNoDoubleSend(t *testing.T) {
	db := freshOutboxDB(t)
	ctx := context.Background()

	const total = 40

	for i := 0; i < total; i++ {
		if err := db.EnqueueOutbox(ctx, "remote.test", fmt.Appendf(nil, `{"event_id":"$e%d"}`, i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	sender := &countingSender{events: map[string]int{}, txns: map[string]int{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var wg sync.WaitGroup

	// Оба воркера крутятся до опустошения очереди, а не фиксированное число тиков:
	// тик, заставший пачку в аренде у соседа, ничего не доставляет, и под нагрузкой
	// 30 тиков не хватало (тест падал, не находя двойной отправки).
	deadline := time.Now().Add(60 * time.Second)

	for w := 0; w < 2; w++ {
		worker := outbox.New(db, sender, outbox.Options{Origin: "test.local", BatchSize: 5, Logger: logger})

		wg.Add(1)

		go func() {
			defer wg.Done()

			for time.Now().Before(deadline) {
				stats, err := worker.Tick(ctx)
				if err != nil {
					t.Errorf("tick: %v", err)

					return
				}

				if stats.Depth == 0 {
					return
				}

				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()

	sender.mu.Lock()
	defer sender.mu.Unlock()

	if len(sender.events) != total {
		t.Fatalf("доставлено %d событий, ожидали %d", len(sender.events), total)
	}

	for id, n := range sender.events {
		if n != 1 {
			t.Fatalf("событие %s отправлено %d раз", id, n)
		}
	}

	for txn, n := range sender.txns {
		if n != 1 {
			t.Fatalf("транзакция %s отправлена %d раз", txn, n)
		}
	}
}

// TestOutboxConcurrentClaim — параллельный захват одного destination: пачку
// получает ровно один, аренда держит ее до истечения, затем пачка повторяется
// с тем же txn_id.
func TestOutboxConcurrentClaim(t *testing.T) {
	db := freshOutboxDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := db.EnqueueOutbox(ctx, "remote.test", fmt.Appendf(nil, `{"event_id":"$c%d"}`, i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	now := time.Now()

	var (
		mu      sync.Mutex
		winners []store.OutboxBatch
		wg      sync.WaitGroup
	)

	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			batch, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
				Destination: "remote.test", BatchSize: 10, Now: now, Lease: time.Minute,
				NewTxnID: fmt.Sprintf("txn-%d", i),
			})
			if err != nil {
				t.Errorf("claim: %v", err)

				return
			}

			if len(batch.Items) > 0 {
				mu.Lock()
				winners = append(winners, batch)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if len(winners) != 1 || len(winners[0].Items) != 3 {
		t.Fatalf("пачку получили %d захватчиков (%v), ожидали одного с 3 элементами", len(winners), winners)
	}

	txnID := winners[0].TxnID

	// Аренда не истекла: пачку никто не берет, destination не готов.
	again, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: now.Add(30 * time.Second), Lease: time.Minute, NewTxnID: "late",
	})
	if err != nil || len(again.Items) != 0 {
		t.Fatalf("захват при живой аренде: %v err=%v", again, err)
	}

	ready, err := db.OutboxDestinations(ctx, now.Add(30*time.Second), 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("destination с пачкой в полете готов: %v err=%v", ready, err)
	}

	// Инстанс упал, аренда истекла: пачка повторяется целиком с тем же txn_id.
	retry, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: now.Add(2 * time.Minute), Lease: time.Minute, NewTxnID: "late",
	})
	if err != nil || retry.TxnID != txnID || !retry.Retry || len(retry.Items) != 3 {
		t.Fatalf("повтор после аренды: %+v err=%v, ожидали txn %s", retry, err, txnID)
	}
}

// TestOutboxFailureModes — 4xx снимает txn_id и изолирует элементы, dead-letter
// переносит с причиной, backoff получателя держит destination, ack двигает
// last_successful_stream_ordering.
func TestOutboxFailureModes(t *testing.T) {
	db := freshOutboxDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := db.EnqueueOutbox(ctx, "remote.test", fmt.Appendf(nil, `{"event_id":"$f%d"}`, i)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	now := time.Now().Add(time.Second)
	claim := func(at time.Time, txn string) store.OutboxBatch {
		t.Helper()

		batch, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
			Destination: "remote.test", BatchSize: 10, Now: at, Lease: time.Minute, NewTxnID: txn,
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}

		return batch
	}

	first := claim(now, "a")
	if len(first.Items) != 3 {
		t.Fatalf("первая пачка %d элементов", len(first.Items))
	}

	ids := []int64{first.Items[0].ID, first.Items[1].ID, first.Items[2].ID}

	// 4xx: txn_id снят, элементы изолированы - следующие пачки по одному.
	if err := db.FailOutbox(ctx, store.OutboxFailure{IDs: ids, RetryAt: now, Isolate: true, Error: "HTTP 400"}); err != nil {
		t.Fatalf("fail: %v", err)
	}

	solo := claim(now, "b")
	if solo.TxnID != "b" || solo.Retry || len(solo.Items) != 1 || solo.Items[0].ID != ids[0] {
		t.Fatalf("изолированный элемент: %+v", solo)
	}

	// Dead-letter с причиной.
	if err := db.DeadLetterOutbox(ctx, []store.OutboxDead{{ID: ids[0], Reason: "rejected by destination: bad"}}); err != nil {
		t.Fatalf("dead-letter: %v", err)
	}

	var reason string
	if err := db.Pool().QueryRow(ctx, `SELECT reason FROM federation_outbox_dead WHERE id = $1`, ids[0]).Scan(&reason); err != nil ||
		reason != "rejected by destination: bad" {
		t.Fatalf("dead-letter reason = %q err=%v", reason, err)
	}

	stats, err := db.OutboxMetrics(ctx)
	if err != nil || stats.Depth != 2 || stats.DeadLetters != 1 {
		t.Fatalf("метрики: %+v err=%v", stats, err)
	}

	// Backoff получателя (destinations) держит destination вне выборки.
	if err := db.SaveDestinationFailure(ctx, "remote.test", now, time.Hour); err != nil {
		t.Fatalf("destination failure: %v", err)
	}

	ready, err := db.OutboxDestinations(ctx, now.Add(time.Minute), 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("destination в backoff выбран: %v err=%v", ready, err)
	}

	ready, err = db.OutboxDestinations(ctx, now.Add(2*time.Hour), 10)
	if err != nil || len(ready) != 1 {
		t.Fatalf("после backoff destination не выбран: %v err=%v", ready, err)
	}

	// Ack элементов-событий двигает last_successful_stream_ordering.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE federation_outbox SET event_id = 'x', stream_ordering = 777 WHERE id = $1`, ids[1]); err != nil {
		t.Fatalf("mark event: %v", err)
	}

	if err := db.AckOutbox(ctx, []int64{ids[1], ids[2]}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	var last int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT last_successful_stream_ordering FROM destinations WHERE destination = 'remote.test'`).Scan(&last); err != nil || last != 777 {
		t.Fatalf("last_successful_stream_ordering = %d err=%v", last, err)
	}
}
