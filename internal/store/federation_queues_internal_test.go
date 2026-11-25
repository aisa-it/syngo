package store

import "testing"

// TestPDUFromMapKeepsSignatures — pduFromMap терял signatures/hashes: события
// пира сохранялись без его подписи, и он не мог проверить их при пересылке
// («Signature on retrieved event … invalid»).
func TestPDUFromMapKeepsSignatures(t *testing.T) {
	t.Parallel()

	pdu, err := pduFromMap(map[string]any{
		"event_id": "$e:remote.test", "room_id": "!r:remote.test", "sender": "@u:remote.test",
		"type": "m.room.create", "state_key": "", "content": map[string]any{"creator": "@u:remote.test"},
		"prev_events": []any{}, "auth_events": []any{},
		"signatures": map[string]any{"remote.test": map[string]any{"ed25519:a": "sig"}},
		"hashes":     map[string]any{"sha256": "h"},
	})
	if err != nil {
		t.Fatalf("pduFromMap: %v", err)
	}

	if len(pdu.Signatures) == 0 || len(pdu.Hashes) == 0 {
		t.Fatalf("signatures=%q hashes=%q", pdu.Signatures, pdu.Hashes)
	}
}

// TestDomainOfMatrixIDPort - server_name идентификатора - все после первого
// двоеточия, с портом: раньше бралось после последнего и выходило "8448".
func TestDomainOfMatrixIDPort(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"@u:example.com:8448": "example.com:8448",
		"!r:example.com":      "example.com",
		"@u:[::1]:8448":       "[::1]:8448",
		"nocolon":             "",
	}

	for id, want := range cases {
		if got := domainOfMatrixID(id); got != want {
			t.Errorf("domainOfMatrixID(%q) = %q, ожидали %q", id, got, want)
		}

		if got := domainFromMatrixID(id); got != want {
			t.Errorf("domainFromMatrixID(%q) = %q, ожидали %q", id, got, want)
		}
	}
}
