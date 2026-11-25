package core

import (
	"encoding/json"
	"testing"
)

// TestEventObjectTopLevelRedacts — до v11 redaction несёт цель в top-level
// `redacts` (в v11 — только content.redacts). Поле входит и в объект на проводе,
// и в reference hash: без него prune_event пира и наш id расходятся.
func TestEventObjectTopLevelRedacts(t *testing.T) {
	t.Parallel()

	event := Event{
		Type:        "m.room.redaction",
		Sender:      "@alice:test",
		RoomID:      "!room:test",
		Content:     json.RawMessage(`{"reason":"spam","redacts":"$target"}`),
		RoomVersion: 10,
	}

	if got := PDUObject(event)["redacts"]; got != "$target" {
		t.Fatalf("v10: top-level redacts = %v, ожидали $target", got)
	}

	// Тот же объект должен давать стабильный id, а id — совпадать с EventID по
	// тому же объекту (единая точка расчёта).
	object := PDUObject(event)

	id, err := EventID(object, 10)
	if err != nil {
		t.Fatalf("EventID() error: %v", err)
	}

	if id == "" || id[0] != '$' {
		t.Fatalf("event_id = %q", id)
	}

	event.RoomVersion = 11

	if _, ok := PDUObject(event)["redacts"]; ok {
		t.Fatal("v11: top-level redacts не должен появляться")
	}

	// Без content.redacts поле не добавляется.
	event.RoomVersion = 10
	event.Content = json.RawMessage(`{"reason":"spam"}`)

	if _, ok := PDUObject(event)["redacts"]; ok {
		t.Fatal("v10 без content.redacts: поле не должно появляться")
	}
}
