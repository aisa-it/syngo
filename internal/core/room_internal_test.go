package core

import (
	"testing"
)

// TestFormatParseStreamToken — курсоры /messages: формат "s<n>" и границы.
func TestFormatParseStreamToken(t *testing.T) {
	t.Parallel()

	// Отрицательные - события backfill из Synapse.
	for _, value := range []int64{-10, -1, 0, 1, 42, 1 << 40} {
		token := formatStreamToken(value)
		if token != "s"+itoa(value) {
			t.Fatalf("formatStreamToken(%d) = %q", value, token)
		}

		got, err := parsePageToken(token)
		if err != nil || got != (pageToken{kind: pageTokenStream, stream: value}) {
			t.Fatalf("parsePageToken(%q) = %+v, %v", token, got, err)
		}
	}

	if _, err := parsePageToken("xx"); err == nil {
		t.Fatal("мусорный курсор должен быть ошибкой")
	}
}

// TestDefaultPowerLevels — создатель получает 100, приглашённые 0.
func TestDefaultPowerLevels(t *testing.T) {
	t.Parallel()

	levels := defaultPowerLevels("@alice:test.local", []string{"@bob:test.local", "@alice:test.local"})

	users, ok := levels["users"].(map[string]int)
	if !ok {
		t.Fatalf("users = %T", levels["users"])
	}

	if users["@alice:test.local"] != 100 {
		t.Fatalf("создатель = %d, ожидали 100", users["@alice:test.local"])
	}

	if users["@bob:test.local"] != 0 {
		t.Fatalf("приглашённый = %d, ожидали 0", users["@bob:test.local"])
	}

	if levels["users_default"] != 0 || levels["state_default"] != 50 {
		t.Fatalf("дефолты power_levels: %v", levels)
	}
}

// itoa — локальная замена strconv, чтобы не тянуть импорт в тест ядра.
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}

	if value < 0 {
		return "-" + itoa(-value)
	}

	var buf [20]byte
	pos := len(buf)

	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}

	return string(buf[pos:])
}
