package api

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestParseFederationPDUComputesEventID — PDU из комнат v3+ приходит без event_id,
// получатель обязан его вычислить (reference hash по redacted-событию), а не
// отвергать транзакцию как malformed.
func TestParseFederationPDUComputesEventID(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{
		"type":"m.room.member","room_id":"!r:a.test","sender":"@a:a.test","state_key":"@b:b.test",
		"content":{"membership":"invite"},"depth":3,"origin_server_ts":1,
		"prev_events":[],"auth_events":[],"hashes":{"sha256":"x"}
	}`)

	pdu, err := parseFederationPDU(raw, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("parseFederationPDU: %v", err)
	}

	if len(pdu.EventID) < 10 || pdu.EventID[0] != '$' {
		t.Fatalf("event_id не вычислен: %q", pdu.EventID)
	}
}
