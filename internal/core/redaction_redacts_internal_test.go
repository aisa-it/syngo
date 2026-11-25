package core

import (
	"encoding/json"
	"testing"
)

// TestEventObjectRedactionStripsContentRedacts — до v11 цель redaction живёт
// только в top-level `redacts`; в content её быть не должно.
func TestEventObjectRedactionStripsContentRedacts(t *testing.T) {
	t.Parallel()

	ev := Event{
		RoomID: "!r:test", Type: "m.room.redaction", Sender: "@a:test",
		Content: json.RawMessage(`{"redacts":"$t","reason":"x"}`), RoomVersion: 10,
	}

	object := PDUObject(ev)

	if object["redacts"] != "$t" {
		t.Fatalf("top-level redacts = %v", object["redacts"])
	}

	var content map[string]any
	if err := json.Unmarshal(object["content"].(json.RawMessage), &content); err != nil {
		t.Fatalf("content: %v", err)
	}

	if _, has := content["redacts"]; has {
		t.Fatalf("content.redacts не вырезан: %v", content)
	}

	if content["reason"] != "x" {
		t.Fatalf("reason потерян: %v", content)
	}
}
