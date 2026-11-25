package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// leaveRemoteRoom - выход нашего пользователя из комнаты другого сервера:
// make_leave/send_leave через сервер комнаты (у комнаты v12 -
// через servers). Если удаленная
// сторона не ответила или отказала:
//   - приглашение отклоняется локально (out-of-band rejection, как
//     _generate_local_out_of_band_leave Synapse): пишется свой leave поверх
//     invite, клиент получает 200 - иначе от приглашения недоступного сервера
//     не избавиться;
//   - из комнаты, где мы состоим, выходим обычным локальным событием: состояние
//     комнаты у нас есть, событие уходит участникам через outbox (так Synapse
//     выходит всегда, когда его сервер в комнате).
//
// Раньше любая ошибка давала клиенту 502, и локальный leave не писался.
func (s *Server) leaveRemoteRoom(ctx context.Context, roomID, userID string, servers []string) error {
	membership, err := store.NewRoomStore(s.db).Membership(ctx, roomID, userID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}

	remoteErr := s.remoteJoiner.LeaveVia(ctx, roomID, userID, servers)
	if remoteErr == nil {
		return nil
	}

	s.log.WarnContext(ctx, "api: remote leave failed", "room_id", roomID, "membership", membership, "error", remoteErr)

	switch membership {
	case "invite":
		if err := s.rejectInviteLocally(ctx, roomID, userID); err != nil {
			return fmt.Errorf("%w (local rejection: %w)", remoteErr, err)
		}

		return nil
	case "join":
		if _, err := s.rooms.Leave(ctx, userID, roomID); err != nil {
			return fmt.Errorf("%w (local leave: %w)", remoteErr, err)
		}

		return nil
	default:
		return remoteErr
	}
}

// rejectInviteLocally пишет out-of-band leave: m.room.member leave
// пользователя поверх его invite (prev_events и auth_events - сам invite, depth
// на единицу больше), подписанный нашим ключом. В федерацию не уходит:
// сервер комнаты недоступен или отказал, а других серверов мы не знаем.
func (s *Server) rejectInviteLocally(ctx context.Context, roomID, userID string) error {
	invite, err := store.NewRoomStore(s.db).CurrentStateEvent(ctx, roomID, "m.room.member", userID)
	if err != nil {
		return err
	}

	if membershipOfContent(invite.Content) != "invite" {
		return fmt.Errorf("api: %s is not invited to %s: %w", userID, roomID, core.ErrForbidden)
	}

	version := s.roomVersionOf(ctx, roomID)
	content := json.RawMessage(`{"membership":"leave"}`)
	originTS := time.Now().UnixMilli()

	object := map[string]any{
		"type": "m.room.member", "room_id": roomID, "sender": userID, "state_key": userID,
		"content": content, "depth": invite.Depth + 1, "origin_server_ts": originTS,
		"prev_events": []string{invite.EventID}, "auth_events": []string{invite.EventID},
	}

	hash, err := signing.ContentHash(object)
	if err != nil {
		return fmt.Errorf("api: out-of-band leave hash: %w", err)
	}

	object["hashes"] = map[string]string{"sha256": hash}

	eventID, err := eventjson.EventID(object, version)
	if err != nil {
		return fmt.Errorf("api: out-of-band leave id: %w", err)
	}

	pdu := store.FedPDU{
		EventID: eventID, RoomID: roomID, Type: "m.room.member", Sender: userID, StateKey: &userID,
		Content: content, Prev: []string{invite.EventID}, Auth: []string{invite.EventID},
		Depth: invite.Depth + 1, OriginTS: originTS,
	}

	if s.federationKey != nil {
		signed, err := eventjson.SignVersioned(*s.federationKey, s.cfg.Matrix.ServerName, object, version)
		if err != nil {
			return fmt.Errorf("api: out-of-band leave sign: %w", err)
		}

		if pdu.PDU, err = core.MarshalPDU(signed); err != nil {
			return err
		}
	}

	return s.db.ApplyLocalMembershipChange(ctx, pdu)
}

// membershipOfContent - membership из content m.room.member.
func membershipOfContent(content json.RawMessage) string {
	var body struct {
		Membership string `json:"membership"`
	}

	if json.Unmarshal(content, &body) != nil {
		return ""
	}

	return body.Membership
}

// writeRemoteLeaveError - ответ клиенту, когда выйти не удалось.
func (s *Server) writeRemoteLeaveError(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrNotFound) {
		s.writeRoomError(w, err)

		return
	}

	writeMatrixError(w, http.StatusBadGateway, ErrCodeUnknown, "Remote server rejected leave")
}
