package api

import (
	"encoding/json"
	"testing"
)

// TestStringSlicePairsAndStrings — prev_events/auth_events приходят
// и строками (v1/v2), и парами [event_id, {sha256}] (v3+). Пары раньше терялись,
// из-за чего входящий invite от Synapse падал 400 «Invalid event JSON».
func TestStringSlicePairsAndStrings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "строки", value: `["$a","$b"]`, want: []string{"$a", "$b"}},
		{
			name:  "пары v3+",
			value: `[["$a",{"sha256":"aaa"}],["$b",{"sha256":"bbb"}]]`,
			want:  []string{"$a", "$b"},
		},
		{name: "смешанный", value: `["$a",["$b",{"sha256":"b"}]]`, want: []string{"$a", "$b"}},
		{name: "пустой элемент пары", value: `[["",{}]]`, want: []string{}},
		{name: "не массив", value: `null`, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var value any
			if err := json.Unmarshal([]byte(tc.value), &value); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			got := stringSlice(value)

			if len(got) != len(tc.want) {
				t.Fatalf("stringSlice(%s) = %v, ожидали %v", tc.value, got, tc.want)
			}

			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("stringSlice(%s) = %v, ожидали %v", tc.value, got, tc.want)
				}
			}
		})
	}
}

// TestPDUFromObjectWithPairs — PDU с парами разбирается полностью (auth/prev не теряются).
func TestPDUFromObjectWithPairs(t *testing.T) {
	t.Parallel()

	var object map[string]any
	body := `{
		"event_id":"$e1","room_id":"!r:test.local","type":"m.room.member","sender":"@a:remote.test",
		"depth":4,"origin_server_ts":1700000000000,
		"content":{"membership":"invite"},
		"prev_events":[["$p1",{"sha256":"p1"}]],
		"auth_events":[["$a1",{"sha256":"a1"}],["$a2",{"sha256":"a2"}]]
	}`

	if err := json.Unmarshal([]byte(body), &object); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	pdu, err := pduFromObject(object)
	if err != nil {
		t.Fatalf("pduFromObject() error: %v", err)
	}

	if len(pdu.Prev) != 1 || pdu.Prev[0] != "$p1" {
		t.Fatalf("prev = %v", pdu.Prev)
	}

	if len(pdu.Auth) != 2 || pdu.Auth[0] != "$a1" || pdu.Auth[1] != "$a2" {
		t.Fatalf("auth = %v", pdu.Auth)
	}
}
