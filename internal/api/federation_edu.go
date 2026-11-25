package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// Входящие EDU транзакции /send. Раньше они игнорировались, и
// typing, receipts, presence, to-device и изменения устройств пользователей
// других серверов до наших клиентов не доходили: шифрованная комната с
// участником другого сервера не работала.
//
// Как в Synapse (FederationServer._handle_edus_in_txn): ошибка в одном EDU не
// отвергает транзакцию, EDU от имени пользователя чужого сервера (sender не
// принадлежит origin) отбрасывается.

// federationEDU - EDU в транзакции.
type federationEDU struct {
	Type    string          `json:"edu_type"`
	Content json.RawMessage `json:"content"`
}

// maxEDUsPerTxn - предел EDU в транзакции (спецификация /send: не больше 100).
const maxEDUsPerTxn = 100

// eduTimeout - предел обработки EDU одной транзакции (ресинк устройств ходит к origin).
const eduTimeout = 30 * time.Second

// applyFederationEDUs обрабатывает EDU транзакции от origin.
func (s *Server) applyFederationEDUs(ctx context.Context, origin string, raws []json.RawMessage) {
	if len(raws) == 0 {
		return
	}

	if len(raws) > maxEDUsPerTxn {
		s.log.WarnContext(ctx, "api: too many edus in transaction", "origin", origin, "count", len(raws))
		raws = raws[:maxEDUsPerTxn]
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eduTimeout)
	defer cancel()

	for _, raw := range raws {
		var edu federationEDU
		if json.Unmarshal(raw, &edu) != nil || edu.Type == "" {
			s.log.WarnContext(ctx, "api: malformed edu", "origin", origin)

			continue
		}

		var err error

		switch edu.Type {
		case "m.typing":
			err = s.applyTypingEDU(ctx, origin, edu.Content)
		case "m.receipt":
			err = s.applyReceiptEDU(ctx, origin, edu.Content)
		case "m.presence":
			err = s.applyPresenceEDU(ctx, origin, edu.Content)
		case "m.direct_to_device":
			err = s.applyToDeviceEDU(ctx, origin, edu.Content)
		case "m.device_list_update":
			err = s.applyDeviceListEDU(ctx, origin, edu.Content)
		case "m.signing_key_update":
			err = s.applySigningKeyEDU(ctx, origin, edu.Content)
		default:
			s.log.DebugContext(ctx, "api: unsupported edu", "origin", origin, "edu_type", edu.Type)

			continue
		}

		if err != nil {
			s.log.WarnContext(ctx, "api: edu rejected", "origin", origin, "edu_type", edu.Type, "error", err)
		}
	}
}

// eduRoomACL - EDU комнаты от сервера, запрещенного m.room.server_acl,
// отбрасывается (Synapse: typing и receipts проверяют ACL).
func (s *Server) eduRoomACL(ctx context.Context, roomID, origin string) error {
	if s.db == nil {
		return nil
	}

	allowed, err := s.serverACLAllowed(ctx, roomID, origin)
	if err != nil {
		return err
	}

	if !allowed {
		return fmt.Errorf("edu: server %s is banned from room %s", origin, roomID)
	}

	return nil
}

// errEDUOrigin - EDU от имени пользователя другого сервера.
type errEDUOrigin struct{ userID, origin string }

func (e errEDUOrigin) Error() string {
	return "edu user " + e.userID + " does not belong to " + e.origin
}

// eduUserAllowed - пользователь EDU принадлежит origin.
func eduUserAllowed(userID, origin string) error {
	if userID == "" || domainOfID(userID) != origin {
		return errEDUOrigin{userID: userID, origin: origin}
	}

	return nil
}

// errNotJoined - пользователь EDU не в комнате.
type errNotJoined struct{ userID, roomID string }

func (e errNotJoined) Error() string { return e.userID + " is not joined to " + e.roomID }

// requireJoined - удаленный пользователь joined в комнате (typing и receipts
// принимаются только от участников).
func (s *Server) requireJoined(ctx context.Context, roomID, userID string) error {
	joined, err := s.db.UserJoined(ctx, roomID, userID)
	if err != nil {
		return err
	}

	if !joined {
		return errNotJoined{userID: userID, roomID: roomID}
	}

	return nil
}

// applyTypingEDU - m.typing: {room_id, user_id, typing}.
func (s *Server) applyTypingEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var typing struct {
		RoomID string `json:"room_id"`
		UserID string `json:"user_id"`
		Typing bool   `json:"typing"`
	}

	if err := json.Unmarshal(content, &typing); err != nil {
		return err
	}

	if err := eduUserAllowed(typing.UserID, origin); err != nil {
		return err
	}

	if err := s.eduRoomACL(ctx, typing.RoomID, origin); err != nil {
		return err
	}

	if err := s.requireJoined(ctx, typing.RoomID, typing.UserID); err != nil {
		return err
	}

	s.ephemeral.SetTyping(typing.RoomID, typing.UserID, typing.Typing)
	s.notifySync()

	return nil
}

// receiptEDUData - отметка одного пользователя в m.receipt.
type receiptEDUData struct {
	EventIDs []string `json:"event_ids"`
	Data     struct {
		TS       int64  `json:"ts"`
		ThreadID string `json:"thread_id"`
	} `json:"data"`
}

// applyReceiptEDU - m.receipt: {room_id: {m.read: {user_id: {event_ids, data}}}}.
// Приватные отметки по федерации не ходят: принимается только m.read.
func (s *Server) applyReceiptEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var rooms map[string]map[string]map[string]receiptEDUData
	if err := json.Unmarshal(content, &rooms); err != nil {
		return err
	}

	applied := false

	for roomID, byType := range rooms {
		if err := s.eduRoomACL(ctx, roomID, origin); err != nil {
			s.log.WarnContext(ctx, "api: receipt edu skipped", "error", err)

			continue
		}

		for userID, data := range byType[core.ReceiptRead] {
			if eduUserAllowed(userID, origin) != nil || len(data.EventIDs) == 0 {
				continue
			}

			if err := s.requireJoined(ctx, roomID, userID); err != nil {
				s.log.WarnContext(ctx, "api: receipt edu skipped", "error", err)

				continue
			}

			ts := data.Data.TS
			if ts <= 0 {
				ts = s.ephemeral.Now().UnixMilli()
			}

			s.readMarkers.ApplyRemote(ctx, core.ReceiptEvent{
				RoomID: roomID, EventID: data.EventIDs[0], UserID: userID, Type: core.ReceiptRead,
				ThreadID: data.Data.ThreadID, TS: ts,
			})

			applied = true
		}
	}

	if applied {
		s.notifySync()
	}

	return nil
}

// applyPresenceEDU - m.presence: {push: [{user_id, presence, status_msg}]}.
func (s *Server) applyPresenceEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var presence struct {
		Push []struct {
			UserID    string `json:"user_id"`
			Presence  string `json:"presence"`
			StatusMsg string `json:"status_msg"`
		} `json:"push"`
	}

	if err := json.Unmarshal(content, &presence); err != nil {
		return err
	}

	for _, push := range presence.Push {
		if eduUserAllowed(push.UserID, origin) != nil || !core.ValidPresence(push.Presence) {
			continue
		}

		if s.ephemeral.SetPresence(push.UserID, push.Presence, push.StatusMsg) {
			s.notifyPresence(ctx, push.UserID)
		}
	}

	return nil
}

// applyToDeviceEDU - m.direct_to_device: {sender, type, message_id, messages}.
// Доставляются только нашим пользователям; повтор message_id отбрасывается.
func (s *Server) applyToDeviceEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var message struct {
		Sender    string                                `json:"sender"`
		Type      string                                `json:"type"`
		MessageID string                                `json:"message_id"`
		Messages  map[string]map[string]json.RawMessage `json:"messages"`
	}

	if err := json.Unmarshal(content, &message); err != nil {
		return err
	}

	if err := eduUserAllowed(message.Sender, origin); err != nil {
		return err
	}

	if message.Type == "" {
		return errMalformedEDU
	}

	targets := map[store.DeviceTarget]json.RawMessage{}
	recipients := map[string]struct{}{}

	for userID, byDevice := range message.Messages {
		if domainOfID(userID) != s.cfg.Matrix.ServerName {
			continue
		}

		for deviceID, body := range byDevice {
			targets[store.DeviceTarget{UserID: userID, DeviceID: deviceID}] = body
			recipients[userID] = struct{}{}
		}
	}

	if len(targets) == 0 {
		return nil
	}

	duplicate, err := s.db.DeliverFederatedToDevice(ctx, origin, message.MessageID, message.Sender, message.Type, targets)
	if err != nil || duplicate {
		return err
	}

	for userID := range recipients {
		s.notifySyncUser(userID)
	}

	return nil
}

// errMalformedEDU - в EDU нет обязательного поля.
var errMalformedEDU = errEDUField("required field is missing")

type errEDUField string

func (e errEDUField) Error() string { return string(e) }

// applyDeviceListEDU - m.device_list_update.
func (s *Server) applyDeviceListEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var update struct {
		UserID      string          `json:"user_id"`
		DeviceID    string          `json:"device_id"`
		DisplayName string          `json:"device_display_name"`
		StreamID    int64           `json:"stream_id"`
		PrevID      []int64         `json:"prev_id"`
		Deleted     bool            `json:"deleted"`
		Keys        json.RawMessage `json:"keys"`
	}

	if err := json.Unmarshal(content, &update); err != nil {
		return err
	}

	if err := eduUserAllowed(update.UserID, origin); err != nil {
		return err
	}

	if s.remoteKeys == nil {
		return nil
	}

	return s.remoteKeys.HandleDeviceListUpdate(ctx, origin, core.RemoteDeviceUpdate{
		UserID: update.UserID, DeviceID: update.DeviceID, DisplayName: update.DisplayName,
		StreamID: update.StreamID, PrevIDs: update.PrevID, Deleted: update.Deleted, Keys: update.Keys,
	})
}

// applySigningKeyEDU - m.signing_key_update: {user_id, master_key, self_signing_key}.
func (s *Server) applySigningKeyEDU(ctx context.Context, origin string, content json.RawMessage) error {
	var update struct {
		UserID         string          `json:"user_id"`
		MasterKey      json.RawMessage `json:"master_key"`
		SelfSigningKey json.RawMessage `json:"self_signing_key"`
	}

	if err := json.Unmarshal(content, &update); err != nil {
		return err
	}

	if err := eduUserAllowed(update.UserID, origin); err != nil {
		return err
	}

	if s.remoteKeys == nil {
		return nil
	}

	return s.remoteKeys.HandleSigningKeyUpdate(ctx, origin, update.UserID, update.MasterKey, update.SelfSigningKey)
}

// newFederationMessageID - message_id исходящего m.direct_to_device.
func newFederationMessageID() string {
	var buf [16]byte

	_, _ = rand.Read(buf[:])

	return hex.EncodeToString(buf[:])
}
