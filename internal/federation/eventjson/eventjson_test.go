package eventjson

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Подпись события считается по redacted-JSON (как Synapse), а content на проводе
// остаётся полным: проверка подписи должна проходить по Redact(object).
func TestSignVerifyWithRedaction(t *testing.T) {
	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	object := map[string]any{
		"type": "m.room.name", "room_id": "!r:test", "sender": "@a:test",
		"state_key": "", "depth": 1, "origin_server_ts": 1,
		"content": map[string]any{"name": "секретное имя"},
	}

	signed, err := Sign(key, "test", object)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, ok := signed["content"].(map[string]any); !ok {
		t.Fatal("content на проводе должен остаться полным")
	}

	if err := Verify(signed, "test", func(_, _ string) (string, int64, error) {
		return key.Public(), 0, nil
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestRedactPrunesTopLevel — при redaction выпадают top-level поля вне
// whitelist prune_event (origin, произвольный мусор), content фильтруется по типу,
// а event_id остаётся (нужен для v1-подписей).
func TestRedactPrunesTopLevel(t *testing.T) {
	t.Parallel()

	object := map[string]any{
		"event_id":  "$e1:test.local",
		"type":      "m.room.member",
		"room_id":   "!r:test.local",
		"sender":    "@a:test.local",
		"state_key": "@b:test.local",
		"content": map[string]any{
			"membership":  "invite",
			"displayname": "Боб",
			"reason":      "секрет",
		},
		"depth":            1,
		"prev_events":      []any{},
		"auth_events":      []any{},
		"origin_server_ts": 1,
		"origin":           "test.local",
		"junk":             []any{"лишнее"},
	}

	redacted := Redact(object)

	if _, ok := redacted["origin"]; ok {
		t.Fatal("origin не выпал из redacted-события")
	}

	if _, ok := redacted["junk"]; ok {
		t.Fatal("произвольное поле не выпало из redacted-события")
	}

	if redacted["event_id"] != "$e1:test.local" {
		t.Fatalf("event_id потерян: %v", redacted["event_id"])
	}

	content, ok := redacted["content"].(json.RawMessage)
	if !ok {
		t.Fatalf("content = %T, ожидали json.RawMessage", redacted["content"])
	}

	body := string(content)
	if !contains(body, "membership") || contains(body, "displayname") || contains(body, "reason") {
		t.Fatalf("content не отфильтрован по redaction: %s", body)
	}

	if redacted["type"] != "m.room.member" || redacted["state_key"] != "@b:test.local" {
		t.Fatalf("ключевые поля потеряны: %+v", redacted)
	}
}

// contains — локальная проверка подстроки (без импорта strings).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
