package eventjson

import "testing"

// TestEventIDGoldenForCreate — единая точка расчёта event_id: вектор снят с
// живого Synapse 1.165.0 (create с content {"creator","room_version"}).
func TestEventIDGoldenForCreate(t *testing.T) {
	t.Parallel()

	object := map[string]any{
		"room_id": "!VsllOLRHleslbPrAai:synapse.test", "type": "m.room.create",
		"sender": "@bob:synapse.test", "state_key": "", "depth": float64(1),
		"origin_server_ts": float64(1795047317101),
		"prev_events":      []any{}, "auth_events": []any{},
		"content":    map[string]any{"creator": "@bob:synapse.test", "room_version": "10"},
		"hashes":     map[string]any{"sha256": "7LgHTILL846itrf2oZhqmtkDUhiX8thl5l0NFxS+Mgg"},
		"signatures": map[string]any{"synapse.test": map[string]any{"ed25519:a_WqNg": "x"}},
		"unsigned":   map[string]any{"age_ts": float64(1795047317101)},
	}

	id, err := EventID(object, 10)
	if err != nil {
		t.Fatalf("EventID: %v", err)
	}

	if id != "$fjTdteGDsi8Mw7vkSJVR3OCxtmXu-Z65EDFJMZ3M-Go" {
		t.Fatalf("event_id = %s", id)
	}
}
