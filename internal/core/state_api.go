package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/aisa-it/syngo/internal/federation/auth"
)

// ErrNotAllowed — недостаточно прав на изменение состояния (403 M_FORBIDDEN).
var ErrNotAllowed = errors.New("core: not allowed")

// ErrInvalidAliasContent — некорректный m.room.canonical_alias (400 M_INVALID_PARAM).
var ErrInvalidAliasContent = errors.New("core: invalid canonical alias")

// ErrBadAlias - синтаксически верный алиас из m.room.canonical_alias не существует
// или указывает на другую комнату (400 M_BAD_ALIAS, как в Synapse).
var ErrBadAlias = errors.New("core: alias does not point to the room")

// SendState пишет state-событие с проверкой power levels и auth-правил.
func (s *RoomService) SendState(
	ctx context.Context,
	sender, deviceID, roomID, eventType, stateKey, txnID string,
	content json.RawMessage,
) (eventID string, duplicate bool, err error) {
	if err := validateEvent(eventType, content); err != nil {
		return "", false, err
	}

	if err := s.requireJoined(ctx, roomID, sender); err != nil {
		return "", false, err
	}

	// m.room.create бывает только первым событием комнаты: позже его подмена
	// ломает auth chain всех следующих событий.
	if eventType == "m.room.create" {
		return "", false, fmt.Errorf("%w: m.room.create cannot be replaced", ErrNotAllowed)
	}

	// canonical_alias ссылается на существующие алиасы этой же комнаты.
	if eventType == "m.room.canonical_alias" {
		if err := s.validateCanonicalAlias(ctx, roomID, content); err != nil {
			return "", false, err
		}
	}

	// Для событий не о членстве auth rules хватает среза состояния;
	// m.room.member смотрит еще на цель, приглашения третьих лиц и т.п.
	var stateEvents []Event

	if eventType == "m.room.member" {
		stateEvents, err = s.store.StateEvents(ctx, roomID)
	} else {
		stateEvents, err = s.store.AuthStateEvents(ctx, roomID, []string{sender})
	}

	if err != nil {
		return "", false, err
	}

	key := stateKey

	if err := s.authorizeStateEvent(sender, roomID, eventType, &key, content, stateEvents); err != nil {
		return "", false, err
	}

	// То же значение от того же отправителя, что в текущем состоянии, - нового
	// события нет, возвращается текущее (Synapse deduplicate_state_event,
	// Complement «Setting state twice is idempotent»). Повтор после
	// смены значения (A -> B -> A) - по-прежнему новое событие.
	// Блокировка комнаты проверяется раньше: повтор в заблокированной комнате -
	// отказ, а не «уже есть».
	if current, curErr := s.store.CurrentStateEvent(ctx, roomID, eventType, key); curErr == nil &&
		current.Sender == sender && sameJSON(current.Content, content) {
		if err := s.checkRoomBlock(ctx, Event{RoomID: roomID, Type: eventType, StateKey: &key, Content: content}); err != nil {
			return "", false, err
		}

		return current.EventID, true, nil
	}

	prev, err := s.prevEvents(ctx, roomID)
	if err != nil {
		return "", false, err
	}

	event := s.newEvent(ctx, roomID, sender, eventType, &key, content, prev, s.now().UnixMilli(), s.roomVersion(ctx, roomID))

	if err := checkEventSize(event); err != nil {
		return "", false, err
	}

	// PUT /state не идемпотентен по телу: повтор того же значения после смены -
	// новое событие (раньше A -> B -> A оставляло B, повторный kick глотался).
	// Ключ транзакции - только при явном txnId вызывающего.
	var txn *TransactionKey
	if txnID != "" {
		txn = &TransactionKey{UserID: sender, DeviceID: deviceID, Endpoint: "state:" + roomID, TxnID: txnID}
	}

	written, duplicate, err := s.appendEvent(ctx, event, txn)
	if err != nil {
		return "", false, err
	}

	return written.EventID, duplicate, nil
}

// sameJSON - два JSON равны по значению (порядок ключей не важен).
func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}

	return reflect.DeepEqual(left, right)
}

// validateCanonicalAlias проверяет content m.room.canonical_alias: каждый алиас
// (alias и alt_aliases) должен быть валидным и указывать на эту комнату.
func (s *RoomService) validateCanonicalAlias(ctx context.Context, roomID string, content []byte) error {
	var payload struct {
		Alias      string   `json:"alias"`
		AltAliases []string `json:"alt_aliases"`
	}

	if err := json.Unmarshal(content, &payload); err != nil {
		return fmt.Errorf("%w: malformed content", ErrInvalidAliasContent)
	}

	aliases := make([]string, 0, len(payload.AltAliases)+1)

	if payload.Alias != "" {
		aliases = append(aliases, payload.Alias)
	}

	aliases = append(aliases, payload.AltAliases...)

	for _, alias := range aliases {
		if !strings.HasPrefix(alias, "#") || !strings.Contains(alias, ":") {
			return fmt.Errorf("%w: %q is not a valid room alias", ErrInvalidAliasContent, alias)
		}

		aliasRoomID, err := s.store.ResolveAlias(ctx, alias)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("%w: %q does not exist", ErrBadAlias, alias)
			}

			return err
		}

		if aliasRoomID != roomID {
			return fmt.Errorf("%w: %q points to another room", ErrBadAlias, alias)
		}
	}

	return nil
}

// SendMembership меняет membership участника (kick/ban/unban/invite).
func (s *RoomService) SendMembership(
	ctx context.Context,
	sender, deviceID, roomID, target, membership, reason, txnID string,
) (eventID string, duplicate bool, err error) {
	if target == "" || !strings.HasPrefix(target, "@") {
		return "", false, fmt.Errorf("core: invalid user_id %q: %w", target, ErrBadJSON)
	}

	switch membership {
	case "ban", "leave":
	default:
		return "", false, fmt.Errorf("core: unsupported membership %q: %w", membership, ErrBadJSON)
	}

	// Нельзя банить/выгонять себя и равного по уровню (как в Synapse room_batch).
	stateEvents, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return "", false, err
	}

	state, err := auth.BuildState(toAuthEvents(stateEvents))
	if err != nil {
		return "", false, err
	}

	senderLevel := auth.Level(state, sender)
	targetLevel := auth.Level(state, target)

	if target == sender {
		return "", false, fmt.Errorf("%w: cannot %s yourself", ErrNotAllowed, membership)
	}

	if membership == "ban" {
		if senderLevel < banLevelOf(state) {
			return "", false, fmt.Errorf("%w: ban level %d не хватает", ErrNotAllowed, senderLevel)
		}

		if targetLevel >= senderLevel {
			return "", false, fmt.Errorf("%w: нельзя банить равного по уровню", ErrNotAllowed)
		}
	}

	if membership == "leave" {
		// kick — цель должна быть в комнате; unban — цель в бане (нужен уровень ban).
		switch state.Members[target] {
		case "join":
			if targetLevel >= senderLevel {
				return "", false, fmt.Errorf("%w: нельзя выгонять равного по уровню", ErrNotAllowed)
			}
		case "ban":
			if senderLevel < banLevelOf(state) {
				return "", false, fmt.Errorf("%w: ban level %d не хватает для unban", ErrNotAllowed, senderLevel)
			}
		default:
			return "", false, fmt.Errorf("%w: user is not in the room", ErrNotAllowed)
		}
	}

	body := map[string]any{"membership": membership}
	if reason != "" {
		body["reason"] = reason
	}

	content, err := json.Marshal(body)
	if err != nil {
		return "", false, err
	}

	return s.SendState(ctx, sender, deviceID, roomID, "m.room.member", target, txnID, content)
}

// StateEvents возвращает материализованное состояние комнаты (для клиента).
//
// Вышедший или забаненный (и не забывший комнату) видит состояние на момент
// выхода, как в Synapse (SPEC-216, Complement TestLeftRoomFixture).
func (s *RoomService) StateEvents(ctx context.Context, userID, roomID string) ([]Event, error) {
	bound, err := s.readBound(ctx, roomID, userID)
	if err != nil {
		return nil, err
	}

	if bound > 0 {
		return s.store.StateAtStream(ctx, roomID, bound)
	}

	return s.store.StateEvents(ctx, roomID)
}

// CurrentState возвращает текущее состояние комнаты без проверки доступа:
// доступ решает вызывающий (обход пространства /hierarchy проверяет его по
// join_rules, history_visibility и членству). Неизвестная комната -
// пустое состояние.
func (s *RoomService) CurrentState(ctx context.Context, roomID string) ([]Event, error) {
	return s.store.StateEvents(ctx, roomID)
}

// readBound - до какой позиции пользователь читает комнату: 0 - участник,
// без ограничения; позиция выхода - вышедший или забаненный, не забывший
// комнату. Остальным - ErrForbidden.
func (s *RoomService) readBound(ctx context.Context, roomID, userID string) (int64, error) {
	info, err := s.store.MembershipInfo(ctx, roomID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, ErrForbidden
		}

		return 0, err
	}

	switch {
	case info.Membership == "join":
		return 0, nil
	case (info.Membership == "leave" || info.Membership == "ban") && !info.Forgotten && info.Stream > 0:
		return info.Stream, nil
	}

	return 0, ErrForbidden
}

// StateEvent возвращает конкретное state-событие комнаты.
func (s *RoomService) StateEvent(ctx context.Context, userID, roomID, eventType, stateKey string) (Event, error) {
	events, err := s.StateEvents(ctx, userID, roomID)
	if err != nil {
		return Event{}, err
	}

	for i := range events {
		ev := events[i]
		if ev.Type != eventType || ev.StateKey == nil || *ev.StateKey != stateKey {
			continue
		}

		return ev, nil
	}

	return Event{}, ErrNotFound
}

// Redact скрывает content события. Автор события может всегда,
// остальным нужен уровень redact из power_levels; само событие m.room.redaction
// требует уровня отправки этого типа (events["m.room.redaction"] или
// events_default), как _can_send_event в Synapse. Правила одинаковы
// для v10 и v11: доменная проверка по event_id была только в v1/v2.
//
// Пометка цели и вырезание ее content выполняются в транзакции записи события
// (store.AppendEvent), а не отдельным шагом: повтор с тем же txn после сбоя не
// оставляет цель видимой.
func (s *RoomService) Redact(
	ctx context.Context,
	sender, deviceID, roomID, targetEventID, reason, txnID string,
) (eventID string, duplicate bool, err error) {
	if err := s.requireJoined(ctx, roomID, sender); err != nil {
		return "", false, err
	}

	target, err := s.store.EventByID(ctx, roomID, targetEventID)
	if err != nil {
		return "", false, err
	}

	stateEvents, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return "", false, err
	}

	state, err := auth.BuildState(toAuthEvents(stateEvents))
	if err != nil {
		return "", false, err
	}

	level := auth.Level(state, sender)

	if level < auth.SendLevel(state, "m.room.redaction", false) {
		return "", false, fmt.Errorf("%w: недостаточно прав на отправку m.room.redaction", ErrNotAllowed)
	}

	if !IsRedactionAllowedBy(sender, target.Sender) && level < redactLevelOf(state) {
		return "", false, fmt.Errorf("%w: недостаточно прав на redaction", ErrNotAllowed)
	}

	prev, err := s.prevEvents(ctx, roomID)
	if err != nil {
		return "", false, err
	}

	body := map[string]any{"redacts": targetEventID}
	if reason != "" {
		body["reason"] = reason
	}

	content, err := json.Marshal(body)
	if err != nil {
		return "", false, err
	}

	event := s.newEvent(ctx, roomID, sender, "m.room.redaction", nil, content, prev, s.now().UnixMilli(), s.roomVersion(ctx, roomID))
	txn := &TransactionKey{UserID: sender, DeviceID: deviceID, Endpoint: "redact:" + roomID, TxnID: txnID}

	written, duplicate, err := s.appendEvent(ctx, event, txn)
	if err != nil {
		return "", false, err
	}

	return written.EventID, duplicate, nil
}

// authorizeStateEvent проверяет auth-правила и power level для state-события.
func (s *RoomService) authorizeStateEvent(
	sender, roomID, eventType string, stateKey *string, content []byte, stateEvents []Event,
) error {
	state, err := auth.BuildState(toAuthEvents(stateEvents))
	if err != nil {
		return err
	}

	candidate := auth.Event{
		EventID: "$check", RoomID: roomID, Type: eventType, Sender: sender,
		StateKey: stateKey, Content: content,
	}

	if err := auth.Check(candidate, state); err != nil {
		if errors.Is(err, auth.ErrUnauthorized) || errors.Is(err, auth.ErrBadEvent) {
			return fmt.Errorf("%w: %w", ErrNotAllowed, err)
		}

		return err
	}

	// power_levels, с которыми состояние комнаты не строится (строковые уровни и
	// т.п.), ломают все последующие события комнаты - отклоняем до записи.
	if eventType == "m.room.power_levels" {
		if _, err := auth.BuildState(append(toAuthEvents(stateEvents), candidate)); err != nil {
			return fmt.Errorf("core: invalid power_levels: %w: %w", ErrBadJSON, err)
		}
	}

	return nil
}

// toAuthEvents переводит события комнаты в тип auth.
func toAuthEvents(events []Event) []auth.Event {
	out := make([]auth.Event, 0, len(events))

	for i := range events {
		ev := events[i]
		out = append(out, auth.Event{
			EventID: ev.EventID, RoomID: ev.RoomID, Type: ev.Type, Sender: ev.Sender,
			StateKey: ev.StateKey, Content: ev.Content, Prev: ev.PrevEvents, Depth: ev.Depth,
		})
	}

	return out
}

// redactLevelOf — уровень redact из power_levels (по умолчанию 50).
func redactLevelOf(state auth.State) int {
	level, _ := auth.PowerLevelFor(state, "redact")

	return level
}

// banLevelOf — уровень ban (по умолчанию 50).
func banLevelOf(state auth.State) int {
	level, _ := auth.PowerLevelFor(state, "ban")

	return level
}
