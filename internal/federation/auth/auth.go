// Package auth реализует минимальные auth rules Matrix для комнат v10/v11 (ADR-0007 §4)
// и v12: создание комнаты, членство, power levels. Проверка state
// resolution — отдельный блок (5), здесь только авторизация одного события по auth_events.
//
// Правила v12 (MSC4289, MSC4291): создатели - отправитель m.room.create и
// content.additional_creators, их уровень бесконечен и не записывается в power_levels;
// room_id - хеш m.room.create без серверной части, create не входит в auth_events
// (вызывающий кладет его в состояние сам, как room_state у локальных событий).
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Event — минимальное представление события для проверки.
type Event struct {
	EventID  string
	RoomID   string
	Type     string
	Sender   string
	StateKey *string
	Content  json.RawMessage
	Prev     []string
	Depth    int64
}

// Ошибки авторизации.
var (
	ErrUnauthorized = fmt.Errorf("auth: event is not authorized")
	ErrBadEvent     = fmt.Errorf("auth: malformed event")
)

// CreatorLevel - уровень создателя комнаты v12: бесконечный, выше любого значения
// power_levels (MSC4289).
const CreatorLevel = math.MaxInt

// powerLevels — разобранные m.room.power_levels.
type powerLevels struct {
	Users         map[string]int `json:"users"`
	UsersDefault  *int           `json:"users_default"`
	Events        map[string]int `json:"events"`
	EventsDefault *int           `json:"events_default"`
	StateDefault  *int           `json:"state_default"`
	Ban           *int           `json:"ban"`
	Kick          *int           `json:"kick"`
	Invite        *int           `json:"invite"`
	Redact        *int           `json:"redact"`
	// creators - создатели комнаты v12 с бесконечным уровнем (не из content).
	creators map[string]struct{}
}

// State — авторизационное состояние комнаты, собранное по auth_events.
type State struct {
	Create      *Event
	PowerLevels *powerLevels
	JoinRules   string
	// JoinRulesAllow — комнаты из allow-списка restricted join_rules.
	JoinRulesAllow []string
	// ThirdPartyInvites — content m.room.third_party_invite по токену (state_key).
	// Нужны публичные ключи, которыми подписан third_party_invite приглашения.
	ThirdPartyInvites map[string]json.RawMessage
	// AllowMemberships — членство вступающего в комнатах allow-списка
	// (roomID → membership). Заполняет вызывающая сторона: auth не ходит в БД.
	AllowMemberships map[string]string
	Members          map[string]string // userID → membership
}

// BuildState собирает состояние из auth_events (последние события по типу/ключу).
func BuildState(events []Event) (State, error) {
	state := State{Members: map[string]string{}}

	for i := range events {
		ev := events[i]

		switch ev.Type {
		case "m.room.create":
			state.Create = &events[i]
		case "m.room.power_levels":
			var levels powerLevels
			if err := json.Unmarshal(ev.Content, &levels); err != nil {
				return State{}, ErrBadEvent
			}

			state.PowerLevels = &levels
		case "m.room.third_party_invite":
			if ev.StateKey == nil {
				continue
			}

			if state.ThirdPartyInvites == nil {
				state.ThirdPartyInvites = map[string]json.RawMessage{}
			}

			state.ThirdPartyInvites[*ev.StateKey] = ev.Content
		case "m.room.join_rules":
			var rules struct {
				JoinRule string `json:"join_rule"`
			}
			if err := json.Unmarshal(ev.Content, &rules); err == nil {
				state.JoinRules = rules.JoinRule
			}
		case "m.room.member":
			if ev.StateKey == nil {
				continue
			}

			var content struct {
				Membership string `json:"membership"`
			}
			if err := json.Unmarshal(ev.Content, &content); err == nil {
				state.Members[*ev.StateKey] = content.Membership
			}
		}
	}

	return state, nil
}

// allowedAuthTypes — типы событий, которые по спеке могут быть в auth_events.
var allowedAuthTypes = map[string]struct{}{
	"m.room.create":             {},
	"m.room.power_levels":       {},
	"m.room.join_rules":         {},
	"m.room.member":             {},
	"m.room.third_party_invite": {},
}

// ValidateAuthEvents проверяет сам набор auth_events события: без дублей, только
// из той же комнаты, только разрешённых типов, с create и собственным membership
// отправителя. Раньше принимался любой набор, и пир мог прислать событие с
// auth_events чужой комнаты или без create. В комнате v12 (room_id без
// домена) create, наоборот, в auth_events запрещен.
func ValidateAuthEvents(ev Event, authEvents []Event) error {
	if ev.Type == "m.room.create" {
		return nil
	}

	hashRoom := IsCreateHashRoomID(ev.RoomID)

	// В v12 create в auth_events не входит: у первого события создателя (его
	// join) набор пуст.
	if len(authEvents) == 0 && !hashRoom {
		return fmt.Errorf("%w: empty auth_events", ErrUnauthorized)
	}

	seen := make(map[string]struct{}, len(authEvents))

	var hasCreate bool

	for i := range authEvents {
		authEvent := authEvents[i]

		if authEvent.EventID != "" {
			if _, dup := seen[authEvent.EventID]; dup {
				return fmt.Errorf("%w: duplicate auth_events %s", ErrUnauthorized, authEvent.EventID)
			}

			seen[authEvent.EventID] = struct{}{}
		}

		if authEvent.RoomID != ev.RoomID {
			return fmt.Errorf("%w: auth_event %s from another room", ErrUnauthorized, authEvent.EventID)
		}

		if _, ok := allowedAuthTypes[authEvent.Type]; !ok {
			return fmt.Errorf("%w: auth_event %s of type %q", ErrUnauthorized, authEvent.EventID, authEvent.Type)
		}

		if authEvent.Type == "m.room.create" {
			hasCreate = true
		}
	}

	if hasCreate && hashRoom {
		return fmt.Errorf("%w: m.room.create in auth_events of v12 room", ErrUnauthorized)
	}

	if !hasCreate && !hashRoom {
		return fmt.Errorf("%w: auth_events without m.room.create", ErrUnauthorized)
	}

	// Собственный membership отправителя обязателен (кроме самого create и
	// приглашения отправителем себя — его запрещает checkInvite). Первый вход
	// или knock за себя членства еще не имеет: по алгоритму выбора auth_events
	// оно включается, только если есть (иначе в публичную комнату по
	// федерации было не войти без приглашения).
	if firstSelfMembership(ev) {
		return nil
	}

	for i := range authEvents {
		authEvent := authEvents[i]

		if authEvent.Type == "m.room.member" && authEvent.StateKey != nil && *authEvent.StateKey == ev.Sender {
			return nil
		}
	}

	return fmt.Errorf("%w: auth_events without sender membership", ErrUnauthorized)
}

// firstSelfMembership - join или knock отправителя за себя: событие, у
// которого собственного членства в auth_events может не быть.
func firstSelfMembership(ev Event) bool {
	if ev.Type != "m.room.member" || ev.StateKey == nil || *ev.StateKey != ev.Sender {
		return false
	}

	var content struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return false
	}

	return content.Membership == "join" || content.Membership == "knock"
}

// Check проверяет, авторизовано ли событие текущим состоянием.
//
// Минимальные правила v10/v11:
//   - m.room.create: без prev_events, домен sender'а = домен room_id;
//   - m.room.member: join по invite/public, leave для себя, kick/ban по power levels;
//   - m.room.power_levels: только участник с достаточным power level;
//   - прочие события: sender должен быть в комнате (join), state-события — power >= state_default.
func Check(ev Event, state State) error {
	if state.Create == nil {
		return fmt.Errorf("%w: no m.room.create in auth chain", ErrUnauthorized)
	}

	if ev.RoomID == "" || ev.Sender == "" {
		return ErrBadEvent
	}

	if ev.Type == "m.room.create" {
		if len(ev.Prev) > 0 {
			return fmt.Errorf("%w: create with prev_events", ErrUnauthorized)
		}

		// v12: room_id - хеш самого create, домена в нем нет; дополнительные
		// создатели обязаны быть корректными user_id.
		if CreatorsPrivileged(createVersion(ev.Content)) {
			return checkAdditionalCreators(ev.Content)
		}

		if domain(ev.Sender) != domain(ev.RoomID) {
			return fmt.Errorf("%w: create sender domain mismatch", ErrUnauthorized)
		}

		// creator в content, если есть, обязан совпадать с отправителем (v10; в v11
		// поля может не быть). Иначе пир назначает «создателя» из воздуха.
		var create struct {
			Creator  string `json:"creator"`
			Federate *bool  `json:"m.federate"`
		}

		if err := json.Unmarshal(ev.Content, &create); err != nil {
			return ErrBadEvent
		}

		if create.Creator != "" && create.Creator != ev.Sender {
			return fmt.Errorf("%w: create.creator %q != sender %q", ErrUnauthorized, create.Creator, ev.Sender)
		}

		return nil
	}

	// m.federate=false в create запрещает события с других серверов (спека v10+).
	if !federated(state, ev) {
		return fmt.Errorf("%w: room is not federated", ErrUnauthorized)
	}

	levels := effectiveLevels(state)
	senderLevel := levels.level(ev.Sender)

	// До появления m.room.power_levels создатель комнаты неявно имеет 100 (как в спеке);
	// в v12 его уровень и так бесконечен (levels.creators).
	if state.PowerLevels == nil && state.Create != nil && ev.Sender == creatorOf(state.Create) && senderLevel < 100 {
		senderLevel = 100
	}

	switch ev.Type {
	case "m.room.member":
		return checkMembership(ev, state, levels, senderLevel)
	case "m.room.power_levels":
		if state.Members[ev.Sender] != "join" {
			return fmt.Errorf("%w: sender is not joined", ErrUnauthorized)
		}

		// Synapse: требуемый уровень для power_levels = events["m.room.power_levels"],
		// иначе state_default (50), как get_send_level в event_auth.py.
		required := levels.stateDefault()

		if custom, ok := levels.Events["m.room.power_levels"]; ok {
			required = custom
		}

		if senderLevel < required {
			return fmt.Errorf("%w: power level %d < %d for power_levels", ErrUnauthorized, senderLevel, required)
		}

		return validatePowerLevelsContent(ev, levels, senderLevel)
	case "m.room.third_party_invite":
		if state.Members[ev.Sender] != "join" {
			return fmt.Errorf("%w: sender is not joined", ErrUnauthorized)
		}

		// По спеке третьесторонний invite создаёт участник с invite-уровнем,
		// а не с state_default.
		return requireLevel(senderLevel, levels.invite(), "m.room.third_party_invite")
	case "m.room.redaction":
		if state.Members[ev.Sender] != "join" {
			return fmt.Errorf("%w: sender is not joined", ErrUnauthorized)
		}

		// Redaction требует redact-уровня (правило спеки v10/v11); исключение для
		// автора самого события проверяется при применении, здесь его не знаем.
		return requireLevel(senderLevel, levels.redactLevel(), "redaction")
	default:
		if state.Members[ev.Sender] != "join" {
			return fmt.Errorf("%w: sender is not joined", ErrUnauthorized)
		}

		// Порог зависит от вида события: для state — state_default, для обычных
		// (m.room.message и прочие) — events_default. Раньше для не-state событий
		// проверка не выполнялась вовсе, поэтому забаненный или понижённый
		// участник мог слать сообщения.
		required := levels.eventsDefault()

		if ev.StateKey != nil {
			required = levels.stateDefault()
		}

		if custom, ok := levels.Events[ev.Type]; ok {
			required = custom
		}

		if senderLevel < required {
			return fmt.Errorf("%w: power level %d < %d for %s", ErrUnauthorized, senderLevel, required, ev.Type)
		}

		return nil
	}
}

// SendLevel - уровень, нужный для отправки события типа eventType (events[type],
// иначе events_default или state_default), как get_send_level в Synapse.
func SendLevel(state State, eventType string, isState bool) int {
	if state.PowerLevels == nil {
		return 0 // без power_levels в Synapse любой уровень отправки - 0
	}

	if custom, ok := state.PowerLevels.Events[eventType]; ok {
		return custom
	}

	if isState {
		return state.PowerLevels.stateDefault()
	}

	return state.PowerLevels.eventsDefault()
}

// Level — эффективный power level пользователя в состоянии комнаты.
// До появления power_levels создатель неявно имеет 100; в v12 создатели имеют
// CreatorLevel всегда.
func Level(state State, userID string) int {
	if _, ok := Creators(state)[userID]; ok {
		return CreatorLevel
	}

	if state.PowerLevels == nil {
		if state.Create != nil && creatorOf(state.Create) == userID {
			return 100
		}

		return 0
	}

	return state.PowerLevels.level(userID)
}

// PowerLevelFor — требуемый уровень действия (ban, kick, redact, invite) из power_levels.
// Второе значение — задан ли уровень явно (иначе действует дефолт 50).
func PowerLevelFor(state State, action string) (int, bool) {
	if state.PowerLevels == nil {
		return 50, false
	}

	levels := state.PowerLevels

	switch action {
	case "ban":
		if levels.Ban != nil {
			return *levels.Ban, true
		}
	case "kick":
		if levels.Kick != nil {
			return *levels.Kick, true
		}
	case "redact":
		if levels.Redact != nil {
			return *levels.Redact, true
		}
	case "invite":
		if levels.Invite != nil {
			return *levels.Invite, true
		}
	}

	return 50, false
}

// checkMembership проверяет переходы membership.
func checkMembership(ev Event, state State, levels *powerLevels, senderLevel int) error {
	if ev.StateKey == nil {
		return fmt.Errorf("%w: membership event without state_key", ErrBadEvent)
	}

	target := *ev.StateKey
	joining := target == ev.Sender

	var content struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return ErrBadEvent
	}

	switch content.Membership {
	case "join":
		if !joining {
			return fmt.Errorf("%w: cannot join for another user", ErrUnauthorized)
		}

		// Бан блокирует вход даже в публичную комнату: раньше проверялся только
		// join_rule, и забаненный входил обратно.
		if state.Members[target] == "ban" {
			return fmt.Errorf("%w: banned user cannot join", ErrUnauthorized)
		}

		switch state.Members[target] {
		case "join", "invite":
			return nil
		}

		// Создатель комнаты может войти сразу после m.room.create (join до появления invite).
		if state.Create != nil && creatorOf(state.Create) == target {
			return nil
		}

		// По умолчанию join_rule = invite (как в спеке и Synapse): пустое значение
		// не означает «публичная комната».
		if state.JoinRules == "public" {
			return nil
		}

		// restricted: вход разрешён, если join подписан авторизующим участником
		// (join_authorised_via_users_server) с достаточным уровнем. Списки allow
		// проверяются при сборке состояния комнаты — здесь их нет.
		if state.JoinRules == "restricted" || state.JoinRules == "knock_restricted" {
			return checkRestrictedJoin(ev, state, levels, target)
		}

		return fmt.Errorf("%w: join requires invite", ErrUnauthorized)
	case "leave":
		if joining {
			// Забаненный не может снять с себя бан выходом: leave не должен
			// перезаписывать ban.
			if state.Members[target] == "ban" {
				return fmt.Errorf("%w: banned user cannot leave the ban", ErrUnauthorized)
			}

			return nil
		}

		// Кик: цель должна быть в комнате, порог — kick. Снятие бана — порог ban,
		// и цель обязана быть забаненной (иначе это не unban). Во всех случаях
		// отправитель обязан быть СТРОГО выше цели: иначе равный по уровню
		// участник выкидывает другого (спека v10).
		switch state.Members[target] {
		case "join", "invite", "knock":
			if err := requireLevel(senderLevel, levels.kick(), "leave (kick) for another user"); err != nil {
				return err
			}

			return requireHigher(senderLevel, levels.level(target), "kick")
		case "ban":
			if err := requireLevel(senderLevel, levels.banDuration(), "unban"); err != nil {
				return err
			}

			return requireHigher(senderLevel, levels.level(target), "unban")
		default:
			return fmt.Errorf("%w: kick of absent user", ErrUnauthorized)
		}
	case "ban":
		if err := requireLevel(senderLevel, levels.banDuration(), "ban"); err != nil {
			return err
		}

		// Банить можно только того, кто строго ниже по уровню.
		return requireHigher(senderLevel, levels.level(target), "ban")
	case "invite":
		return checkInvite(ev, state, levels, senderLevel, target)
	case "knock":
		if state.JoinRules != "knock" && state.JoinRules != "knock_restricted" {
			return fmt.Errorf("%w: knock is not allowed by join rules", ErrUnauthorized)
		}

		if state.Members[target] == "ban" || state.Members[target] == "join" {
			return fmt.Errorf("%w: cannot knock in state %q", ErrUnauthorized, state.Members[target])
		}

		return nil
	default:
		return fmt.Errorf("%w: unknown membership %q", ErrBadEvent, content.Membership)
	}
}

// federated сообщает, разрешает ли комната федерацию: m.federate=false в create
// запрещает события с серверов, отличных от сервера комнаты.
func federated(state State, ev Event) bool {
	if state.Create == nil {
		return true
	}

	var create struct {
		Federate *bool `json:"m.federate"`
	}

	if err := json.Unmarshal(state.Create.Content, &create); err != nil || create.Federate == nil {
		return true
	}

	if *create.Federate {
		return true
	}

	// В v12 у room_id нет домена: сервер комнаты - сервер создателя.
	if IsCreateHashRoomID(ev.RoomID) {
		return domain(ev.Sender) == domain(state.Create.Sender)
	}

	return domain(ev.Sender) == domain(ev.RoomID)
}

// checkRestrictedJoin проверяет вход в restricted-комнату.
//
// Вход разрешён, если вступающий состоит в одной из комнат allow-списка
// (membership проверяет вызывающая сторона и кладёт в State.AllowMemberships).
// Иначе нужен join_authorised_via_users_server: этот пользователь должен быть в
// комнате и иметь уровень не ниже invite. Без обоих restricted-комната открыта
// всем.
func checkRestrictedJoin(ev Event, state State, levels *powerLevels, target string) error {
	for _, allowRoom := range state.JoinRulesAllow {
		if state.AllowMemberships[allowRoom] == "join" {
			return nil
		}
	}

	var content struct {
		Authorised string `json:"join_authorised_via_users_server"`
	}

	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return ErrBadEvent
	}

	if content.Authorised == "" {
		return fmt.Errorf("%w: restricted join without join_authorised_via_users_server", ErrUnauthorized)
	}

	if state.Members[content.Authorised] != "join" {
		return fmt.Errorf("%w: authorising user %s is not joined", ErrUnauthorized, content.Authorised)
	}

	if target == content.Authorised {
		return fmt.Errorf("%w: user cannot authorise own join", ErrUnauthorized)
	}

	if level := levels.level(content.Authorised); level < levels.invite() {
		return fmt.Errorf("%w: authorising user level %d < invite %d",
			ErrUnauthorized, level, levels.invite())
	}

	return nil
}

// checkInvite проверяет приглашение: отправитель в комнате, хватает invite-уровня,
// цель не забанена и ещё не в комнате. Раньше invite разрешался безусловно.
func checkInvite(ev Event, state State, levels *powerLevels, senderLevel int, target string) error {
	// Третьесторонний invite (спека v10/v11): приглашение подтверждено подписанным
	// third_party_invite, поэтому уровень invite не проверяется, но структура и
	// совпадение mxid с целью обязательны.
	if thirdPartyInvite(ev.Content) != nil {
		return validateThirdPartyInvite(ev, state, target)
	}

	if target == ev.Sender {
		return fmt.Errorf("%w: cannot invite yourself", ErrUnauthorized)
	}

	if state.Members[ev.Sender] != "join" {
		return fmt.Errorf("%w: inviter is not joined", ErrUnauthorized)
	}

	switch state.Members[target] {
	case "ban":
		return fmt.Errorf("%w: cannot invite banned user", ErrUnauthorized)
	case "join":
		return fmt.Errorf("%w: user already joined", ErrUnauthorized)
	case "invite":
		// Повторный invite уже приглашённого — идемпотентен и разрешён
		// безусловно (как в Synapse): уровень проверять не нужно.
		return nil
	}

	if err := requireLevel(senderLevel, levels.invite(), "invite"); err != nil {
		return err
	}

	return nil
}

// requireHigher требует, чтобы отправитель был строго выше цели по power level:
// иначе равный по уровню участник кикает или банит другого.
func requireHigher(senderLevel, targetLevel int, action string) error {
	if senderLevel <= targetLevel {
		return fmt.Errorf("%w: %s requires sender level > target (%d <= %d)",
			ErrUnauthorized, action, senderLevel, targetLevel)
	}

	return nil
}

// requireLevel проверяет достаточность power level.
func requireLevel(senderLevel, required int, action string) error {
	if senderLevel < required {
		return fmt.Errorf("%w: %s requires %d, sender has %d", ErrUnauthorized, action, required, senderLevel)
	}

	return nil
}

// level возвращает power level пользователя (с учётом users_default и создателей v12).
func (p *powerLevels) level(userID string) int {
	if _, ok := p.creators[userID]; ok {
		return CreatorLevel
	}

	if p.Users != nil {
		if level, ok := p.Users[userID]; ok {
			return level
		}
	}

	if p.UsersDefault != nil {
		return *p.UsersDefault
	}

	return 0
}

func (p *powerLevels) eventsDefault() int {
	if p.EventsDefault != nil {
		return *p.EventsDefault
	}

	return 0
}

func (p *powerLevels) stateDefault() int {
	if p.StateDefault != nil {
		return *p.StateDefault
	}

	return 50
}

func (p *powerLevels) kick() int {
	if p.Kick != nil {
		return *p.Kick
	}

	return 50
}

func (p *powerLevels) invite() int {
	if p.Invite != nil {
		return *p.Invite
	}

	return 0
}

func (p *powerLevels) inviteLevel() int { return p.invite() }

func (p *powerLevels) redactLevel() int {
	if p.Redact != nil {
		return *p.Redact
	}

	return 50
}

func (p *powerLevels) usersDefault() int {
	if p.UsersDefault != nil {
		return *p.UsersDefault
	}

	return 0
}

func (p *powerLevels) banDuration() int {
	if p.Ban != nil {
		return *p.Ban
	}

	return 50
}

// creatorOf - создатель комнаты: creator из content m.room.create, а если поля
// нет (v11 убрал его из content), отправитель m.room.create.
func creatorOf(create *Event) string {
	var parsed struct {
		Creator string `json:"creator"`
	}

	if err := json.Unmarshal(create.Content, &parsed); err == nil && parsed.Creator != "" {
		return parsed.Creator
	}

	return create.Sender
}

// domain возвращает домен из идентификатора (@user:domain / !room:domain).
func domain(id string) string {
	if _, d, found := strings.Cut(id, ":"); found {
		return d
	}

	return ""
}

// validatePowerLevelsContent проверяет правила спеки для события m.room.power_levels:
// значения — целые числа, ключи users — валидные user_id, и никто не может выдать
// уровень выше своего или изменить уровень участника не ниже своего.
func validatePowerLevelsContent(ev Event, current *powerLevels, senderLevel int) error {
	var body struct {
		Ban           *json.Number           `json:"ban"`
		Kick          *json.Number           `json:"kick"`
		Redact        *json.Number           `json:"redact"`
		Invite        *json.Number           `json:"invite"`
		EventsDefault *json.Number           `json:"events_default"`
		StateDefault  *json.Number           `json:"state_default"`
		UsersDefault  *json.Number           `json:"users_default"`
		Users         map[string]json.Number `json:"users"`
		Events        map[string]json.Number `json:"events"`
	}

	decoder := json.NewDecoder(bytes.NewReader(ev.Content))
	decoder.UseNumber()

	if err := decoder.Decode(&body); err != nil {
		return fmt.Errorf("%w: power_levels content is not an object", ErrBadEvent)
	}

	// json.Number принимает и строку "50": значения уровней обязаны быть числами.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Content, &raw); err != nil {
		return fmt.Errorf("%w: power_levels content is not an object", ErrBadEvent)
	}

	for field, value := range raw {
		switch field {
		case "users", "events":
			continue
		case "notifications":
			// notifications - объект уровней (room и др.), значения - целые числа
			// (объект целиком отклонялся как «не число», Complement
			// TestDemotingUsersViaUsersDefault).
			var levels map[string]json.RawMessage
			if json.Unmarshal(value, &levels) != nil {
				return fmt.Errorf("%w: power_levels.notifications must be an object", ErrBadEvent)
			}

			for key, level := range levels {
				var number json.Number
				if err := json.Unmarshal(level, &number); err != nil || bytes.HasPrefix(bytes.TrimSpace(level), []byte(`"`)) {
					return fmt.Errorf("%w: power_levels.notifications.%s must be an integer", ErrBadEvent, key)
				}

				if _, err := number.Int64(); err != nil {
					return fmt.Errorf("%w: power_levels.notifications.%s must be an integer", ErrBadEvent, key)
				}
			}

			continue
		}

		trimmed := bytes.TrimSpace(value)
		if len(trimmed) > 0 && (trimmed[0] == '"' || trimmed[0] == '{' || trimmed[0] == '[') {
			return fmt.Errorf("%w: power_levels.%s must be an integer", ErrBadEvent, field)
		}
	}

	scalars := map[string]*json.Number{
		"ban": body.Ban, "kick": body.Kick, "redact": body.Redact, "invite": body.Invite,
		"events_default": body.EventsDefault, "state_default": body.StateDefault,
		"users_default": body.UsersDefault,
	}

	for field, value := range scalars {
		if value == nil {
			continue
		}

		number, err := value.Int64()
		if err != nil {
			return fmt.Errorf("%w: power_levels.%s is not an integer", ErrBadEvent, field)
		}

		old := current.scalar(field)

		if number != int64(old) && int(number) > senderLevel {
			return fmt.Errorf("%w: cannot set %s to %d above own level %d",
				ErrUnauthorized, field, number, senderLevel)
		}

		// Спека: менять значение, которое выше уровня отправителя, нельзя.
		if number != int64(old) && old > senderLevel {
			return fmt.Errorf("%w: cannot change %s from %d above own level %d",
				ErrUnauthorized, field, old, senderLevel)
		}
	}

	for userID, value := range body.Users {
		if !strings.HasPrefix(userID, "@") || !strings.Contains(userID, ":") {
			return fmt.Errorf("%w: invalid user id %q in power_levels", ErrBadEvent, userID)
		}

		// v12: уровень создателя не задается в power_levels.
		if _, creator := current.creators[userID]; creator {
			return fmt.Errorf("%w: room creator %s in power_levels.users", ErrUnauthorized, userID)
		}

		number, err := value.Int64()
		if err != nil {
			return fmt.Errorf("%w: power_levels.users level is not an integer", ErrBadEvent)
		}

		old := current.level(userID)

		if int(number) == old {
			continue
		}

		if int(number) > senderLevel || old > senderLevel {
			return fmt.Errorf("%w: cannot change level of %s (%d -> %d) at sender level %d",
				ErrUnauthorized, userID, old, number, senderLevel)
		}

		// Спека: уровень другого участника, равный своему, менять нельзя (только
		// явную запись users, как Synapse).
		if explicit, ok := current.Users[userID]; ok && userID != ev.Sender && explicit == senderLevel {
			return fmt.Errorf("%w: cannot change level of %s equal to own level %d",
				ErrUnauthorized, userID, senderLevel)
		}
	}

	// Удаление записи users - тоже изменение: тех, кто выше или наравне (кроме
	// себя), убрать нельзя (спека, как Synapse _check_power_levels).
	for userID, old := range current.Users {
		if _, kept := body.Users[userID]; kept {
			continue
		}

		if _, creator := current.creators[userID]; creator {
			continue
		}

		if old > senderLevel || (userID != ev.Sender && old == senderLevel) {
			return fmt.Errorf("%w: cannot remove level of %s (%d) at sender level %d",
				ErrUnauthorized, userID, old, senderLevel)
		}
	}

	for eventType, value := range body.Events {
		number, err := value.Int64()
		if err != nil {
			return fmt.Errorf("%w: power_levels.events level is not an integer", ErrBadEvent)
		}

		old := current.eventsDefault()

		if custom, ok := current.Events[eventType]; ok {
			old = custom
		}

		if int(number) != old && int(number) > senderLevel {
			return fmt.Errorf("%w: cannot require %d for %s above own level %d",
				ErrUnauthorized, number, eventType, senderLevel)
		}
	}

	return nil
}

// scalar — текущее значение скалярного уровня по имени поля.
func (p *powerLevels) scalar(field string) int {
	switch field {
	case "ban":
		return p.banDuration()
	case "kick":
		return p.kick()
	case "redact":
		return p.redactLevel()
	case "invite":
		return p.inviteLevel()
	case "events_default":
		return p.eventsDefault()
	case "state_default":
		return p.stateDefault()
	case "users_default":
		return p.usersDefault()
	}

	return 0
}

// thirdPartyInvite достаёт блок third_party_invite из content, если он есть.
type thirdPartyInviteBlock struct {
	DisplayName string `json:"display_name"`
	Signed      struct {
		MXID       string          `json:"mxid"`
		Token      string          `json:"token"`
		Signatures map[string]any  `json:"signatures"`
		Raw        json.RawMessage `json:"-"`
	} `json:"signed"`
}

func thirdPartyInvite(content []byte) *thirdPartyInviteBlock {
	var parsed struct {
		ThirdPartyInvite *thirdPartyInviteBlock `json:"third_party_invite"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return nil
	}

	if parsed.ThirdPartyInvite == nil {
		return nil
	}

	var signed struct {
		ThirdPartyInvite struct {
			Signed json.RawMessage `json:"signed"`
		} `json:"third_party_invite"`
	}

	if err := json.Unmarshal(content, &signed); err == nil {
		parsed.ThirdPartyInvite.Signed.Raw = signed.ThirdPartyInvite.Signed
	}

	return parsed.ThirdPartyInvite
}

// validateThirdPartyInvite проверяет структуру подписанного приглашения: mxid
// совпадает с целью, token и подписи непустые, приглашающий в комнате.
func validateThirdPartyInvite(ev Event, state State, target string) error {
	block := thirdPartyInvite(ev.Content)
	if block == nil {
		return nil
	}

	if block.Signed.MXID != target {
		return fmt.Errorf("%w: third_party_invite.mxid %q != target %q",
			ErrUnauthorized, block.Signed.MXID, target)
	}

	if block.Signed.Token == "" || len(block.Signed.Signatures) == 0 {
		return fmt.Errorf("%w: third_party_invite without token/signatures", ErrUnauthorized)
	}

	if state.Members[ev.Sender] != "join" {
		return fmt.Errorf("%w: third_party inviter is not joined", ErrUnauthorized)
	}

	// Подпись `signed` — ключом из m.room.third_party_invite с тем же токеном.
	// Без сверки любой пир присылал «приглашение» с любым mxid.
	stateContent, ok := state.ThirdPartyInvites[block.Signed.Token]
	if !ok {
		return fmt.Errorf("%w: no m.room.third_party_invite for token %q",
			ErrUnauthorized, block.Signed.Token)
	}

	if err := verifyThirdPartySignature(block.Signed.Raw, stateContent); err != nil {
		return err
	}

	return nil
}

// verifyThirdPartySignature проверяет, что `signed` подписан одним из публичных
// ключей события m.room.third_party_invite.
func verifyThirdPartySignature(signed, stateContent json.RawMessage) error {
	var keys struct {
		PublicKey  string `json:"public_key"`
		PublicKeys []struct {
			PublicKey string `json:"public_key"`
		} `json:"public_keys"`
	}

	if err := json.Unmarshal(stateContent, &keys); err != nil {
		return ErrBadEvent
	}

	candidates := make([]string, 0, 1+len(keys.PublicKeys))
	if keys.PublicKey != "" {
		candidates = append(candidates, keys.PublicKey)
	}

	for _, k := range keys.PublicKeys {
		if k.PublicKey != "" {
			candidates = append(candidates, k.PublicKey)
		}
	}

	if len(candidates) == 0 {
		return fmt.Errorf("%w: third_party_invite state without public keys", ErrUnauthorized)
	}

	var body struct {
		Signatures map[string]map[string]string `json:"signatures"`
	}

	if err := json.Unmarshal(signed, &body); err != nil || len(body.Signatures) == 0 {
		return fmt.Errorf("%w: third_party_invite without signatures", ErrUnauthorized)
	}

	// Подписывается `signed` без блока signatures (как в спе ке 3pid-инвайтов).
	var message map[string]json.RawMessage
	if err := json.Unmarshal(signed, &message); err != nil {
		return ErrBadEvent
	}

	delete(message, "signatures")

	canonical, err := signing.CanonicalJSON(message)
	if err != nil {
		return fmt.Errorf("%w: canonical signed: %w", ErrBadEvent, err)
	}

	for _, signatures := range body.Signatures {
		for keyID, signature := range signatures {
			if keyID == "" || signature == "" {
				continue
			}

			for _, publicKey := range candidates {
				if verifyErr := signing.VerifyEd25519(publicKey, string(canonical), signature); verifyErr == nil {
					return nil
				}
			}
		}
	}

	return fmt.Errorf("%w: third_party_invite signature does not match room key", ErrUnauthorized)
}

// IsCreateHashRoomID - room_id комнаты v12: «!» и хеш m.room.create без серверной
// части (MSC4291). У комнат ранних версий room_id всегда «!opaque:server».
func IsCreateHashRoomID(roomID string) bool {
	return len(roomID) > 1 && roomID[0] == '!' && !strings.Contains(roomID, ":")
}

// CreatorsPrivileged - действуют ли в версии комнаты правила создателей v12
// (MSC4289, MSC4291): версии 12 и выше и нестабильная org.matrix.hydra.11, под
// которой Synapse создавал такие комнаты до v12.
func CreatorsPrivileged(roomVersion string) bool {
	if roomVersion == "org.matrix.hydra.11" {
		return true
	}

	number, err := strconv.Atoi(roomVersion)

	return err == nil && number >= 12
}

// createVersion - room_version из content m.room.create (по спецификации без поля - "1").
func createVersion(content json.RawMessage) string {
	var body struct {
		RoomVersion string `json:"room_version"`
	}

	if err := json.Unmarshal(content, &body); err != nil || body.RoomVersion == "" {
		return "1"
	}

	return body.RoomVersion
}

// Creators - создатели комнаты v12: отправитель m.room.create и additional_creators.
// Для комнат ранних версий - nil: там уровень создателя задают power_levels.
func Creators(state State) map[string]struct{} {
	if state.Create == nil || !CreatorsPrivileged(createVersion(state.Create.Content)) {
		return nil
	}

	creators := map[string]struct{}{state.Create.Sender: {}}

	var body struct {
		AdditionalCreators []any `json:"additional_creators"`
	}

	if err := json.Unmarshal(state.Create.Content, &body); err == nil {
		for _, value := range body.AdditionalCreators {
			if userID, ok := value.(string); ok {
				creators[userID] = struct{}{}
			}
		}
	}

	return creators
}

// effectiveLevels - power_levels комнаты с создателями v12 (копия: State не меняется).
func effectiveLevels(state State) *powerLevels {
	levels := &powerLevels{}
	if state.PowerLevels != nil {
		copied := *state.PowerLevels
		levels = &copied
	}

	levels.creators = Creators(state)

	return levels
}

// checkAdditionalCreators - additional_creators create v12: массив корректных user_id.
func checkAdditionalCreators(content json.RawMessage) error {
	var body struct {
		AdditionalCreators json.RawMessage `json:"additional_creators"`
	}

	if err := json.Unmarshal(content, &body); err != nil {
		return ErrBadEvent
	}

	if len(body.AdditionalCreators) == 0 {
		return nil
	}

	var creators []any
	if err := json.Unmarshal(body.AdditionalCreators, &creators); err != nil {
		return fmt.Errorf("%w: additional_creators is not an array", ErrUnauthorized)
	}

	for _, value := range creators {
		userID, ok := value.(string)
		if !ok || !strings.HasPrefix(userID, "@") || !strings.Contains(userID, ":") {
			return fmt.Errorf("%w: invalid additional_creators entry %v", ErrUnauthorized, value)
		}
	}

	return nil
}
