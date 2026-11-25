//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/store"
)

// TestEnqueueEDUClaimedAsEDU - EDU ставится получателям без события, дубли
// destination схлопываются, запрещенный политикой пропускается, а захваченная
// пачка отдает EDU в OutboxItem.EDU.
func TestEnqueueEDUClaimedAsEDU(t *testing.T) {
	db := freshOutboxDB(t)
	ctx := context.Background()

	db.SetOutboxDestinationFilter(func(destination string) bool { return destination != "denied.test" })
	t.Cleanup(func() { db.SetOutboxDestinationFilter(nil) })

	edu := []byte(`{"edu_type":"m.typing","content":{"room_id":"!r:x","user_id":"@a:x","typing":true}}`)
	if err := db.EnqueueEDU(ctx, []string{"a.test", "a.test", "denied.test", "b.test"}, edu); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var count int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM federation_outbox`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}

	if count != 2 {
		t.Fatalf("элементов %d, ожидали 2 (a.test, b.test)", count)
	}

	batch, err := db.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: "a.test", BatchSize: 10, Now: time.Now().Add(time.Second), Lease: time.Minute, NewTxnID: "t1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(batch.Items) != 1 || len(batch.Items[0].EDU) == 0 || batch.Items[0].EventID != "" {
		t.Fatalf("пачка: %+v", batch.Items)
	}
}
