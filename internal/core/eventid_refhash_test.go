package core_test

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestEventIDKeepsLegacyTopLevelFields — в комнатах v1-v10 top-level origin,
// membership и prev_state входят в reference hash, в v11 - нет.
func TestEventIDKeepsLegacyTopLevelFields(t *testing.T) {
	t.Parallel()

	mk := func(extra map[string]any) map[string]any {
		obj := map[string]any{
			"auth_events": []string{"$a"}, "content": json.RawMessage(`{}`), "depth": 5,
			"hashes": map[string]string{"sha256": "x"}, "origin_server_ts": 1, "prev_events": []string{"$p"},
			"room_id": "!r:test.local", "sender": "@alice:test.local", "type": "m.room.message",
		}
		for k, v := range extra {
			obj[k] = v
		}

		return obj
	}

	for _, field := range []string{"origin", "membership", "prev_state"} {
		with10, _ := core.EventID(mk(map[string]any{field: "x"}), 10)
		without10, _ := core.EventID(mk(nil), 10)

		if with10 == without10 {
			t.Errorf("v10: %s не входит в reference hash", field)
		}

		with11, _ := core.EventID(mk(map[string]any{field: "x"}), 11)
		without11, _ := core.EventID(mk(nil), 11)

		if with11 != without11 {
			t.Errorf("v11: %s не должен входить в reference hash", field)
		}
	}
}
