package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/store"
)

// remoteInviteError - приглашение не доставлено на сервер приглашенного:
// клиенту уходит ошибка, событие не записывается.
type remoteInviteError struct {
	status  int
	errcode string
	err     error
}

func (e *remoteInviteError) Error() string { return e.err.Error() }

func (e *remoteInviteError) Unwrap() error { return e.err }

// writeRemoteInviteError отвечает клиенту ошибкой доставки приглашения.
func writeRemoteInviteError(w http.ResponseWriter, err *remoteInviteError) {
	writeMatrixError(w, err.status, err.errcode, "Remote server rejected the invite")
}

// inviteFailed - ошибка доставки: отказ сервера (4xx) - 403, иначе (сеть, 5xx,
// неверный ответ) - 502, как Synapse («Can't connect to server»).
func inviteFailed(status int, err error) *remoteInviteError {
	if status >= 400 && status < 500 {
		return &remoteInviteError{status: http.StatusForbidden, errcode: ErrCodeForbidden, err: err}
	}

	return &remoteInviteError{status: http.StatusBadGateway, errcode: ErrCodeUnknown, err: err}
}

// inviteRemoteUser приглашает пользователя другого сервера: событие пишется
// только после успешного PUT /invite на его сервер.
func (s *Server) inviteRemoteUser(ctx context.Context, sender, roomID, target string) (string, error) {
	return s.rooms.InviteRemote(ctx, sender, roomID, target, func(ctx context.Context, event core.Event) (json.RawMessage, error) {
		return s.deliverRemoteInvite(ctx, event)
	})
}

// deliverRemoteInvite доставляет приглашение на сервер приглашённого пользователя
// через PUT /_matrix/federation/v2/invite/{roomId}/{eventId} и
// возвращает PDU с его подписью (как Synapse: event.signatures.update).
//
// Общий /send здесь не годится: у получателя ещё нет комнаты, поэтому invite —
// отдельный федеративный флоу; запасного пути через /send нет - там invite
// терялся. Сервер приглашенного получателем событий комнаты не становится:
// события ему уходят после входа его участника (joined-серверы).
func (s *Server) deliverRemoteInvite(ctx context.Context, event core.Event) (json.RawMessage, error) {
	if s.federationTransport == nil {
		return nil, inviteFailed(0, fmt.Errorf("api: federation transport is not configured"))
	}

	destination := domainOfID(stringOr(event.StateKey))
	if destination == "" || destination == s.cfg.Matrix.ServerName {
		return nil, fmt.Errorf("api: invite of %q is not remote: %w", stringOr(event.StateKey), core.ErrBadJSON)
	}

	// Исходный PDU подписан при создании, и его event_id - reference hash того же
	// JSON. Без ключа сервера PDU нет: неподписанный invite не уходит.
	if len(event.PDU) == 0 {
		return nil, inviteFailed(0, fmt.Errorf("api: invite %s has no signed PDU (no federation key)", event.EventID))
	}

	// invite_room_state — «раздетое» состояние комнаты для пира, у которого её ещё нет.
	// Synapse отвечает 403, если для неизвестной комнаты он пуст.
	inviteState, err := s.inviteRoomState(ctx, event.RoomID)
	if err != nil {
		return nil, err
	}

	// Комната v12: invite_room_state - полные PDU (MSC4311, спека v1.18: SHOULD
	// для v12; Synapse 1.165 проверяет их подписи и create).
	var roomState any = inviteState
	if s.roomVersionOf(ctx, event.RoomID) >= core.HashRoomIDVersion {
		if roomState, err = s.inviteRoomStatePDUs(ctx, inviteState); err != nil {
			return nil, err
		}
	}

	// Спека (и Synapse) ждёт {room_version, event}, а не «голое» событие: получатель
	// выбирает правила комнаты по версии и проверяет подпись отправителя.
	body, err := json.Marshal(map[string]any{
		"room_version":      s.roomVersion(ctx, event.RoomID),
		"event":             event.PDU,
		"invite_room_state": roomState,
	})
	if err != nil {
		return nil, fmt.Errorf("api: marshal invite request: %w", err)
	}

	// Тело приглашения не логируется: это содержимое события.
	s.log.DebugContext(ctx, "api: outgoing invite", "room_id", event.RoomID, "body_bytes", len(body))

	path := fmt.Sprintf("/_matrix/federation/v2/invite/%s/%s", url.PathEscape(event.RoomID), url.PathEscape(event.EventID))

	response, status, err := s.federationTransport.Send(ctx, destination, http.MethodPut, path, body)
	if err != nil {
		return nil, inviteFailed(status, fmt.Errorf("api: send invite to %s: %w", destination, err))
	}

	if status/100 != 2 {
		return nil, inviteFailed(status, fmt.Errorf("api: %s rejected invite: HTTP %d", destination, status))
	}

	signed, err := s.mergeInviteSignature(ctx, event, destination, response)
	if err != nil {
		return nil, inviteFailed(0, err)
	}

	return signed, nil
}

// mergeInviteSignature берет из ответа /invite подпись сервера приглашенного и
// добавляет ее к нашему PDU. Ответ должен быть тем же событием (тот же
// event_id), подпись - верной.
func (s *Server) mergeInviteSignature(ctx context.Context, event core.Event, destination string, response []byte) (json.RawMessage, error) {
	var generic any
	if err := json.Unmarshal(response, &generic); err != nil {
		return nil, fmt.Errorf("api: invite response of %s: %w", destination, err)
	}

	// v1: [200, {"event": ...}], v2: {"event": ...}.
	if array, ok := generic.([]any); ok && len(array) == 2 {
		generic = array[1]
	}

	wrapper, _ := generic.(map[string]any)
	returned, _ := wrapper["event"].(map[string]any)

	if returned == nil {
		return nil, fmt.Errorf("api: invite response of %s without event", destination)
	}

	delete(returned, "event_id")

	version := s.roomVersionOf(ctx, event.RoomID)

	if id, err := eventjson.EventID(returned, version); err != nil || id != event.EventID {
		return nil, fmt.Errorf("api: %s returned another invite event (%s != %s)", destination, id, event.EventID)
	}

	theirs, _ := returned["signatures"].(map[string]any)
	if theirs[destination] == nil {
		return nil, fmt.Errorf("api: %s did not sign the invite", destination)
	}

	var ours map[string]any
	if err := json.Unmarshal(event.PDU, &ours); err != nil {
		return nil, fmt.Errorf("api: invite pdu: %w", err)
	}

	signatures, _ := ours["signatures"].(map[string]any)
	if signatures == nil {
		signatures = map[string]any{}
	}

	signatures[destination] = theirs[destination]
	ours["signatures"] = signatures

	if err := eventjson.VerifyVersioned(withoutEventID(ours), destination, version, s.federationKeyLookup(ctx)); err != nil {
		return nil, fmt.Errorf("api: invite signature of %s: %w", destination, err)
	}

	return core.MarshalPDU(ours)
}

// stringOr - значение строки по указателю или пусто.
func stringOr(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

// inviteStateEvent — «раздетое» state-событие для invite_room_state.
type inviteStateEvent struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Sender   string          `json:"sender"`
	Content  json.RawMessage `json:"content"`
	// eventID - id события для полного PDU (в JSON не уходит).
	eventID string
}

// inviteRoomStatePDUs - те же события invite_room_state полными PDU.
func (s *Server) inviteRoomStatePDUs(ctx context.Context, stripped []inviteStateEvent) ([]json.RawMessage, error) {
	ids := make([]string, 0, len(stripped))
	for i := range stripped {
		ids = append(ids, stripped[i].eventID)
	}

	pdus, err := s.federationPDUs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("api: invite room state pdus: %w", err)
	}

	return pdus, nil
}

// inviteRoomState собирает состояние комнаты для приглашения: создание, правила
// входа, имя, аватар, топик, алиас и шифрование — как Synapse.
func (s *Server) inviteRoomState(ctx context.Context, roomID string) ([]inviteStateEvent, error) {
	events, err := store.NewRoomStore(s.db).StateEvents(ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("api: invite room state: %w", err)
	}

	allowed := map[string]struct{}{
		"m.room.create": {}, "m.room.join_rules": {}, "m.room.name": {},
		"m.room.avatar": {}, "m.room.topic": {}, "m.room.canonical_alias": {},
		"m.room.encryption": {},
	}

	out := make([]inviteStateEvent, 0, len(events))

	// Synapse падает 500 на invite, если stripped-state событие неполное: нужен
	// непустой content-объект и непустой sender. Такие события пропускаем, а общий
	// размер ограничиваем — огромный invite_room_state Synapse тоже не примет.
	const maxStateBytes = 64 << 10

	total := 0

	for i := range events {
		ev := events[i]
		if _, ok := allowed[ev.Type]; !ok || ev.StateKey == nil {
			continue
		}

		var content map[string]any
		if len(ev.Content) == 0 || json.Unmarshal(ev.Content, &content) != nil || content == nil {
			s.log.WarnContext(ctx, "api: invite_room_state skips event without content object",
				"room_id", roomID, "type", ev.Type)

			continue
		}

		if strings.TrimSpace(ev.Sender) == "" {
			continue
		}

		if total+len(ev.Content) > maxStateBytes {
			s.log.WarnContext(ctx, "api: invite_room_state truncated", "room_id", roomID, "limit", maxStateBytes)

			break
		}

		total += len(ev.Content)

		out = append(out, inviteStateEvent{
			Type: ev.Type, StateKey: *ev.StateKey, Sender: ev.Sender,
			Content: ev.Content, eventID: ev.EventID,
		})
	}

	return out, nil
}

// roomVersion возвращает версию комнаты для тела invite-запроса.
func (s *Server) roomVersion(ctx context.Context, roomID string) string {
	var version string

	if err := s.db.Pool().QueryRow(ctx, "SELECT room_version FROM rooms WHERE room_id = $1", roomID).
		Scan(&version); err != nil || version == "" {
		return "10"
	}

	return version
}

// inviteDestination — домен приглашённого пользователя (для fan-out).
func inviteDestination(userID string) string { return domainOfID(userID) }
