package join

import "testing"

// TestComputeEventIDPrunesContent — event_id считается по pruned-событию: для
// m.room.create content урезается до {"creator":…}. Вектор снят с живого Synapse
// 1.165.0 (событие $fjTdteGDsi8…), иначе id расходятся.
func TestComputeEventIDPrunesContent(t *testing.T) {
	t.Parallel()

	event := map[string]any{
		"room_id": "!VsllOLRHleslbPrAai:synapse.test", "type": "m.room.create",
		"sender": "@bob:synapse.test", "state_key": "", "depth": float64(1),
		"origin_server_ts": float64(1795047317101),
		"prev_events":      []any{}, "auth_events": []any{},
		"content":    map[string]any{"creator": "@bob:synapse.test", "room_version": "10"},
		"hashes":     map[string]any{"sha256": "7LgHTILL846itrf2oZhqmtkDUhiX8thl5l0NFxS+Mgg"},
		"signatures": map[string]any{"synapse.test": map[string]any{"ed25519:a_WqNg": "x"}},
		"unsigned":   map[string]any{"age_ts": float64(1795047317101)},
	}

	id, err := computeEventID(event, 10)
	if err != nil {
		t.Fatalf("computeEventID: %v", err)
	}

	if id != "$fjTdteGDsi8Mw7vkSJVR3OCxtmXu-Z65EDFJMZ3M-Go" {
		t.Fatalf("event_id = %s", id)
	}
}

// TestDomainOfPort - сервер комнаты - все после первого двоеточия, с портом:
// раньше join в !r:example.com:8448 уходил на сервер "8448".
func TestDomainOfPort(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"!r:example.com:8448": "example.com:8448",
		"@u:example.com":      "example.com",
		"!r:[::1]:8448":       "[::1]:8448",
		"nocolon":             "",
	}

	for id, want := range cases {
		if got := domainOf(id); got != want {
			t.Errorf("domainOf(%q) = %q, ожидали %q", id, got, want)
		}
	}
}
