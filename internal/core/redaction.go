package core

import (
	"context"
	"encoding/json"
)

// RedactContent применяет правила redaction к content события.
// Правила зависят от версии комнаты (спека, rooms/v10 и rooms/v11):
//   - v11: create сохраняет весь content, power_levels сохраняет invite,
//     redaction сохраняет redacts, у member в third_party_invite остаётся только signed;
//   - v10 и старше: create сохраняет только creator, invite/redacts/third_party_invite
//     вырезаются целиком.
//
// guest_access и aliases в v10+ не защищены и вырезаются.
func RedactContent(eventType string, content []byte) []byte {
	return RedactContentFor(eventType, content, DefaultRoomVersion)
}

// RedactContentFor — то же с явной версией комнаты.
func RedactContentFor(eventType string, content []byte, version int) []byte {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(content, &parsed); err != nil {
		return []byte("{}")
	}

	kept := redactedContent(eventType, version, parsed)
	if kept == nil {
		// Кастомные типы и create в v11: content сохраняется целиком.
		return content
	}

	out, err := json.Marshal(kept)
	if err != nil {
		return []byte("{}")
	}

	return out
}

// redactedContent возвращает разрешённые ключи content. nil — сохранить content целиком.
func redactedContent(eventType string, version int, parsed map[string]json.RawMessage) map[string]json.RawMessage {
	v11 := version >= 11

	switch eventType {
	case "m.room.create":
		if v11 {
			return nil // в v11 у create защищён весь content
		}

		return pick(parsed, "creator")
	case "m.room.member":
		allowed := []string{"membership", "join_authorised_via_users_server"}

		kept := pick(parsed, allowed...)
		if raw, ok := parsed["third_party_invite"]; ok {
			if v11 {
				kept["third_party_invite"] = thirdPartySigned(raw)
			}
		}

		return kept
	case "m.room.join_rules":
		return pick(parsed, "join_rule", "allow")
	case "m.room.power_levels":
		allowed := []string{
			"ban", "events", "events_default", "kick", "redact",
			"state_default", "users", "users_default",
		}
		if v11 {
			allowed = append(allowed, "invite")
		}

		return pick(parsed, allowed...)
	case "m.room.history_visibility":
		return pick(parsed, "history_visibility")
	case "m.room.guest_access":
		// Synapse prune_event вырезает content целиком (и в v10, и в v11): сверено
		// по его же event_id — $4X9I6… соответствует content {}, а не {"guest_access"}.
		return pick(parsed)
	case "m.room.aliases":
		// aliases защищены только в комнатах до v6; для v10 и v11 content удаляется.
		return pick(parsed)
	case "m.room.redaction":
		if v11 {
			return pick(parsed, "redacts")
		}

		return pick(parsed)
	}

	// Все остальные типы (m.reaction, m.sticker, опросы, кастомные) - content
	// удаляется целиком, как в спецификации и Synapse prune_event. Раньше content
	// не-m.room.* событий сохранялся: удаленный стикер или опрос был виден целиком,
	// а event_id и подписи расходились с Synapse.
	return pick(parsed)
}

func pick(parsed map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}

	for _, key := range keys {
		if value, ok := parsed[key]; ok {
			out[key] = value
		}
	}

	return out
}

// thirdPartySigned оставляет в third_party_invite только ключ signed (v11).
func thirdPartySigned(raw json.RawMessage) json.RawMessage {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return json.RawMessage(`{}`)
	}

	signed, ok := parsed["signed"]
	if !ok {
		return json.RawMessage(`{}`)
	}

	out, err := json.Marshal(map[string]json.RawMessage{"signed": signed})
	if err != nil {
		return json.RawMessage(`{}`)
	}

	return out
}

// IsRedactionAllowedBy — грубая проверка «может ли отправитель редактировать событие»:
// автор события всегда может, остальным нужен уровень redact из power_levels.
func IsRedactionAllowedBy(sender, author string) bool {
	return sender != "" && sender == author
}

// RedactionEventStore - события по id (unsigned.redacted_because).
type RedactionEventStore interface {
	// EventsByIDs - события по id; отсутствующие пропускаются.
	EventsByIDs(ctx context.Context, ids []string) ([]Event, error)
}

// RedactionLookup читает события удаления для unsigned.redacted_because: Element
// без него показывает удаленное сообщение как «Unable to decrypt».
// Чтение перенесено из HTTP-слоя.
type RedactionLookup struct {
	store RedactionEventStore
}

// NewRedactionLookup создает чтение событий удаления.
func NewRedactionLookup(store RedactionEventStore) *RedactionLookup {
	return &RedactionLookup{store: store}
}

// ByID - найденные события по id (одним запросом); при сбое - пустая map и
// ошибка: вызывающий подставляет заглушки.
func (l *RedactionLookup) ByID(ctx context.Context, ids []string) (map[string]Event, error) {
	events, err := l.store.EventsByIDs(ctx, ids)

	byID := make(map[string]Event, len(events))
	for i := range events {
		byID[events[i].EventID] = events[i]
	}

	return byID, err
}
