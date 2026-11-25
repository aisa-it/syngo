package store

import (
	"encoding/json"
	"testing"
)

// TestRedactionTarget - цель redaction берется из top-level `redacts` (комнаты до
// v11) или из content.redacts (v11 и наши собственные события).
func TestRedactionTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		pdu  FedPDU
		want string
	}{
		{name: "v10: top-level redacts", pdu: FedPDU{Redacts: "$a", Content: []byte(`{"reason":"x"}`)}, want: "$a"},
		{name: "v11: content.redacts", pdu: FedPDU{Content: []byte(`{"redacts":"$b"}`)}, want: "$b"},
		{name: "пусто", pdu: FedPDU{Content: []byte(`{}`)}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pdu := tc.pdu
			if got := RedactionTarget(&pdu); got != tc.want {
				t.Fatalf("RedactionTarget = %q, ожидали %q", got, tc.want)
			}
		})
	}
}

// TestPowerNumber - уровни power_levels: число, строка с числом, отсутствие.
func TestPowerNumber(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{`50`: 50, `"75"`: 75, ``: 7, `"x"`: 7, `null`: 7}

	for raw, want := range cases {
		if got := powerNumber(json.RawMessage(raw), 7); got != want {
			t.Fatalf("powerNumber(%s) = %d, ожидали %d", raw, got, want)
		}
	}
}

// TestServerOf - сервер берется после первого двоеточия (порт остается частью имени).
func TestServerOf(t *testing.T) {
	t.Parallel()

	for id, want := range map[string]string{"@a:b.c": "b.c", "@a:b.c:8448": "b.c:8448", "x": ""} {
		if got := serverOf(id); got != want {
			t.Fatalf("serverOf(%q) = %q, ожидали %q", id, got, want)
		}
	}
}
