//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/store"
)

// TestFederationQueues — outbox (ack/fail/backoff), дедуп inbox в БД,
// destination_rooms и метрики очереди.
func TestFederationQueues(t *testing.T) {
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

	db := openDB(t)
	ctx := context.Background()

	// Outbox: enqueue → claim → ack.
	if err := db.EnqueueOutbox(ctx, "remote.test", []byte(`{"event_id":"$e1"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	destinations, err := db.OutboxDestinations(ctx, time.Now().Add(time.Minute), 10)
	if err != nil || len(destinations) != 1 || destinations[0] != "remote.test" {
		t.Fatalf("destinations = %v, err=%v", destinations, err)
	}

	now := time.Now()

	batch, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: now, Lease: time.Minute, NewTxnID: "t1",
	})
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("claim: %v, err=%v", batch, err)
	}

	// Fail: attempts растёт, next_attempt сдвигается, аренда снята.
	if err := db.FailOutbox(ctx, store.OutboxFailure{
		IDs: []int64{batch.Items[0].ID}, RetryAt: now.Add(time.Minute), KeepTxn: true,
	}); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// До next_attempt пачка не берется (раньше бралась сразу).
	early, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: now, Lease: time.Minute, NewTxnID: "t2",
	})
	if err != nil || len(early.Items) != 0 {
		t.Fatalf("claim до next_attempt: %v, err=%v", early, err)
	}

	items, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: now.Add(2 * time.Minute), Lease: time.Minute, NewTxnID: "t3",
	})
	if err != nil || len(items.Items) != 1 {
		t.Fatalf("claim after fail: %v, err=%v", items, err)
	}

	if items.Items[0].Attempts != 1 || items.TxnID != "t1" || !items.Retry {
		t.Fatalf("повтор: attempts=%d txn=%q retry=%v, ожидали 1, t1, true",
			items.Items[0].Attempts, items.TxnID, items.Retry)
	}

	// Пока next_attempt в будущем — destination не готов.
	ready, err := db.OutboxDestinations(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("destinations ready: %v", err)
	}

	if len(ready) != 0 {
		t.Fatalf("destination не должен быть готов до next_attempt: %v", ready)
	}

	// Ack удаляет.
	if err := db.AckOutbox(ctx, []int64{items.Items[0].ID}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	stats, err := db.OutboxMetrics(ctx)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}

	if stats.Depth != 0 {
		t.Fatalf("после ack глубина %d, ожидали 0", stats.Depth)
	}

	// Inbox: первый раз — новая, второй — уже видели (переживает «рестарт», т.к. в БД).
	seen, err := db.SeenInbox(ctx, "remote.test", "txn-1")
	if err != nil || seen {
		t.Fatalf("первая транзакция: seen=%v err=%v", seen, err)
	}

	seen, err = db.SeenInbox(ctx, "remote.test", "txn-1")
	if err != nil || !seen {
		t.Fatalf("повторная транзакция: seen=%v err=%v", seen, err)
	}

	// Другой origin с тем же txn_id — независимая запись.
	seen, err = db.SeenInbox(ctx, "other.test", "txn-1")
	if err != nil || seen {
		t.Fatalf("другой origin: seen=%v err=%v", seen, err)
	}

	// destination_rooms.
	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ('!r:test.local','@a:test.local') ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	if err := db.SetDestinationRoom(ctx, "remote.test", "!r:test.local"); err != nil {
		t.Fatalf("destination room: %v", err)
	}

	rooms, err := db.DestinationRooms(ctx, "remote.test")
	if err != nil || len(rooms) != 1 || rooms[0] != "!r:test.local" {
		t.Fatalf("destination rooms = %v, err=%v", rooms, err)
	}
}

// TestOutboxKeepsPDUBytes - PDU в outbox хранится байт-в-байт. Раньше
// колонка jsonb переупорядочивала ключи, переписывала числа (1e2 -> 100) и не
// принимала \u0000, и в транзакцию уходил не тот JSON, что был подписан.
func TestOutboxKeepsPDUBytes(t *testing.T) {
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

	db := openDB(t)
	ctx := context.Background()

	pdu := `{"type":"m.room.message","room_id":"!r:t","content":{"body":"a\u0000b <&>","n":1e2}}`

	if err := db.EnqueueOutbox(ctx, "remote.test", []byte(pdu)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	batch, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "remote.test", BatchSize: 10, Now: time.Now().Add(time.Minute), Lease: time.Minute, NewTxnID: "t1",
	})
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("claim: %v, err=%v", batch.Items, err)
	}

	items := batch.Items

	if string(items[0].PDU) != pdu {
		t.Fatalf("PDU из outbox:\n got: %s\nwant: %s", items[0].PDU, pdu)
	}

	var roomID string
	if err := db.Pool().QueryRow(ctx, `SELECT room_id FROM federation_outbox WHERE id = $1`, items[0].ID).Scan(&roomID); err != nil || roomID != "!r:t" {
		t.Fatalf("room_id = %q, %v", roomID, err)
	}
}
