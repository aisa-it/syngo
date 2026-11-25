package outbox_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/outbox"
	"github.com/aisa-it/syngo/internal/store"
)

// bodySender запоминает тела отправленных транзакций.
type bodySender struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (b *bodySender) Send(_ context.Context, _, _, _ string, body []byte) (resp []byte, status int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.bodies = append(b.bodies, append([]byte(nil), body...))

	return []byte(`{"pdus":{}}`), 200, nil
}

// TestWorkerSendsEDUs - элемент с готовым EDU уходит в "edus" транзакции и
// подтверждается вместе с PDU. Раньше "edus" всегда был пустым, а
// элемент без PDU уходил в dead-letter как «event not found».
func TestWorkerSendsEDUs(t *testing.T) {
	t.Parallel()

	st := newFakeStore()
	st.add("peer.test")

	st.mu.Lock()
	st.nextID++
	eduID := st.nextID
	st.items = append(st.items, &fakeItem{OutboxItem: store.OutboxItem{
		ID: eduID, Destination: "peer.test",
		EDU: []byte(`{"edu_type":"m.typing","content":{"room_id":"!r:ours.test","user_id":"@a:ours.test","typing":true}}`),
	}})
	st.mu.Unlock()

	sender := &bodySender{}
	worker := outbox.New(st, sender, outbox.Options{
		Origin: "ours.test", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }, Logger: quiet(),
	})

	if _, err := worker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(st.dead) != 0 {
		t.Fatalf("EDU ушел в dead-letter: %v", st.dead)
	}

	if len(st.acked) != 2 {
		t.Fatalf("acked=%v, ожидали PDU и EDU", st.acked)
	}

	if len(sender.bodies) != 1 {
		t.Fatalf("транзакций %d, ожидали 1", len(sender.bodies))
	}

	var txn struct {
		PDUs []json.RawMessage `json:"pdus"`
		EDUs []struct {
			Type    string         `json:"edu_type"`
			Content map[string]any `json:"content"`
		} `json:"edus"`
	}

	if err := json.Unmarshal(sender.bodies[0], &txn); err != nil {
		t.Fatalf("тело: %v", err)
	}

	if len(txn.PDUs) != 1 || len(txn.EDUs) != 1 || txn.EDUs[0].Type != "m.typing" {
		t.Fatalf("транзакция: %s", sender.bodies[0])
	}
}

// TestWorkerOnlyEDUs - транзакция только из EDU: "pdus" - пустой массив, не null
// (Synapse требует список).
func TestWorkerOnlyEDUs(t *testing.T) {
	t.Parallel()

	st := newFakeStore()

	st.mu.Lock()
	st.nextID++
	st.items = append(st.items, &fakeItem{OutboxItem: store.OutboxItem{
		ID: st.nextID, Destination: "peer.test",
		EDU: []byte(`{"edu_type":"m.presence","content":{"push":[]}}`),
	}})
	st.mu.Unlock()

	sender := &bodySender{}
	worker := outbox.New(st, sender, outbox.Options{Origin: "ours.test", Logger: quiet()})

	if _, err := worker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(sender.bodies) != 1 {
		t.Fatalf("транзакций %d", len(sender.bodies))
	}

	var txn map[string]json.RawMessage
	_ = json.Unmarshal(sender.bodies[0], &txn)

	if string(txn["pdus"]) != "[]" {
		t.Fatalf("pdus=%s, ожидали []", txn["pdus"])
	}
}
