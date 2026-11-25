// Package eventjson подписывает и проверяет события Matrix так же, как Synapse:
// подпись считается по **редактированному** (redacted) JSON события
// (synapse/crypto/keyring.py: redact_event(event).get_pdu_json()), а не по полному.
package eventjson

import (
	"encoding/json"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// refAllowedKeys — top-level поля, которые остаются после prune_event Synapse
// (room v10/v11). Всё остальное (“origin”, “unsigned”, случайные поля) вырезается
// до подписи/проверки.
var refAllowedKeys = map[string]struct{}{
	// event_id Synapse оставляет в pruned-PDU: он нужен для проверки подписей
	// событий в v1-формате (invite/send_join), где id передаётся явно.
	"event_id": {},
	"room_id":  {}, "type": {}, "sender": {}, "state_key": {}, "content": {},
	"hashes": {}, "depth": {}, "prev_events": {}, "auth_events": {}, "origin_server_ts": {},
	// signatures нужны для Verify (сама подпись считается без них).
	"signatures": {},
	// redacts — top-level в комнатах < v11: prune_event его сохраняет, и без него
	// reference hash redaction-события не совпадёт с Synapse.
	"redacts": {},
}

// EventID — единственная точка расчёта event_id для комнат v3+:
// reference hash по redacted-копии события с правилами версии комнаты.
// Раньше id считался в трёх местах (core, /send, join-клиент), и любое
// расхождение в правилах давало «событие с другим id» на стороне пира.
func EventID(object map[string]any, version int) (string, error) {
	// Каноническая реализация — в core (eventjson импортирует core, не наоборот):
	// один расчёт на все пути, включая core.eventID.
	return core.EventID(object, version)
}

// Redact возвращает копию события с content, урезанным по правилам redaction
// для его типа, и без top-level полей вне whitelist prune_event. Подпись/проверка
// идут по этой копии, на провод уходит оригинал.
func Redact(object map[string]any) map[string]any {
	return RedactVersioned(object, core.DefaultRoomVersion)
}

// RedactVersioned — то же с правилами redaction конкретной версии комнаты
// (в v11 create сохраняет content целиком, PL сохраняет invite и т.д.).
func RedactVersioned(object map[string]any, version int) map[string]any {
	clone := make(map[string]any, len(object))

	for k, v := range object {
		if _, ok := refAllowedKeys[k]; !ok {
			continue
		}

		clone[k] = v
	}

	eventType, _ := object["type"].(string)
	content, ok := object["content"]
	if !ok || eventType == "" {
		return clone
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return clone
	}

	redacted := core.RedactContentFor(eventType, raw, version)
	clone["content"] = json.RawMessage(redacted)

	return clone
}

// Sign подписывает событие: подпись считается по redacted-копии, а возвращается
// объект с полным content и подписью в signatures (так делает Synapse).
func Sign(key signing.Key, serverName string, object map[string]any) (map[string]any, error) {
	return SignVersioned(key, serverName, object, core.DefaultRoomVersion)
}

// SignVersioned — подпись по redacted-копии с правилами версии комнаты.
func SignVersioned(key signing.Key, serverName string, object map[string]any, version int) (map[string]any, error) {
	signed, err := signing.SignJSON(key, serverName, RedactVersioned(object, version))
	if err != nil {
		return nil, fmt.Errorf("eventjson: sign: %w", err)
	}

	signatures, ok := signed["signatures"]
	if !ok {
		return nil, fmt.Errorf("eventjson: no signatures after sign")
	}

	object["signatures"] = signatures

	return object, nil
}

// Verify проверяет подпись события по redacted-копии.
func Verify(object map[string]any, serverName string, keyLookup func(origin, keyID string) (string, int64, error)) error {
	return VerifyVersioned(object, serverName, core.DefaultRoomVersion, keyLookup)
}

// VerifyVersioned — проверка подписи по redacted-копии с правилами версии комнаты.
func VerifyVersioned(
	object map[string]any, serverName string, version int,
	keyLookup func(origin, keyID string) (string, int64, error),
) error {
	return signing.VerifyJSON(normalize(RedactVersioned(object, version)), serverName, keyLookup)
}

// normalize приводит значения к generic-виду (map[string]any), иначе строгие
// приведения типов в signing.VerifyJSON не срабатывают на map[string]string.
func normalize(object map[string]any) map[string]any {
	raw, err := json.Marshal(object)
	if err != nil {
		return object
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return object
	}

	return out
}

// VerifyWithoutEventID — то же, но не включает event_id (room v3+).
func VerifyWithoutEventID(object map[string]any, serverName string, keyLookup func(origin, keyID string) (string, int64, error)) error {
	return signing.VerifyJSONWithoutEventID(normalize(Redact(object)), serverName, keyLookup)
}
