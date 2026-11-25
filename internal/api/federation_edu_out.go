package api

import (
	"context"
	"encoding/json"

	"github.com/aisa-it/syngo/internal/core"
)

// Исходящие EDU: typing, receipts, presence, изменения device list
// и ключей кросс-подписи наших пользователей ставятся в outbox серверам,
// которые делят комнату с пользователем (typing и receipts - серверам
// комнаты). Раньше исходящих EDU не было: собеседник на другом сервере не видел
// набора и прочтения, а главное - не узнавал о новых устройствах и ключах и
// не мог зашифровать для них сообщение. Сбой постановки не ломает запрос
// клиента: действие уже выполнено локально.

// federationEDUsEnabled - исходящие EDU ставятся только при включенной федерации.
func (s *Server) federationEDUsEnabled() bool {
	return s.db != nil && s.cfg.Federation.Enabled
}

// enqueueEDU ставит EDU получателям.
func (s *Server) enqueueEDU(ctx context.Context, destinations []string, eduType string, content any) {
	if len(destinations) == 0 {
		return
	}

	raw, err := json.Marshal(map[string]any{"edu_type": eduType, "content": content})
	if err == nil {
		err = s.db.EnqueueEDU(ctx, destinations, raw)
	}

	if err != nil {
		s.log.WarnContext(ctx, "api: enqueue edu failed", "edu_type", eduType, "error", err)
	}
}

// roomDestinations - серверы комнаты (joined-участники) без своего.
func (s *Server) roomDestinations(ctx context.Context, roomID string) []string {
	destinations, err := s.db.DestinationsForRoom(ctx, roomID, s.cfg.Matrix.ServerName)
	if err != nil {
		s.log.WarnContext(ctx, "api: room destinations failed", "room_id", roomID, "error", err)

		return nil
	}

	return destinations
}

// userDestinations - серверы, делящие комнату с пользователем.
func (s *Server) userDestinations(ctx context.Context, userID string) []string {
	destinations, err := s.db.HostsSharingRoomWith(ctx, userID, s.cfg.Matrix.ServerName)
	if err != nil {
		s.log.WarnContext(ctx, "api: user destinations failed", "user_id", userID, "error", err)

		return nil
	}

	return destinations
}

// sendTypingEDU - m.typing серверам комнаты.
func (s *Server) sendTypingEDU(ctx context.Context, roomID, userID string, typing bool) {
	if !s.federationEDUsEnabled() {
		return
	}

	s.enqueueEDU(ctx, s.roomDestinations(ctx, roomID), "m.typing",
		map[string]any{"room_id": roomID, "user_id": userID, "typing": typing})
}

// sendReceiptEDU - m.receipt (m.read) серверам комнаты; приватные отметки не уходят.
func (s *Server) sendReceiptEDU(ctx context.Context, roomID, userID, eventID, threadID string) {
	if !s.federationEDUsEnabled() {
		return
	}

	data := map[string]any{"ts": s.ephemeral.Now().UnixMilli()}
	if threadID != "" {
		data["thread_id"] = threadID
	}

	s.enqueueEDU(ctx, s.roomDestinations(ctx, roomID), "m.receipt", map[string]any{
		roomID: map[string]any{core.ReceiptRead: map[string]any{
			userID: map[string]any{"event_ids": []string{eventID}, "data": data},
		}},
	})
}

// sendPresenceEDU - m.presence нашего пользователя серверам общих комнат.
func (s *Server) sendPresenceEDU(ctx context.Context, userID string) {
	if !s.federationEDUsEnabled() || domainOfID(userID) != s.cfg.Matrix.ServerName {
		return
	}

	view := presenceView(s.ephemeral.Presence(userID), s.ephemeral.Now().UnixMilli())

	content, ok := view.Content.(map[string]any)
	if !ok {
		return
	}

	push := map[string]any{"user_id": userID}
	for key, value := range content {
		push[key] = value
	}

	s.enqueueEDU(ctx, s.userDestinations(ctx, userID), "m.presence", map[string]any{"push": []any{push}})
}

// sendDeviceListUpdate - m.device_list_update об устройствах пользователя. На
// каждое устройство - свой stream_id, prev_id - предыдущий stream_id
// пользователя: получатель, пропустивший изменение, видит дыру и делает ресинк
// через /user/devices. stream_id растет и без получателей - его отдает /user/devices.
func (s *Server) sendDeviceListUpdate(ctx context.Context, userID string, deviceIDs []string, deleted bool) {
	if !s.federationEDUsEnabled() || len(deviceIDs) == 0 {
		return
	}

	var keys map[string]map[string]json.RawMessage

	if !deleted && s.keys != nil {
		queried, err := s.keys.Query(ctx, map[string][]string{userID: deviceIDs})
		if err != nil {
			s.log.WarnContext(ctx, "api: device list update keys failed", "user_id", userID, "error", err)
		}

		keys = queried
		if keys == nil {
			keys = map[string]map[string]json.RawMessage{}
		}

		// Подписи self_signing подмешиваются в ключи устройства, как в /keys/query.
		if _, err := s.crossSigningQuery(ctx, "", keys, []string{userID}); err != nil {
			s.log.WarnContext(ctx, "api: device list update signatures failed", "user_id", userID, "error", err)
		}
	}

	destinations := s.userDestinations(ctx, userID)

	for _, deviceID := range deviceIDs {
		stream, prev, err := s.db.NextDeviceListStream(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "api: device list stream failed", "user_id", userID, "error", err)

			return
		}

		prevIDs := []int64{}
		if prev > 0 {
			prevIDs = append(prevIDs, prev)
		}

		content := map[string]any{
			"user_id": userID, "device_id": deviceID, "stream_id": stream, "prev_id": prevIDs, "deleted": deleted,
		}

		if key, ok := keys[userID][deviceID]; ok {
			content["keys"] = key
		}

		s.enqueueEDU(ctx, destinations, "m.device_list_update", content)
	}
}

// sendSigningKeyUpdate - m.signing_key_update: master и self_signing пользователя.
func (s *Server) sendSigningKeyUpdate(ctx context.Context, userID string) {
	if !s.federationEDUsEnabled() || s.crossSigning == nil {
		return
	}

	cross, err := s.crossSigningQuery(ctx, "", map[string]map[string]json.RawMessage{}, []string{userID})
	if err != nil {
		s.log.WarnContext(ctx, "api: signing key update failed", "user_id", userID, "error", err)

		return
	}

	content := map[string]any{"user_id": userID}

	if key, ok := cross.Master[userID]; ok {
		content["master_key"] = key
	}

	if key, ok := cross.SelfSigning[userID]; ok {
		content["self_signing_key"] = key
	}

	if len(content) == 1 {
		return
	}

	s.enqueueEDU(ctx, s.userDestinations(ctx, userID), "m.signing_key_update", content)
}

// sendSignaturesUpdate - после /keys/signatures/upload: подписанные свои
// устройства - m.device_list_update, подпись своего мастер-ключа -
// m.signing_key_update. Подписи чужих ключей (user_signing) видны только
// подписавшему и по федерации не уходят.
func (s *Server) sendSignaturesUpdate(ctx context.Context, userID string, body map[string]map[string]json.RawMessage) {
	if !s.federationEDUsEnabled() {
		return
	}

	own := body[userID]
	if len(own) == 0 {
		return
	}

	devices, err := s.db.LocalDevices(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "api: signatures update devices failed", "user_id", userID, "error", err)

		return
	}

	var (
		signedDevices []string
		signedMaster  bool
	)

	for keyID := range own {
		if _, ok := devices[keyID]; ok {
			signedDevices = append(signedDevices, keyID)
		} else {
			signedMaster = true
		}
	}

	s.sendDeviceListUpdate(ctx, userID, signedDevices, false)

	if signedMaster {
		s.sendSigningKeyUpdate(ctx, userID)
	}
}
