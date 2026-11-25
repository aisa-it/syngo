package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// Исходный PDU события.
//
// Событие хранится с исходным PDU (events.pdu_json, store.FedPDU.PDU): свое - как
// мы его подписали при создании (core.RoomService с WithPDUSigner), чужое - как
// пришло (/send, send_join, invite, send_leave, догон дыр, ответ send_join).
// Федерация отдает ровно этот JSON: /event, /state, /backfill, /get_missing_events,
// send_join и транзакции outbox. События без исходного PDU (до миграции 0041,
// импорт без json) собираются из колонок тем же core.PDUObject, по которому
// считается event_id.
//
// Точки доступа:
//   - federationPDUs / FederationPDUs (воркер outbox) - PDU событий для провода
//     по id; visiblePDUs - с учетом history_visibility сервера;
//   - rawPDU - PDU для сохранения из разобранного входящего объекта;
//   - store.DB.EventPDU / AuthEventsByID - поля события и исходный PDU (FedPDU.PDU).

// federationPDUs отдает события в формате федерации в порядке ids; неизвестные id
// пропускаются, повторы - один раз.
func (s *Server) federationPDUs(ctx context.Context, ids []string) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	byID, err := s.FederationPDUs(ctx, ids)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		if raw, ok := byID[id]; ok {
			out = append(out, raw)
			delete(byID, id)
		}
	}

	return out, nil
}

// wirePDU - исходный PDU события или, если его нет, PDU, собранный из колонок:
// свое событие получает hashes и нашу подпись, чужое - сохраненные при приеме.
func (s *Server) wirePDU(ctx context.Context, pdu store.FedPDU, version int) (json.RawMessage, error) {
	if len(pdu.PDU) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, pdu.PDU); err != nil {
			return nil, fmt.Errorf("api: stored pdu %s: %w", pdu.EventID, err)
		}

		return compact.Bytes(), nil
	}

	if version <= 0 {
		version = core.DefaultRoomVersion
	}

	// Те же поля, что у события при расчете event_id (core.PDUObject): state_key только
	// у state-событий, top-level redacts у redaction до v11, пустые prev/auth - [].
	object := core.PDUObject(core.Event{
		RoomID: pdu.RoomID, Type: pdu.Type, Sender: pdu.Sender, StateKey: pdu.StateKey,
		Content: pdu.Content, PrevEvents: pdu.Prev, AuthEvents: pdu.Auth,
		Depth: pdu.Depth, OriginServerTS: pdu.OriginTS, RoomVersion: version,
	})

	ownEvent := domainOfID(pdu.Sender) == s.cfg.Matrix.ServerName

	switch {
	case ownEvent:
		// hashes удаленного события сохранены при redaction (по исходному content),
		// у остальных считаются по колонкам.
		if len(pdu.Hashes) > 0 {
			object["hashes"] = json.RawMessage(pdu.Hashes)
		} else {
			hash, err := signing.ContentHash(object)
			if err != nil {
				return nil, fmt.Errorf("api: content hash %s: %w", pdu.EventID, err)
			}

			object["hashes"] = map[string]string{"sha256": hash}
		}

		if s.federationKey != nil {
			signed, err := eventjson.SignVersioned(*s.federationKey, s.cfg.Matrix.ServerName, object, version)
			if err != nil {
				return nil, fmt.Errorf("api: sign %s: %w", pdu.EventID, err)
			}

			object = signed
		}
	case len(pdu.Hashes) > 0:
		// Чужое событие: hashes и подписи только сохраненные - пересчет сломал бы
		// подпись отправителя.
		object["hashes"] = json.RawMessage(pdu.Hashes)
	default:
		// Без hashes Synapse не разбирает PDU («missing field hashes»); хеш по нашим
		// колонкам может не совпасть с исходным - тогда пир отредактирует событие.
		s.log.WarnContext(ctx, "api: чужое событие без исходного PDU и hashes",
			"event_id", pdu.EventID, "sender", pdu.Sender)

		if hash, err := signing.ContentHash(object); err == nil {
			object["hashes"] = map[string]string{"sha256": hash}
		}
	}

	if !ownEvent && len(pdu.Signatures) > 0 {
		object["signatures"] = json.RawMessage(pdu.Signatures)
	}

	return core.MarshalPDU(object)
}

// rawPDU кодирует разобранный входящий PDU для хранения (без HTML-экранирования;
// числа json.Number остаются как пришли). Вызывающий передает объект без полей,
// добавленных для разбора (event_id из пути).
func rawPDU(object map[string]any) json.RawMessage {
	raw, err := core.MarshalPDU(object)
	if err != nil {
		return nil
	}

	return raw
}

// compactPDU - PDU из транзакции без незначащих пробелов, байты значений как пришли.
func compactPDU(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}

	return buf.Bytes()
}

// writeFederationJSON - ответ федерации 200 без HTML-экранирования: исходные PDU
// внутри уходят байт-в-байт (encoding/json переписал бы <, >, &, U+2028 в \u...).
func writeFederationJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(v); err != nil {
		slog.Error("api: encode federation response", "error", err)
	}
}

// pduHeader - поля PDU, нужные фильтрам ответа (partial state, висячие ссылки).
type pduHeader struct {
	Type     string          `json:"type"`
	StateKey *string         `json:"state_key"`
	Prev     json.RawMessage `json:"prev_events"`
	Auth     json.RawMessage `json:"auth_events"`
}

// parsePDUHeader разбирает заголовок PDU; битый JSON - пустой заголовок.
func parsePDUHeader(raw json.RawMessage) pduHeader {
	var header pduHeader

	_ = json.Unmarshal(raw, &header)

	return header
}

// refIDs - id из prev_events/auth_events: строки (v3+) или пары [id, hashes] (v1/v2).
func refIDs(raw json.RawMessage) []string {
	var list []any
	if len(raw) == 0 || json.Unmarshal(raw, &list) != nil {
		return nil
	}

	return stringSlice(list)
}
