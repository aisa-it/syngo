package api

import (
	"context"

	"github.com/aisa-it/syngo/internal/state"
	"github.com/aisa-it/syngo/internal/store"
)

// materializeRoomsState применяет записанные события федерации к состоянию их
// комнат: состояние на событие, forward extremities, room_state (state resolution
// v2 по наборам состояний); логика - в
// core.StateResolutionService. События группируются по комнатам в
// порядке появления. Без БД - ничего.
func (s *Server) materializeRoomsState(ctx context.Context, pdus []store.FedPDU) error {
	if s.stateResolution == nil || len(pdus) == 0 {
		return nil
	}

	var rooms []string

	byRoom := map[string][]state.Event{}

	for i := range pdus {
		roomID := pdus[i].RoomID
		if _, ok := byRoom[roomID]; !ok {
			rooms = append(rooms, roomID)
		}

		byRoom[roomID] = append(byRoom[roomID], stateEventFromPDU(pdus[i]))
	}

	for _, roomID := range rooms {
		if err := s.stateResolution.Materialize(ctx, roomID, byRoom[roomID]); err != nil {
			return err
		}
	}

	return nil
}

// stateEventFromPDU переводит событие хранилища в state.Event.
func stateEventFromPDU(pdu store.FedPDU) state.Event {
	return state.Event{
		ID: pdu.EventID, RoomID: pdu.RoomID, Type: pdu.Type, StateKey: pdu.StateKey, Sender: pdu.Sender,
		Content: pdu.Content, Prev: pdu.Prev, Auth: pdu.Auth, Depth: pdu.Depth, OriginTS: pdu.OriginTS,
	}
}
