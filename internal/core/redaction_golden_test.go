package core_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestRedactContentGoldenVectors — ожидания взяты из спеки
// (rooms/v10 и rooms/v11, раздел Redactions), а не из нашего кода.
func TestRedactContentGoldenVectors(t *testing.T) {
	t.Parallel()

	vectors := []struct {
		name    string
		typ     string
		version int
		in      string
		want    string
	}{
		// v10
		{
			"v10 create", "m.room.create", 10,
			`{"creator":"@a:test","room_version":"10","m.federate":true}`, `{"creator":"@a:test"}`,
		},
		{
			"v10 member", "m.room.member", 10,
			`{"membership":"join","displayname":"A","third_party_invite":{"signed":{"token":"t"}}}`,
			`{"membership":"join"}`,
		},
		{
			"v10 join_rules", "m.room.join_rules", 10,
			`{"join_rule":"restricted","allow":[{"type":"m.room_membership","room_id":"!r:test"}],"x":1}`,
			`{"allow":[{"type":"m.room_membership","room_id":"!r:test"}],"join_rule":"restricted"}`,
		},
		{
			"v10 power_levels", "m.room.power_levels", 10,
			`{"ban":50,"invite":50,"users":{"@a:test":100},"notifications":{"room":50}}`,
			`{"ban":50,"users":{"@a:test":100}}`,
		},
		{
			"v10 history_visibility", "m.room.history_visibility", 10,
			`{"history_visibility":"shared","x":1}`, `{"history_visibility":"shared"}`,
		},
		{
			// Сверено по event_id Synapse 1.165.0: prune_event вырезает content
			// guest_access целиком и в v10 ($4X9I6… = content {}).
			"v10 guest_access", "m.room.guest_access", 10,
			`{"guest_access":"can_join","other":1}`, `{}`,
		},
		{
			// aliases защищены только до v6 (спецификация, rooms/v6).
			"v10 aliases", "m.room.aliases", 10,
			`{"aliases":["#a:test"],"x":1}`, `{}`,
		},
		{
			"v10 redaction", "m.room.redaction", 10,
			`{"redacts":"$e","reason":"x"}`, `{}`,
		},
		{
			// Типы вне списка спецификации - content удаляется целиком (prune_event Synapse).
			"v10 custom", "com.example.custom", 10,
			`{"a":1,"b":{"c":2}}`, `{}`,
		},

		// v11
		{
			"v11 create", "m.room.create", 11,
			`{"creator":"@a:test","room_version":"11","m.federate":true,"additional_creators":["@b:test"]}`,
			`{"additional_creators":["@b:test"],"creator":"@a:test","m.federate":true,"room_version":"11"}`,
		},
		{
			"v11 member", "m.room.member", 11,
			`{"membership":"invite","displayname":"A","third_party_invite":{"display_name":"B","signed":{"token":"t"}}}`,
			`{"membership":"invite","third_party_invite":{"signed":{"token":"t"}}}`,
		},
		{
			"v11 power_levels", "m.room.power_levels", 11,
			`{"ban":50,"invite":25,"events":{"m.room.name":50},"notifications":{"room":50}}`,
			`{"ban":50,"events":{"m.room.name":50},"invite":25}`,
		},
		{
			"v11 redaction", "m.room.redaction", 11,
			`{"redacts":"$e","reason":"x"}`, `{"redacts":"$e"}`,
		},
		{
			"v11 guest_access", "m.room.guest_access", 11,
			`{"guest_access":"can_join"}`, `{}`,
		},
		{
			"v11 aliases", "m.room.aliases", 11,
			`{"aliases":["#a:test"]}`, `{}`,
		},
		{
			"v11 custom", "com.example.custom", 11,
			`{"a":1}`, `{}`,
		},
	}

	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			t.Parallel()

			got := core.RedactContentFor(vector.typ, []byte(vector.in), vector.version)

			var gotAny, wantAny any
			if err := json.Unmarshal(got, &gotAny); err != nil {
				t.Fatalf("результат не JSON: %v", err)
			}

			if err := json.Unmarshal([]byte(vector.want), &wantAny); err != nil {
				t.Fatalf("ожидание не JSON: %v", err)
			}

			gotJSON, _ := json.Marshal(gotAny)
			wantJSON, _ := json.Marshal(wantAny)

			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("%s v%d:\n got %s\nwant %s", vector.typ, vector.version, gotJSON, wantJSON)
			}
		})
	}
}
