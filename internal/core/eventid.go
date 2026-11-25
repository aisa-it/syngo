package core

import (
	"encoding/json"
	"fmt"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// pduFields — top-level поля PDU, которые входят в reference hash (room v3+).
// event_id, signatures, unsigned, origin и прочее вырезаются, как в prune_event
// Synapse.
var pduFields = map[string]struct{}{
	"room_id": {}, "type": {}, "sender": {}, "state_key": {}, "content": {},
	"hashes": {}, "depth": {}, "prev_events": {}, "auth_events": {}, "origin_server_ts": {},
}

// legacyPDUFields — top-level поля, которые алгоритм redaction комнат v1-v10
// сохраняет, а v11 вырезает (спецификация, rooms/v11 «Redactions»). Без них
// event_id событий Synapse с полем origin расходился.
var legacyPDUFields = map[string]struct{}{"origin": {}, "membership": {}, "prev_state": {}}

// EventID считает event_id для события комнаты: «$» + base64url(sha256(canonical
// JSON)). Единственная точка расчёта — её используют core, федеративный приём и
// исходящий join; eventjson.EventID вызывает её же (цикла импортов нет: core не
// зависит от eventjson).
func EventID(object map[string]any, version int) (string, error) {
	if version <= 0 {
		version = DefaultRoomVersion
	}

	pruned := make(map[string]any, len(object))

	for key, value := range object {
		_, keep := pduFields[key]
		if !keep && version <= 10 {
			_, keep = legacyPDUFields[key]
		}

		if keep {
			pruned[key] = value
		}
	}

	eventType, _ := object["type"].(string)

	if raw, err := json.Marshal(object["content"]); err == nil && eventType != "" {
		// json.RawMessage, иначе []byte закодируется base64 и хеш разойдётся.
		pruned["content"] = json.RawMessage(RedactContentFor(eventType, raw, version))
	}

	id, err := signing.ReferenceHash(pruned)
	if err != nil {
		return "", fmt.Errorf("core: event id: %w", err)
	}

	return id, nil
}
