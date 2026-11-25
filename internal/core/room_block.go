package core

import (
	"context"
	"encoding/json"
	"fmt"
)

// ErrRoomBlocked - комната заблокирована администратором (admin API).
// Оборачивает ErrForbidden: API отвечает 403, как на прочие отказы в правах.
var ErrRoomBlocked = fmt.Errorf("%w: room is blocked", ErrForbidden)

// RoomBlockChecker - необязательная часть RoomStore: блокировки комнат admin API.
// Хранилище без нее (in-memory в тестах) блокировок не знает.
type RoomBlockChecker interface {
	RoomBlocked(ctx context.Context, roomID string) (bool, error)
}

// RoomBlocked сообщает, заблокирована ли комната (проверка до join);
// хранилище без блокировок - false.
func (s *RoomService) RoomBlocked(ctx context.Context, roomID string) (bool, error) {
	checker, ok := s.store.(RoomBlockChecker)
	if !ok {
		return false, nil
	}

	return checker.RoomBlocked(ctx, roomID)
}

type blockBypassKey struct{}

// WithRoomBlockBypass помечает контекст операцией администратора: tombstone и
// служебные события purge пишутся и в заблокированную комнату.
func WithRoomBlockBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockBypassKey{}, true)
}

// appendEvent - единая точка записи локальных событий: заблокированная комната
// принимает только выход и бан (участники могут покинуть комнату, модератор - убрать
// нарушителя), остальное - ErrRoomBlocked. Раньше блокировка проверялась только в
// /join, и в заблокированную комнату писали и приглашали.
func (s *RoomService) appendEvent(ctx context.Context, event Event, txn *TransactionKey) (Event, bool, error) {
	if err := s.checkRoomBlock(ctx, event); err != nil {
		return Event{}, false, err
	}

	return s.store.AppendEvent(ctx, event, txn)
}

// checkRoomBlock отказывает в записи события в заблокированную комнату.
func (s *RoomService) checkRoomBlock(ctx context.Context, event Event) error {
	checker, ok := s.store.(RoomBlockChecker)
	if !ok {
		return nil
	}

	if bypass, _ := ctx.Value(blockBypassKey{}).(bool); bypass {
		return nil
	}

	if allowedInBlockedRoom(event) {
		return nil
	}

	blocked, err := checker.RoomBlocked(ctx, event.RoomID)
	if err != nil {
		return err
	}

	if blocked {
		return ErrRoomBlocked
	}

	return nil
}

// allowedInBlockedRoom - membership leave (выход, kick) и ban.
func allowedInBlockedRoom(event Event) bool {
	if event.Type != "m.room.member" || event.StateKey == nil {
		return false
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(event.Content, &content); err != nil {
		return false
	}

	return content.Membership == "leave" || content.Membership == "ban"
}
