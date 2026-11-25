package store

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/state"
)

// applyRemoteJoinState применяет состояние из ответа send_join и join-событие.
// Присланный state - состояние до join; если локально
// известно состояние на prev_events join или у комнаты уже есть room_state, оно
// разрешается вместе с присланным.
func applyRemoteJoinState(ctx context.Context, tx *roomStateTx, roomID string, statePDUs []FedPDU, join FedPDU) error {
	sent := state.StateMap{}
	for i := range statePDUs {
		sent = state.StateAfter(sent, stateEventFromFedPDU(statePDUs[i]))
	}

	sets := []state.StateMap{sent}

	local, err := tx.StateAfter(ctx, join.Prev)
	if err != nil {
		return err
	}

	for _, id := range join.Prev {
		if st, ok := local[id]; ok {
			sets = append(sets, st)
		}
	}

	if len(sets) == 1 {
		current, err := tx.CurrentState(ctx)
		if err != nil {
			return err
		}

		if len(current) > 0 {
			sets = append(sets, current)
		}
	}

	before := sent

	if len(sets) > 1 {
		before, err = core.ResolveRoomStateSets(ctx, tx, roomID, sets)
		if err != nil {
			return fmt.Errorf("store: resolve join state: %w", err)
		}
	}

	joinEvent := stateEventFromFedPDU(join)

	if _, err := core.ApplyRoomEvents(ctx, tx, roomID, []state.Event{joinEvent}, core.ApplyOptions{
		StateBefore: map[string]state.StateMap{join.EventID: before},
		Trusted:     map[string]struct{}{join.EventID: {}},
	}); err != nil {
		return fmt.Errorf("store: remote join state: %w", err)
	}

	return nil
}

// applyTrustedEventState применяет к состоянию комнаты событие, которое не
// проверяется по состоянию до него.
func applyTrustedEventState(ctx context.Context, tx *roomStateTx, pdu FedPDU) error {
	_, err := core.ApplyRoomEvents(ctx, tx, pdu.RoomID, []state.Event{stateEventFromFedPDU(pdu)},
		core.ApplyOptions{Trusted: map[string]struct{}{pdu.EventID: {}}})

	return err
}

// stateEventFromFedPDU переводит событие хранилища в state.Event.
func stateEventFromFedPDU(pdu FedPDU) state.Event {
	return state.Event{
		ID: pdu.EventID, RoomID: pdu.RoomID, Type: pdu.Type, StateKey: pdu.StateKey, Sender: pdu.Sender,
		Content: pdu.Content, Prev: pdu.Prev, Auth: pdu.Auth, Depth: pdu.Depth, OriginTS: pdu.OriginTS,
	}
}
