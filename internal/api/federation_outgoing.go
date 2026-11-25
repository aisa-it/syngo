package api

import (
	"context"
	"encoding/json"
	"fmt"
)

// Исходящая федерация: локальное событие ставится в
// federation_outbox в транзакции его записи (store.RoomStore.AppendEvent), а
// join/leave из send_join/send_leave - в транзакции их приема
// (applyMembershipEvent). PDU берется при отправке воркером outbox через
// FederationPDUs. Синхронных отправок нет - только очередь.

// FederationPDUs отдает PDU событий по event_id для воркера outbox. Событие,
// которого нет (purge), в ответе отсутствует.
//
// Это исходный PDU (federationPDUs): свое событие подписано при
// создании, чужое - как пришло; подпись не пересчитывается, и в транзакцию
// уходит ровно тот JSON, по которому пир посчитает тот же event_id. События
// без исходного PDU собираются из колонок и подписываются (wirePDU).
func (s *Server) FederationPDUs(ctx context.Context, eventIDs []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(eventIDs))

	if s.db == nil || len(eventIDs) == 0 {
		return out, nil
	}

	pdus, err := s.db.AuthEventsByID(ctx, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("api: federation events: %w", err)
	}

	versions := map[string]int{}

	for i := range pdus {
		pdu := &pdus[i]

		version, known := versions[pdu.RoomID]
		if !known && len(pdu.PDU) == 0 {
			version = s.roomVersionOf(ctx, pdu.RoomID)
			versions[pdu.RoomID] = version
		}

		raw, err := s.wirePDU(ctx, *pdu, version)
		if err != nil {
			return nil, fmt.Errorf("api: federation pdu %s: %w", pdu.EventID, err)
		}

		out[pdu.EventID] = raw
	}

	return out, nil
}
