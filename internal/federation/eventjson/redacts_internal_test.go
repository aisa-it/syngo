package eventjson

import "testing"

// TestRedactKeepsTopLevelRedacts — prune_event Synapse сохраняет top-level
// `redacts` (v10), поэтому вырезать его нельзя: иначе подпись/event_id
// redaction-события расходятся.
func TestRedactKeepsTopLevelRedacts(t *testing.T) {
	t.Parallel()

	object := map[string]any{
		"type": "m.room.redaction", "room_id": "!r:test", "sender": "@a:test",
		"redacts": "$target", "content": map[string]any{"reason": "x"},
		"depth": float64(5), "prev_events": []any{}, "auth_events": []any{},
		"origin_server_ts": float64(1), "hashes": map[string]any{"sha256": "h"},
		"unsigned": map[string]any{"age": float64(3)},
	}

	redacted := RedactVersioned(object, 10)
	if _, ok := redacted["redacts"]; !ok {
		t.Fatalf("redacts вырезан: %v", redacted)
	}

	if _, ok := redacted["unsigned"]; ok {
		t.Fatalf("unsigned не должен попадать в pruned: %v", redacted)
	}
}
