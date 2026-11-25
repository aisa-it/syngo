package core

import "testing"

// TestRedactionMatchesSynapseV10 — правила redaction для room v10 сверены с
// prune_event живого Synapse: у create остаётся только creator, у power_levels
// нет invite/notifications/historical.
func TestRedactionMatchesSynapseV10(t *testing.T) {
	t.Parallel()

	create := []byte(`{"creator":"@a:test","room_version":"10","m.federate":true}`)
	if got := string(RedactContent("m.room.create", create)); got != `{"creator":"@a:test"}` {
		t.Fatalf("create redaction = %s", got)
	}

	pl := []byte(`{"ban":50,"invite":0,"users":{"@a:test":100},"notifications":{"room":50}}`)
	if got := string(RedactContent("m.room.power_levels", pl)); got != `{"ban":50,"users":{"@a:test":100}}` {
		t.Fatalf("power_levels redaction = %s", got)
	}
}
