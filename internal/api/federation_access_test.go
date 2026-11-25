package api

import (
	"container/heap"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// TestServerACLAllows — правила m.room.server_acl (порт не
// учитывается, deny сильнее allow, IP-литералы, глоб без учета регистра).
func TestServerACLAllows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, acl, origin string
		want              bool
	}{
		{"allow *", `{"allow":["*"]}`, "remote.test", true},
		{"deny сильнее allow", `{"allow":["*"],"deny":["remote.test"]}`, "remote.test", false},
		{"порт не учитывается", `{"allow":["*"],"deny":["remote.test"]}`, "remote.test:8448", false},
		{"глоб поддомена", `{"allow":["*.example.org"]}`, "matrix.example.org", true},
		{"глоб без учета регистра", `{"allow":["*.Example.org"]}`, "MATRIX.example.org", true},
		{"? - один символ", `{"allow":["h?.test"]}`, "h1.test", true},
		{"не в allow", `{"allow":["*.example.org"]}`, "remote.test", false},
		{"пустой ACL запрещает всех", `{}`, "remote.test", false},
		{"IP-литерал запрещен", `{"allow":["*"],"allow_ip_literals":false}`, "10.0.0.1:8448", false},
		{"IPv6-литерал запрещен", `{"allow":["*"],"allow_ip_literals":false}`, "[::1]:8448", false},
		{"IP-литерал по умолчанию разрешен", `{"allow":["*"]}`, "10.0.0.1", true},
		{"точки в глобе - буквально", `{"allow":["remote.test"]}`, "remoteXtest", false},
		{"битый ACL не применяется", `{"allow":"*"}`, "remote.test", true},
		{"не строка в deny пропускается", `{"allow":["*"],"deny":[1]}`, "remote.test", true},
	}

	for _, tc := range cases {
		if got := serverACLAllows(json.RawMessage(tc.acl), tc.origin); got != tc.want {
			t.Errorf("%s: %v, ожидали %v", tc.name, got, tc.want)
		}
	}

	if host := serverHost("[::1"); host != "[::1" {
		t.Errorf("serverHost без скобки: %q", host)
	}

	if host := serverHost("remote.test"); host != "remote.test" {
		t.Errorf("serverHost без порта: %q", host)
	}
}

// TestPrunedPDU — невидимое серверу событие отдается урезанным по
// правилам redaction версии; подпись отправителя остается верной.
func TestPrunedPDU(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatal(err)
	}

	object := map[string]any{
		"room_id": "!r:test.local", "type": "m.room.message", "sender": "@a:test.local",
		"content": map[string]any{"body": "секрет"}, "depth": 3, "origin_server_ts": 1,
		"prev_events": []string{}, "auth_events": []string{}, "origin": "test.local",
		"unsigned": map[string]any{"age": 1},
	}

	signed, err := eventjson.SignVersioned(key, "test.local", object, 10)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := core.MarshalPDU(signed)
	if err != nil {
		t.Fatal(err)
	}

	for _, version := range []int{0, 10, 11} {
		pruned := prunedPDU(raw, version)

		var got map[string]any
		if err := json.Unmarshal(pruned, &got); err != nil {
			t.Fatalf("v%d: %v", version, err)
		}

		if content, _ := got["content"].(map[string]any); len(content) != 0 {
			t.Fatalf("v%d: content не урезан: %v", version, content)
		}

		if _, has := got["unsigned"]; has {
			t.Fatalf("v%d: unsigned остался", version)
		}

		if _, has := got["origin"]; has != (version != 11) {
			t.Fatalf("v%d: origin = %v", version, got["origin"])
		}

		if version == 10 {
			lookup := func(string, string) (string, int64, error) { return key.Public(), 0, nil }
			if err := eventjson.VerifyVersioned(got, "test.local", 10, lookup); err != nil {
				t.Fatalf("подпись урезанного события: %v", err)
			}
		}
	}

	if got := prunedPDU(json.RawMessage(`[1]`), 10); string(got) != `[1]` {
		t.Fatalf("битый PDU изменен: %s", got)
	}
}

// TestDAGQueueOrder — очередь backfill: больше глубина, затем больше stream.
func TestDAGQueueOrder(t *testing.T) {
	t.Parallel()

	queue := &dagQueue{}
	for _, node := range []store.DAGEvent{
		{EventID: "a", Depth: 1},
		{EventID: "c", Depth: 3, StreamOrdering: 1},
		{EventID: "d", Depth: 3, StreamOrdering: 2},
		{EventID: "b", Depth: 2},
	} {
		heap.Push(queue, node)
	}

	var order []string
	for queue.Len() > 0 {
		order = append(order, heap.Pop(queue).(store.DAGEvent).EventID)
	}

	if strings.Join(order, "") != "dcba" {
		t.Fatalf("порядок %v", order)
	}
}
