package core_test

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestReferenceHashGoldenVector — вектор посчитан ВНЕ нашего кода
// (Python: canonical JSON + sha256 + URL-safe unpadded base64) для create-события
// комнаты v10. Ловит расхождения канонизации/redaction, которые наши тесты,
// считающие ожидание тем же кодом, не видят.
func TestReferenceHashGoldenVector(t *testing.T) {
	t.Parallel()

	content := []byte(`{"creator":"@alice:test.local"}`)

	object := map[string]any{
		"auth_events": []string{}, "content": json.RawMessage(content), "depth": 1,
		"origin_server_ts": 1, "prev_events": []string{},
		"room_id": "!golden:test.local", "sender": "@alice:test.local",
		"state_key": "", "type": "m.room.create",
	}

	hash, err := signing.ContentHash(object)
	if err != nil {
		t.Fatalf("content hash: %v", err)
	}

	if hash != "NRv8XS+CCpIxVvEXRi8u7yX6hV8QZDqF8ovRTdZEIU0" {
		t.Fatalf("content hash = %q, вектор = %q", hash,
			"NRv8XS+CCpIxVvEXRi8u7yX6hV8QZDqF8ovRTdZEIU0")
	}

	object["hashes"] = map[string]string{"sha256": hash}

	// reference hash — по redacted-событию (create: только creator).
	pruned := map[string]any{}
	for k, v := range object {
		pruned[k] = v
	}

	pruned["content"] = json.RawMessage(core.RedactContent("m.room.create", content))

	id, err := signing.ReferenceHash(pruned)
	if err != nil {
		t.Fatalf("reference hash: %v", err)
	}

	if id != "$kj0TPBbd451P70hemGrIHlrY5Qo4YeXLC9u3OfC-wGc" {
		t.Fatalf("event_id = %q, вектор = %q", id, "$kj0TPBbd451P70hemGrIHlrY5Qo4YeXLC9u3OfC-wGc")
	}
}

// TestEventIDSinglePointOfTruth — core.EventID и eventjson.EventID дают один id
// на одном событии: расчёт больше не дублируется.
func TestEventIDSinglePointOfTruth(t *testing.T) {
	t.Parallel()

	object := map[string]any{
		"room_id": "!single:test.local", "type": "m.room.create", "sender": "@alice:test.local",
		"state_key": "", "depth": 1, "origin_server_ts": 1,
		"prev_events": []string{}, "auth_events": []string{},
		"hashes":     map[string]string{"sha256": "NRv8XS+CCpIxVvEXRi8u7yX6hV8QZDqF8ovRTdZEIU0"},
		"content":    json.RawMessage(`{"creator":"@alice:test.local","room_version":"11"}`),
		"event_id":   "$ignored:test.local",
		"signatures": map[string]any{"test.local": map[string]any{"ed25519:a_1": "x"}},
		"unsigned":   map[string]any{"age_ts": 1},
	}

	fromCore, err := core.EventID(object, 11)
	if err != nil {
		t.Fatalf("core.EventID: %v", err)
	}

	fromJSON, err := eventjson.EventID(object, 11)
	if err != nil {
		t.Fatalf("eventjson.EventID: %v", err)
	}

	if fromCore != fromJSON {
		t.Fatalf("id расходятся: core %s, eventjson %s", fromCore, fromJSON)
	}
}
