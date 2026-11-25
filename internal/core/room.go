package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Ошибки комнат и событий; транспорт отображает их в M_FORBIDDEN/M_NOT_FOUND/M_BAD_JSON.
var (
	// ErrForbidden — действие запрещено (нет прав или нет членства в комнате).
	ErrForbidden = errors.New("core: forbidden")
	// ErrLastAdmin — попытка снять роль у последнего администратора.
	ErrLastAdmin = errors.New("core: cannot remove the last admin")
	// ErrBadAuthRefs — auth_events события ссылаются на неизвестное событие.
	ErrBadAuthRefs = errors.New("core: auth event references unknown event")
	// ErrInvalidRoomVersion — запрошена неподдерживаемая версия комнаты.
	ErrInvalidRoomVersion = errors.New("core: unsupported room version")
	// ErrBadJSON — некорректный параметр запроса (отображается в M_BAD_JSON).
	ErrBadJSON = errors.New("core: invalid request parameter")
	// ErrTooLarge — событие больше лимита PDU 65536 байт (M_TOO_LARGE).
	ErrTooLarge = errors.New("core: event too large")
)

// MaxEventSize — предельный размер события (PDU) в байтах, как в спецификации.
const MaxEventSize = 65536

// validateEvent проверяет тип и content клиентского события: тип - любая
// непустая строка до 255 байт (спецификация не ограничивает пространство имен,
// раньше io.element.* и im.vector.* отклонялись), content - JSON-объект.
func validateEvent(eventType string, content []byte) error {
	if eventType == "" || len(eventType) > 255 || strings.ContainsAny(eventType, "\x00\n\r\t") {
		return fmt.Errorf("core: invalid event type %q: %w", eventType, ErrBadJSON)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(content, &obj); err != nil || obj == nil {
		return fmt.Errorf("core: content must be a JSON object: %w", ErrBadJSON)
	}

	return validateCanonical(content)
}

// validateCanonical отклоняет content, который нельзя записать каноническим JSON:
// дробные числа, целые вне ±(2^53-1), одиночные суррогаты. Synapse для комнат v6+
// отвечает так же 400 M_BAD_JSON; без проверки hashes и event_id такого события не
// посчитались бы.
func validateCanonical(content []byte) error {
	if _, err := signing.Canonicalize(content); err != nil {
		return fmt.Errorf("core: content is not canonical JSON: %w: %w", ErrBadJSON, err)
	}

	return nil
}

// checkEventSize отклоняет событие, которое в виде PDU больше MaxEventSize.
func checkEventSize(event Event) error {
	raw, err := json.Marshal(PDUObject(event))
	if err != nil {
		return err
	}

	if len(raw) > MaxEventSize {
		return fmt.Errorf("core: event is %d bytes: %w", len(raw), ErrTooLarge)
	}

	return nil
}

// Поддерживаемые версии комнат: v10 по умолчанию, v11 и v12 (комнаты v12
// переезжают из Synapse 1.165, где это версия по умолчанию).
const (
	DefaultRoomVersion = 10
	MaxRoomVersion     = 12
	// HashRoomIDVersion - первая версия, где room_id - хеш m.room.create без домена,
	// создатели из create с бесконечным уровнем, create вне auth_events (MSC4289,
	// MSC4291).
	HashRoomIDVersion = 12
	// FederationMaxRoomVersion - старшая версия для федерации: v12 по федерации в обе
	// стороны - create без room_id, create вне auth_events, state resolution v2.1
	// (до него - 11).
	FederationMaxRoomVersion = 12
)

// Room — комната: opaque room_id, версия и создатель.
type Room struct {
	RoomID      string
	RoomVersion int
	Creator     string
	IsPublic    bool
}

// TransactionKey — ключ идемпотентности txn-эндпоинта (/send и родственные).
type TransactionKey struct {
	UserID   string
	DeviceID string
	Endpoint string
	TxnID    string
}

// CreateRoomRequest — параметры POST /createRoom, которые мы поддерживаем.
type CreateRoomRequest struct {
	Creator      string
	Name         string
	Topic        string
	RoomVersion  int
	IsPublic     bool
	Preset       string
	Invite       []string
	InitialState []StateEventRequest
	// CreationExtras — дополнительные поля content для m.room.create (например,
	// {"type":"m.space"} для пространств).
	CreationExtras map[string]any
	// PowerLevelOverride — power_level_content_override: сливается с уровнями по
	// умолчанию (users и events - по ключам, остальное - заменой), как в Synapse.
	PowerLevelOverride map[string]any
	// PowerLevels - готовый content m.room.power_levels вместо уровней по умолчанию
	// (upgrade переносит уровни старой комнаты целиком).
	PowerLevels map[string]any
	// IsDirect - личный чат: приглашения получают is_direct=true в content, по нему
	// Element показывает комнату приглашенному как личную.
	IsDirect bool
}

// StateEventRequest — событие начального состояния из initial_state.
type StateEventRequest struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

// RoomStore — хранилище комнат, событий, состояния и членства.
// Реализация — internal/store (pgx); домен не знает про SQL.
type RoomStore interface {
	// CreateRoom атомарно создаёт комнату и записывает стартовые события.
	CreateRoom(ctx context.Context, room Room, events []Event) error
	RoomByID(ctx context.Context, roomID string) (Room, error)
	// LatestEvents отдаёт последние события комнаты (по stream_ordering) для DAG.
	LatestEvents(ctx context.Context, roomID string, limit int) ([]Event, error)
	// AppendEvent пишет событие и обновляет состояние/членство. Если txn уже
	// использован, возвращает ранее созданное событие без новой записи.
	AppendEvent(ctx context.Context, event Event, txn *TransactionKey) (Event, bool, error)
	EventByID(ctx context.Context, roomID, eventID string) (Event, error)
	// RoomEvents отдаёт окно событий комнаты от курсора from в направлении backward.
	RoomEvents(ctx context.Context, roomID string, from int64, limit int, backward bool) ([]Event, error)
	// MinMaxStream возвращает минимальный и максимальный stream_ordering комнаты.
	MinMaxStream(ctx context.Context, roomID string) (int64, int64, error)
	// StreamAtDepth - граница топологической позиции Synapse (depth, stream) в
	// stream_ordering комнаты: backward - max(stream_ordering) событий
	// с (depth, stream_ordering) <= (depth, stream), иначе - min(stream_ordering)
	// событий с > (depth, stream). found=false - таких событий нет.
	StreamAtDepth(ctx context.Context, roomID string, depth, stream int64, backward bool) (pos int64, found bool, err error)
	Membership(ctx context.Context, roomID, userID string) (string, error)
	// AuthEventIDs отдаёт event_id'ы auth-событий для нового события комнаты
	// (create, power_levels, member отправителя и участника, join_rules). Нужны до
	// расчёта event_id: reference hash считается по каноническому JSON с auth_events.
	AuthEventIDs(ctx context.Context, roomID, sender, eventType string, stateKey *string) ([]string, error)
	// MembersPage — страница m.room.member-событий комнаты (state_key > at).
	MembersPage(ctx context.Context, roomID, at string, limit int) ([]Event, string, error)
	// ForgetRoom помечает комнату забытой для пользователя.
	ForgetRoom(ctx context.Context, userID, roomID string) error
	// JoinedMembers — участники с membership=join и их профили.
	JoinedMembers(ctx context.Context, roomID string) (map[string]Profile, error)
	// AliasesByRoom — алиасы комнаты.
	AliasesByRoom(ctx context.Context, roomID string) ([]string, error)
	// ResolveAlias — room_id по алиасу (ErrNotFound, если нет).
	ResolveAlias(ctx context.Context, alias string) (string, error)
	// AliasOwner — создатель алиаса и его комната.
	AliasOwner(ctx context.Context, alias string) (creator, roomID string, err error)
	// CreateAlias — добавить алиас (ErrForbidden, если занят).
	CreateAlias(ctx context.Context, alias, roomID, creator string) error
	// DeleteAlias — удалить алиас (ErrNotFound, если нет).
	DeleteAlias(ctx context.Context, alias string) error
	// StateEvents — материализованное состояние комнаты.
	StateEvents(ctx context.Context, roomID string) ([]Event, error)
	// CurrentStateEvent - текущее событие состояния по ключу; нет - ErrNotFound.
	CurrentStateEvent(ctx context.Context, roomID, eventType, stateKey string) (Event, error)
	// MembershipInfo - членство пользователя с позицией события и отметкой forget.
	MembershipInfo(ctx context.Context, roomID, userID string) (MembershipInfo, error)
	// StateAtStream - состояние комнаты на позиции stream_ordering (включительно).
	StateAtStream(ctx context.Context, roomID string, pos int64) ([]Event, error)
	// AuthStateEvents - срез текущего состояния для auth rules события не о
	// членстве: m.room.create, m.room.power_levels, m.room.join_rules и
	// m.room.member перечисленных пользователей.
	AuthStateEvents(ctx context.Context, roomID string, members []string) ([]Event, error)
	// SetRoomPublic публикует комнату в каталоге или снимает с публикации
	// (перенос публикации при upgrade).
	SetRoomPublic(ctx context.Context, roomID string, public bool) error
}

// MembershipInfo - членство пользователя в комнате: значение, позиция события
// членства и отметка «забыто» (POST /forget).
type MembershipInfo struct {
	Membership string
	Stream     int64
	Forgotten  bool
}

// RoomService — доменный сервис комнат: создание, отправка, чтение, членство.
type RoomService struct {
	store      RoomStore
	serverName string
	now        func() time.Time
	// profileOf - профиль локального пользователя для displayname и avatar_url в
	// m.room.member (join, invite); nil - без профиля.
	profileOf func(ctx context.Context, userID string) (Profile, error)
	// upgradeMu сериализует upgrade комнат.
	upgradeMu sync.Mutex
	// signPDU подписывает PDU своего события ключом сервера; nil - федерация
	// выключена, PDU не сохраняется.
	signPDU PDUSigner
}

// PDUSigner подписывает PDU своего события (объект с hashes) по правилам redaction
// версии комнаты и возвращает объект с signatures. Ключ сервера живет в api
// (eventjson.SignVersioned), core его не знает.
type PDUSigner func(object map[string]any, version int) (map[string]any, error)

// WithPDUSigner включает сохранение исходного PDU: каждое новое событие сразу
// подписывается, и в федерацию уходит ровно этот JSON.
func (s *RoomService) WithPDUSigner(sign PDUSigner) *RoomService {
	s.signPDU = sign

	return s
}

// NewRoomService создаёт сервис комнат.
func NewRoomService(store RoomStore, serverName string) *RoomService {
	return &RoomService{store: store, serverName: serverName, now: time.Now}
}

// WithProfiles подключает профили: join и invite кладут displayname и avatar_url
// участника в m.room.member, как Synapse.
func (s *RoomService) WithProfiles(profileOf func(ctx context.Context, userID string) (Profile, error)) *RoomService {
	s.profileOf = profileOf

	return s
}

// memberProfileContent дополняет content m.room.member профилем локального
// участника: displayname и avatar_url, если их нет в content.
func (s *RoomService) memberProfileContent(ctx context.Context, userID string, content map[string]any) {
	if s.profileOf == nil || !strings.HasSuffix(userID, ":"+s.serverName) {
		return
	}

	profile, err := s.profileOf(ctx, userID)
	if err != nil {
		return // профиль не обязателен: членство важнее имени
	}

	if _, ok := content["displayname"]; !ok && profile.DisplayName != "" {
		content["displayname"] = profile.DisplayName
	}

	if _, ok := content["avatar_url"]; !ok && profile.AvatarURL != "" {
		content["avatar_url"] = profile.AvatarURL
	}
}

// WithClock подменяет время (тесты).
func (s *RoomService) WithClock(now func() time.Time) *RoomService {
	s.now = now

	return s
}

// UniqueInvitees убирает повторы из списка приглашенных, сохраняя порядок.
func UniqueInvitees(invitees []string) []string {
	seen := make(map[string]struct{}, len(invitees))
	out := invitees[:0:0]

	for _, id := range invitees {
		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		out = append(out, id)
	}

	return out
}

// CreateRoom создаёт комнату с genesis-событиями и начальным состоянием.
func (s *RoomService) CreateRoom(ctx context.Context, req CreateRoomRequest) (string, error) {
	version := req.RoomVersion
	if version == 0 {
		version = DefaultRoomVersion
	}

	if version < DefaultRoomVersion || version > MaxRoomVersion {
		return "", ErrInvalidRoomVersion
	}

	// initial_state не может создавать членство и второй create: иначе создатель
	// вписывал бы в комнату чужих пользователей без их согласия.
	for _, st := range req.InitialState {
		if st.Type == "m.room.create" || st.Type == "m.room.member" || st.Type == "" {
			return "", fmt.Errorf("core: initial_state type %q is not allowed: %w", st.Type, ErrBadJSON)
		}
	}

	// Повтор пользователя в invite - одно приглашение (как в Synapse): второе
	// членство того же ключа ломало материализацию состояния - 500.
	req.Invite = UniqueInvitees(req.Invite)

	for _, invitee := range req.Invite {
		if !validUserID(invitee) {
			return "", fmt.Errorf("core: invalid invitee %q: %w", invitee, ErrBadJSON)
		}
	}

	preset := req.Preset
	if preset == "" {
		preset = "private_chat"
		if req.IsPublic {
			preset = "public_chat"
		}
	}

	hashRoomID := version >= HashRoomIDVersion

	var creators map[string]bool

	if hashRoomID {
		var err error
		if creators, err = roomCreators(&req, preset); err != nil {
			return "", err
		}
	}

	// v12: room_id - хеш m.room.create, он известен после сборки create.
	var roomID string

	if !hashRoomID {
		var err error
		if roomID, err = s.newRoomID(); err != nil {
			return "", err
		}
	}

	now := s.now().UnixMilli()

	var events []Event

	appendState := func(eventType, stateKey string, content any) error {
		raw, marshalErr := json.Marshal(content)
		if marshalErr != nil {
			return fmt.Errorf("core: marshal %s: %w", eventType, marshalErr)
		}

		// initial_state, creation_content и power_level_content_override приходят
		// от клиента: дробное число в них не дало бы посчитать event_id.
		if err := validateCanonical(raw); err != nil {
			return err
		}

		ev := s.newEvent(ctx, roomID, req.Creator, eventType, &stateKey, raw, events, now, version)
		events = append(events, ev)

		return nil
	}

	createContent := map[string]any{
		"room_version": fmt.Sprintf("%d", version),
	}

	// В v11 creator убран из content m.room.create: создатель - отправитель
	// события (спецификация v11).
	if version < 11 {
		createContent["creator"] = req.Creator
	}

	for key, value := range req.CreationExtras {
		if key == "creator" || key == "room_version" {
			continue // не даём переопределить инварианты
		}

		createContent[key] = value
	}

	if err := appendState("m.room.create", "", createContent); err != nil {
		return "", err
	}

	// v12: одинаковый create в ту же миллисекунду дал бы тот же room_id - тогда
	// origin_server_ts сдвигается на 1 мс, и хеш меняется.
	for attempt := 0; hashRoomID; attempt++ {
		roomID = "!" + strings.TrimPrefix(events[0].EventID, "$")
		events[0].RoomID = roomID

		_, lookupErr := s.store.RoomByID(ctx, roomID)
		if errors.Is(lookupErr, ErrNotFound) {
			break
		}

		if lookupErr != nil {
			return "", lookupErr
		}

		if attempt >= 3 {
			return "", fmt.Errorf("core: room %s already exists", roomID)
		}

		now++
		events, roomID = nil, ""

		if err := appendState("m.room.create", "", createContent); err != nil {
			return "", err
		}
	}

	room := Room{
		RoomID:      roomID,
		RoomVersion: version,
		Creator:     req.Creator,
		IsPublic:    req.IsPublic || req.Preset == "public_chat",
	}

	creatorMember := map[string]any{"membership": "join"}
	s.memberProfileContent(ctx, req.Creator, creatorMember)

	if err := appendState("m.room.member", req.Creator, creatorMember); err != nil {
		return "", err
	}

	levels := req.PowerLevels
	if levels == nil {
		levels = defaultPowerLevels(req.Creator, req.Invite)
		if preset == "trusted_private_chat" {
			users, _ := levels["users"].(map[string]int)
			for _, invitee := range req.Invite {
				users[invitee] = 100
			}
		}

		if hashRoomID {
			levelsForCreators(levels, creators)
		}

		mergePowerLevels(levels, req.PowerLevelOverride)
	}

	// v12: уровень создателей бесконечен и в power_levels не пишется - auth rules
	// отвергают такое событие (MSC4289).
	if hashRoomID && levelsMentionCreator(levels, creators) {
		return "", fmt.Errorf("core: power_levels.users of v12 room contains a creator: %w", ErrBadJSON)
	}

	if err := appendState("m.room.power_levels", "", levels); err != nil {
		return "", err
	}

	// Пресет пишет join_rules, history_visibility и guest_access, как Synapse:
	// без join_rules auth rules считают комнату invite-only (публичная комната
	// не пускала бы без приглашения). Ключи из initial_state имеют приоритет.
	inInitial := map[string]bool{}
	for _, st := range req.InitialState {
		inInitial[st.Type+"|"+st.StateKey] = true
	}

	joinRule, guestAccess := "invite", "can_join"
	if preset == "public_chat" {
		joinRule, guestAccess = "public", "forbidden"
	}

	for _, ps := range []struct {
		eventType string
		content   map[string]any
	}{
		{"m.room.join_rules", map[string]any{"join_rule": joinRule}},
		{"m.room.history_visibility", map[string]any{"history_visibility": "shared"}},
		{"m.room.guest_access", map[string]any{"guest_access": guestAccess}},
	} {
		if inInitial[ps.eventType+"|"] {
			continue
		}

		if err := appendState(ps.eventType, "", ps.content); err != nil {
			return "", err
		}
	}

	// name и topic перекрывают initial_state (порядок Synapse), повтор ключа в
	// initial_state - последнее значение: в genesis одно событие на ключ
	// состояния, иначе запись комнаты падала на сверке состояния (500).
	lastInitial := map[string]int{}
	for i, state := range req.InitialState {
		lastInitial[state.Type+"|"+state.StateKey] = i
	}

	for i, state := range req.InitialState {
		key := state.Type + "|" + state.StateKey
		if lastInitial[key] != i || (key == "m.room.name|" && req.Name != "") || (key == "m.room.topic|" && req.Topic != "") {
			continue
		}

		if err := appendState(state.Type, state.StateKey, state.Content); err != nil {
			return "", err
		}
	}

	if req.Name != "" {
		if err := appendState("m.room.name", "", map[string]any{"name": req.Name}); err != nil {
			return "", err
		}
	}

	// topic из параметра дублируется в m.topic (MSC3765, спека v1.15): так пишет
	// Synapse, и клиенты с rich topics читают его оттуда.
	if req.Topic != "" {
		topic := map[string]any{
			"topic":   req.Topic,
			"m.topic": map[string]any{"m.text": []map[string]any{{"body": req.Topic}}},
		}

		if err := appendState("m.room.topic", "", topic); err != nil {
			return "", err
		}
	}

	for _, invitee := range req.Invite {
		member := map[string]any{"membership": "invite"}
		if req.IsDirect {
			member["is_direct"] = true
		}

		if err := appendState("m.room.member", invitee, member); err != nil {
			return "", err
		}
	}

	// Инвариант genesis: каждый auth_event обязан ссылаться на уже собранное
	// событие с финальным id. Иначе в state уедет auth-ссылка на несуществующий
	// id (Synapse метит PL rejected: «No create event in auth events»).
	known := make(map[string]struct{}, len(events))

	for i := range events {
		for _, authID := range events[i].AuthEvents {
			if _, ok := known[authID]; !ok {
				return "", fmt.Errorf("core: %s references unknown auth event %s: %w",
					events[i].EventID, authID, ErrBadAuthRefs)
			}
		}

		known[events[i].EventID] = struct{}{}
	}

	if err := s.store.CreateRoom(ctx, room, events); err != nil {
		return "", err
	}

	return roomID, nil
}

// SendMessage пишет событие в комнату с идемпотентностью по txn_id.
// Возвращает event_id и признак «повтор» (событие не создавалось).
func (s *RoomService) SendMessage(
	ctx context.Context,
	sender, deviceID, roomID, eventType, txnID string,
	content json.RawMessage,
) (eventID string, duplicate bool, err error) {
	if err := validateEvent(eventType, content); err != nil {
		return "", false, err
	}

	if err := s.requireJoined(ctx, roomID, sender); err != nil {
		return "", false, err
	}

	// Power levels и auth rules действуют и на обычные события: забаненный или
	// понижённый участник не должен слать сообщения, а типы с порогом (например,
	// m.room.power_levels через /send) требуют соответствующего уровня. Раньше
	// /send проверял только членство.
	//
	// Auth rules события не о членстве смотрят только на create, power_levels и
	// членство отправителя - полный room_state не нужен: раньше каждое сообщение в
	// комнате на 2000 участников читало 2000 событий членства.
	stateEvents, err := s.store.AuthStateEvents(ctx, roomID, []string{sender})
	if err != nil {
		return "", false, err
	}

	if err := s.authorizeStateEvent(sender, roomID, eventType, nil, content, stateEvents); err != nil {
		return "", false, err
	}

	prev, err := s.prevEvents(ctx, roomID)
	if err != nil {
		return "", false, err
	}

	// auth_events уже проставлены в newEvent до расчёта event_id: перезапись после
	// id ломала reference hash (тело расходилось с подписью).
	event := s.newEvent(ctx, roomID, sender, eventType, nil, content, prev, s.now().UnixMilli(),
		s.roomVersion(ctx, roomID))

	if err := checkEventSize(event); err != nil {
		return "", false, err
	}

	// Ключ транзакции содержит комнату: тот же txnId в другой комнате - другое
	// событие (как в Synapse), а не event_id чужой комнаты.
	txn := &TransactionKey{UserID: sender, DeviceID: deviceID, Endpoint: "send:" + roomID, TxnID: txnID}

	written, duplicate, err := s.appendEvent(ctx, event, txn)
	if err != nil {
		return "", false, err
	}

	return written.EventID, duplicate, nil
}

// Event возвращает событие комнаты; читать может только участник.
func (s *RoomService) Event(ctx context.Context, userID, roomID, eventID string) (Event, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return Event{}, err
	}

	return s.store.EventByID(ctx, roomID, eventID)
}

// VisibleEvent - событие для GET /rooms/{roomId}/event/{eventId}. Не участник
// видит событие только в комнате с history_visibility world_readable; иначе 404
// (ErrNotFound), а не 403 - существование события не раскрывается, как в Synapse
// (Complement TestFetchEvent*). Event остается проверкой членства для
// typing, receipts и т.п.
func (s *RoomService) VisibleEvent(ctx context.Context, userID, roomID, eventID string) (Event, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		if !errors.Is(err, ErrForbidden) {
			return Event{}, err
		}

		if !s.worldReadable(ctx, roomID) {
			return Event{}, fmt.Errorf("core: event %s is not visible: %w", eventID, ErrNotFound)
		}
	}

	return s.store.EventByID(ctx, roomID, eventID)
}

// worldReadable - текущий history_visibility комнаты world_readable.
func (s *RoomService) worldReadable(ctx context.Context, roomID string) bool {
	events, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return false
	}

	for i := range events {
		ev := events[i]
		if ev.Type != "m.room.history_visibility" || ev.StateKey == nil || *ev.StateKey != "" {
			continue
		}

		var content struct {
			HistoryVisibility string `json:"history_visibility"`
		}

		return json.Unmarshal(ev.Content, &content) == nil && content.HistoryVisibility == "world_readable"
	}

	return false
}

// StoredEvent - сохраненное событие комнаты без проверки членства: для
// внутренних путей сервера (push по записанному событию).
func (s *RoomService) StoredEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	return s.store.EventByID(ctx, roomID, eventID)
}

// MessagesResult — страница истории комнаты.
type MessagesResult struct {
	Events []Event
	Start  string
	End    string
}

// Messages отдаёт историю комнаты с пагинацией (dir = b | f).
func (s *RoomService) Messages(
	ctx context.Context, userID, roomID, from, dir string, limit int,
) (MessagesResult, error) {
	// Вышедший видит историю до своего выхода включительно.
	bound, err := s.readBound(ctx, roomID, userID)
	if err != nil {
		return MessagesResult{}, err
	}

	if dir == "" {
		dir = "b"
	}

	if dir != "b" && dir != "f" {
		return MessagesResult{}, requestError(ErrInvalidParam, "Invalid dir")
	}

	if limit <= 0 {
		limit = 10
	}

	if limit > 100 {
		limit = 100
	}

	backward := dir == "b"
	minStream, maxStream, err := s.store.MinMaxStream(ctx, roomID)
	if err != nil {
		return MessagesResult{}, err
	}

	var cursor int64

	switch {
	case from != "":
		// Наш "s<pos>" или токен Synapse, оставшийся у клиента после переезда:
		// раньше на t<depth>-... был 400 M_BAD_JSON, и Element X
		// повторял запрос без конца.
		cursor, err = s.PageCursor(ctx, roomID, from, backward)
		if err != nil {
			return MessagesResult{}, err
		}
	case backward:
		cursor = maxStream + 1
	default:
		cursor = minStream - 1
	}

	if bound > 0 && backward && cursor > bound {
		cursor = bound + 1
	}

	var events []Event

	if bound == 0 || backward || cursor < bound {
		events, err = s.store.RoomEvents(ctx, roomID, cursor, limit, backward)
		if err != nil {
			return MessagesResult{}, err
		}
	}

	if bound > 0 {
		kept := events[:0]

		for i := range events {
			if events[i].StreamOrdering <= bound {
				kept = append(kept, events[i])
			}
		}

		events = kept
	}

	result := MessagesResult{Events: events, Start: from}
	if result.Start == "" {
		if backward {
			result.Start = formatStreamToken(maxStream)
		} else {
			result.Start = formatStreamToken(minStream)
		}
	}

	if len(events) > 0 {
		// from трактуется как эксклюзивная граница (< для b, > для f), поэтому
		// следующая страница начинается ровно с stream_ordering последнего
		// отданного события: -1/+1 теряли события, если между ними был разрыв
		// (пагинация без потерь).
		result.End = formatStreamToken(events[len(events)-1].StreamOrdering)
	} else {
		result.End = result.Start
	}

	return result, nil
}

// Join добавляет пользователя в комнату (нужно приглашение или публичная комната).
func (s *RoomService) Join(ctx context.Context, userID, roomID string) (string, error) {
	return s.JoinWithContent(ctx, userID, roomID, nil)
}

// JoinWithContent - Join с дополнительными полями m.room.member из тела запроса
// (reason и произвольные ключи, как в Synapse; membership не переопределяется).
func (s *RoomService) JoinWithContent(ctx context.Context, userID, roomID string, extra map[string]json.RawMessage) (string, error) {
	if _, err := s.store.RoomByID(ctx, roomID); err != nil {
		return "", err
	}

	// Возвращаем room_id, а не event_id: спека POST /join/{roomIdOrAlias}
	// обещает {"room_id": …}, и клиенты (Element) по нему открывают комнату.
	// event_id остаётся в событии membership и уходит в синк.
	if _, err := s.appendMembershipContent(ctx, roomID, userID, userID, "join", extra); err != nil {
		return "", err
	}

	return roomID, nil
}

// Invite приглашает пользователя в комнату; приглашать может только участник.
func (s *RoomService) Invite(ctx context.Context, userID, roomID, target string) (string, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return "", err
	}

	return s.appendMembership(ctx, roomID, userID, target, "invite")
}

// RemoteInviteDelivery доставляет приглашение пользователя другого сервера
// (PUT /invite) до записи события и возвращает PDU с подписью его сервера.
type RemoteInviteDelivery func(ctx context.Context, event Event) (json.RawMessage, error)

// InviteRemote - приглашение пользователя другого сервера: событие проверяется
// auth rules, доставляется deliver и только после успеха записывается с
// возвращенным PDU (подпись приглашенного сервера сохраняется, как в Synapse).
// Раньше событие записывалось до доставки, ошибка уходила в лог, клиент получал
// 200 с приглашением, которого у адресата нет.
func (s *RoomService) InviteRemote(ctx context.Context, userID, roomID, target string, deliver RemoteInviteDelivery) (string, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return "", err
	}

	return s.appendMembershipVia(ctx, roomID, userID, target, "invite", nil, deliver)
}

// Leave — выход пользователя из комнаты.
func (s *RoomService) Leave(ctx context.Context, userID, roomID string) (string, error) {
	membership, err := s.store.Membership(ctx, roomID, userID)
	if err != nil || (membership != "join" && membership != "invite") {
		return "", ErrForbidden
	}

	return s.appendMembership(ctx, roomID, userID, userID, "leave")
}

// appendMembership проверяет переход членства auth rules по текущему состоянию
// комнаты и пишет m.room.member. Раньше join, invite и leave шли мимо auth rules:
// забаненный входил в публичную комнату, приглашение снимало бан.
func (s *RoomService) appendMembership(ctx context.Context, roomID, sender, target, membership string) (string, error) {
	return s.appendMembershipContent(ctx, roomID, sender, target, membership, nil)
}

// appendMembershipContent - appendMembership с дополнительными полями content.
// Для join и invite в content попадает профиль цели (displayname, avatar_url).
func (s *RoomService) appendMembershipContent(
	ctx context.Context, roomID, sender, target, membership string, extra map[string]json.RawMessage,
) (string, error) {
	return s.appendMembershipVia(ctx, roomID, sender, target, membership, extra, nil)
}

// appendMembershipVia - appendMembershipContent с доставкой до записи: deliver
// (не nil) получает готовое событие и возвращает PDU для записи; ошибка - событие
// не пишется.
func (s *RoomService) appendMembershipVia(
	ctx context.Context, roomID, sender, target, membership string, extra map[string]json.RawMessage,
	deliver RemoteInviteDelivery,
) (string, error) {
	body := make(map[string]any, len(extra)+3)
	for key, value := range extra {
		body[key] = value
	}

	body["membership"] = membership

	if membership == "join" || membership == "invite" {
		s.memberProfileContent(ctx, target, body)
	}

	content, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("core: marshal membership: %w", err)
	}

	// Блокировка раньше auth rules: в заблокированную комнату не входят и по
	// приглашению, ответ - «комната заблокирована», а не «нет приглашения».
	if err := s.checkRoomBlock(ctx, Event{RoomID: roomID, Type: "m.room.member", StateKey: &target, Content: content}); err != nil {
		return "", err
	}

	stateEvents, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return "", err
	}

	// Повторный join с тем же content - нового события нет (Synapse,
	// Complement «Joining room twice is idempotent»).
	if membership == "join" && sender == target {
		for i := range stateEvents {
			ev := stateEvents[i]
			if ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == target &&
				membershipOf(ev.Content) == "join" && sameJSON(ev.Content, content) {
				return ev.EventID, nil
			}
		}
	}

	key := target
	if authErr := s.authorizeStateEvent(sender, roomID, "m.room.member", &key, content, stateEvents); authErr != nil {
		legacy := membership == "join" && sender == target && s.legacyPublicJoin(ctx, roomID, target, stateEvents)
		if !legacy {
			return "", fmt.Errorf("%w: %w", ErrForbidden, authErr)
		}
	}

	prev, err := s.prevEvents(ctx, roomID)
	if err != nil {
		return "", err
	}

	stateKey := target
	event := s.newEvent(ctx, roomID, sender, "m.room.member", &stateKey, content, prev, s.now().UnixMilli(),
		s.roomVersion(ctx, roomID))

	if deliver != nil {
		pdu, err := deliver(ctx, event)
		if err != nil {
			return "", err
		}

		event.PDU = pdu
	}

	written, _, err := s.appendEvent(ctx, event, nil)
	if err != nil {
		return "", err
	}

	return written.EventID, nil
}

// legacyPublicJoin — совместимость с комнатами, созданными до записи join_rules
// пресетом: публичная по флагу комната без m.room.join_rules пускает всех, кроме
// забаненных.
func (s *RoomService) legacyPublicJoin(ctx context.Context, roomID, userID string, stateEvents []Event) bool {
	for i := range stateEvents {
		ev := stateEvents[i]
		if ev.Type == "m.room.join_rules" {
			return false
		}

		if ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == userID {
			var body struct {
				Membership string `json:"membership"`
			}

			if json.Unmarshal(ev.Content, &body) == nil && body.Membership == "ban" {
				return false
			}
		}
	}

	room, err := s.store.RoomByID(ctx, roomID)

	return err == nil && room.IsPublic
}

// validUserID — минимальная проверка формата user_id: @localpart:server.
func validUserID(userID string) bool {
	if !strings.HasPrefix(userID, "@") {
		return false
	}

	local, server, ok := strings.Cut(userID[1:], ":")

	return ok && local != "" && server != ""
}

// mergePowerLevels сливает power_level_content_override с уровнями по умолчанию:
// users и events - по ключам, остальные поля - заменой (как Synapse).
func mergePowerLevels(levels, override map[string]any) {
	for key, value := range override {
		switch key {
		case "users", "events":
			src, ok := value.(map[string]any)
			if !ok {
				levels[key] = value

				continue
			}

			merged := map[string]any{}

			switch cur := levels[key].(type) {
			case map[string]int:
				for k, v := range cur {
					merged[k] = v
				}
			case map[string]any:
				for k, v := range cur {
					merged[k] = v
				}
			}

			for k, v := range src {
				merged[k] = v
			}

			levels[key] = merged
		default:
			levels[key] = value
		}
	}
}

// requireJoined проверяет, что пользователь состоит в комнате.
func (s *RoomService) requireJoined(ctx context.Context, roomID, userID string) error {
	membership, err := s.store.Membership(ctx, roomID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrForbidden
		}

		return err
	}

	if membership != "join" {
		return ErrForbidden
	}

	return nil
}

// newEvent собирает событие с подсчитанным event_id, depth и prev_events.
func (s *RoomService) newEvent(
	ctx context.Context,
	roomID, sender, eventType string,
	stateKey *string,
	content json.RawMessage,
	prev []Event,
	originServerTS int64,
	version int,
) Event {
	depth := int64(1)

	prevIDs := make([]string, 0, len(prev))
	for i := range prev {
		p := &prev[i]
		prevIDs = append(prevIDs, p.EventID)
		if p.Depth >= depth {
			depth = p.Depth + 1
		}
	}

	event := Event{
		RoomID:         roomID,
		Type:           eventType,
		Sender:         sender,
		StateKey:       stateKey,
		Content:        content,
		PrevEvents:     prevIDs,
		Depth:          depth,
		OriginServerTS: originServerTS,
		Origin:         s.serverName,
		RoomVersion:    version,
	}
	// auth_events обязаны быть до расчёта id: они входят в канонический JSON, из
	// которого считается reference hash.
	event.AuthEvents = s.authEventIDs(ctx, roomID, sender, eventType, stateKey, prev)

	// v12: create в auth_events не входит, его id выводится из room_id (MSC4291).
	// Synapse отвергает событие v12 с create в auth_events.
	if version >= HashRoomIDVersion {
		event.AuthEvents = withoutCreate(event.AuthEvents, roomID)
	}

	// Порядок auth_events детерминирован: id считается по ним, и то же самое уйдёт
	// пиру (иначе reference hash не совпадёт с телом).
	sort.Strings(event.AuthEvents)

	var object map[string]any

	event.EventID, object = s.eventID(event)

	// Исходный PDU - тот же объект, по которому посчитан event_id, с подписью:
	// пересборка из колонок давала у пира другой id.
	if s.signPDU != nil && object != nil {
		event.PDU = s.signedPDU(object, event.RoomVersion)
	}

	return event
}

// signedPDU подписывает объект события и кодирует его без HTML-экранирования.
// Ошибка подписи - событие без сохраненного PDU (соберется при отдаче).
func (s *RoomService) signedPDU(object map[string]any, version int) json.RawMessage {
	if version == 0 {
		version = DefaultRoomVersion
	}

	signed, err := s.signPDU(object, version)
	if err != nil {
		return nil
	}

	raw, err := MarshalPDU(signed)
	if err != nil {
		return nil
	}

	return raw
}

// MarshalPDU кодирует PDU в JSON без HTML-экранирования (<, >, & как есть):
// так хранится и отдается исходный PDU.
func MarshalPDU(object any) (json.RawMessage, error) {
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(object); err != nil {
		return nil, fmt.Errorf("core: marshal pdu: %w", err)
	}

	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// roomVersion отдаёт версию комнаты (DefaultRoomVersion, если комнаты ещё нет
// или версия не распознана): от неё зависят правила redaction и reference hash.
func (s *RoomService) roomVersion(ctx context.Context, roomID string) int {
	room, err := s.store.RoomByID(ctx, roomID)
	if err != nil || room.RoomVersion < DefaultRoomVersion {
		return DefaultRoomVersion
	}

	return room.RoomVersion
}

// eventID считает event_id как Synapse: «$» + base64url(sha256(canonical JSON события)).
// canonical JSON у нас — json.Marshal от map (ключи сортируются), этого достаточно,
// чтобы один и тот же ввод давал один и тот же id.
//
// Возвращает и сам объект события с hashes (nil, если id не посчитан): он
// подписывается и хранится как исходный PDU.
func (s *RoomService) eventID(event Event) (id string, object map[string]any) {
	object = PDUObject(event)

	// hashes.sha256 = sha256(канон. JSON без hashes/signatures/unsigned)
	if hash, err := signing.ContentHash(object); err == nil {
		object["hashes"] = map[string]string{"sha256": hash}
	}

	// event_id = reference hash по **отредактированному** (pruned) событию, как
	// compute_event_reference_hash в Synapse: content редуцируется по правилам
	// redaction для типа события (m.room.name и т.п. теряют content). Иначе наши
	// id расходятся с Synapse и он отвергает события.
	version := event.RoomVersion
	if version == 0 {
		version = DefaultRoomVersion
	}

	// Единая точка расчёта (core.EventID): top-level whitelist + redaction content
	// по версии комнаты. Ручная сборка pruned здесь расходилась с eventjson.EventID.
	referenceID, err := EventID(object, version)
	if err != nil {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)

		return "$" + hex.EncodeToString(buf), nil
	}

	return referenceID, object
}

// PDUObject собирает PDU события из его полей - тот набор, по которому считаются
// hashes, event_id и подпись (без hashes и signatures): state_key только у
// state-событий, top-level redacts у redaction до v11, без room_id у create v12.
// Им же федерация пересобирает PDU событий, у которых исходный не сохранен.
func PDUObject(event Event) map[string]any {
	prev := event.PrevEvents
	if prev == nil {
		prev = []string{}
	}

	auth := event.AuthEvents
	if auth == nil {
		auth = []string{}
	}

	content := event.Content
	if len(content) == 0 {
		content = json.RawMessage(`{}`)
	}

	object := map[string]any{
		"type":             event.Type,
		"sender":           event.Sender,
		"room_id":          event.RoomID,
		"content":          content,
		"prev_events":      prev,
		"auth_events":      auth,
		"depth":            event.Depth,
		"origin_server_ts": event.OriginServerTS,
	}

	if event.StateKey != nil {
		object["state_key"] = *event.StateKey
	}

	// До v11 redaction хранит цель в top-level `redacts`, а не в content
	// (с v11 — только content.redacts). Поле обязано быть в объекте на проводе и в
	// reference hash, иначе prune_event пира и наш id расходятся.
	version := event.RoomVersion
	if version == 0 {
		version = DefaultRoomVersion
	}

	// v12: у m.room.create нет room_id - room_id сам выводится из его хеша (MSC4291).
	if isHashRoomCreate(event.Type, event.StateKey, version) {
		delete(object, "room_id")
	}

	if event.Type == "m.room.redaction" && version < 11 {
		if redacts := contentRedacts(event.Content); redacts != "" {
			object["redacts"] = redacts
			// В спеке до v11 content.redacts не существует (там только reason):
			// на провод и в хеш content уходит без него, иначе наши дампы и
			// сверка с эталоном расходятся.
			if stripped, ok := stripContentRedacts(event.Content); ok {
				object["content"] = json.RawMessage(stripped)
			}
		}
	}

	return object
}

// contentRedacts достаёт content.redacts (цель redaction) из content события.
func contentRedacts(content json.RawMessage) string {
	var body struct {
		Redacts string `json:"redacts"`
	}

	if err := json.Unmarshal(content, &body); err != nil {
		return ""
	}

	return body.Redacts
}

// authEventIDs берёт auth-события из хранилища; для ещё не записанной комнаты
// (genesis в CreateRoom) — из уже собранных событий.
func (s *RoomService) authEventIDs(
	ctx context.Context, roomID, sender, eventType string, stateKey *string, prev []Event,
) []string {
	if ids, err := s.store.AuthEventIDs(ctx, roomID, sender, eventType, stateKey); err == nil && len(ids) > 0 {
		return ids
	}

	// Ключи — type + \x00 + state_key: так же строится lookup ниже. Раньше create и
	// power_levels были без \x00 и никогда не попадали в auth_events — их события
	// уходили без ссылки на create, и Synapse отвергал PL («No create event in auth
	// events»).
	needed := map[string]bool{
		"m.room.create\x00":          true,
		"m.room.power_levels\x00":    true,
		"m.room.member\x00" + sender: true,
	}

	if eventType == "m.room.member" && stateKey != nil {
		needed["m.room.member\x00"+*stateKey] = true
		needed["m.room.join_rules\x00"] = true
	}

	var ids []string

	for i := range prev {
		event := &prev[i]
		if event.StateKey == nil {
			continue
		}

		if needed[event.Type+"\x00"+*event.StateKey] {
			ids = append(ids, event.EventID)
		}
	}

	return ids
}

// isHashRoomCreate - m.room.create комнаты v12: без room_id в PDU.
func isHashRoomCreate(eventType string, stateKey *string, version int) bool {
	return version >= HashRoomIDVersion && eventType == "m.room.create" && stateKey != nil && *stateKey == ""
}

// withoutCreate убирает из auth_events комнаты v12 ее create («$» + хеш из room_id).
func withoutCreate(ids []string, roomID string) []string {
	if !strings.HasPrefix(roomID, "!") {
		return ids
	}

	createID := "$" + roomID[1:]
	out := ids[:0:0]

	for _, id := range ids {
		if id != createID {
			out = append(out, id)
		}
	}

	return out
}

// roomCreators - создатели комнаты v12: создатель и additional_creators из
// creation_content; в пресете trusted_private_chat приглашенные становятся
// дополнительными создателями вместо уровня 100 (MSC4289, как Synapse).
func roomCreators(req *CreateRoomRequest, preset string) (map[string]bool, error) {
	creators := map[string]bool{req.Creator: true}

	var additional []string

	if raw, ok := req.CreationExtras["additional_creators"]; ok {
		list, isList := raw.([]any)
		if !isList {
			return nil, fmt.Errorf("core: additional_creators must be a list: %w", ErrBadJSON)
		}

		for _, value := range list {
			userID, isString := value.(string)
			if !isString || !validUserID(userID) {
				return nil, fmt.Errorf("core: invalid additional_creators entry %v: %w", value, ErrBadJSON)
			}

			if !creators[userID] {
				creators[userID] = true
				additional = append(additional, userID)
			}
		}
	}

	if preset == "trusted_private_chat" {
		for _, invitee := range req.Invite {
			if !creators[invitee] {
				creators[invitee] = true
				additional = append(additional, invitee)
			}
		}
	}

	if len(additional) > 0 {
		extras := make(map[string]any, len(req.CreationExtras)+1)
		for key, value := range req.CreationExtras {
			extras[key] = value
		}

		extras["additional_creators"] = additional
		req.CreationExtras = extras
	}

	return creators, nil
}

// levelsForCreators приводит стартовые power_levels к v12: создатели не
// записываются в users, tombstone требует 150 - выше любого уровня, кроме
// создателей (замену комнаты делают только они, как в Synapse).
func levelsForCreators(levels map[string]any, creators map[string]bool) {
	if users, ok := levels["users"].(map[string]int); ok {
		for userID := range creators {
			delete(users, userID)
		}
	}

	if events, ok := levels["events"].(map[string]int); ok {
		events["m.room.tombstone"] = 150
	}
}

// levelsMentionCreator - есть ли создатель среди ключей power_levels.users.
func levelsMentionCreator(levels map[string]any, creators map[string]bool) bool {
	switch users := levels["users"].(type) {
	case map[string]int:
		for userID := range users {
			if creators[userID] {
				return true
			}
		}
	case map[string]any:
		for userID := range users {
			if creators[userID] {
				return true
			}
		}
	}

	return false
}

// newRoomID генерирует opaque room_id вида !<random>:<server>.
func (s *RoomService) newRoomID() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("core: room id: %w", err)
	}

	return "!" + base64.RawURLEncoding.EncodeToString(buf) + ":" + s.serverName, nil
}

// defaultPowerLevels — стартовые power levels: создатель 100, остальные 0.
func defaultPowerLevels(creator string, invitees []string) map[string]any {
	users := map[string]int{creator: 100}
	for _, invitee := range invitees {
		if _, ok := users[invitee]; !ok {
			users[invitee] = 0
		}
	}

	return map[string]any{
		"users":         users,
		"users_default": 0,
		"events": map[string]int{
			"m.room.name":               50,
			"m.room.power_levels":       100,
			"m.room.history_visibility": 100,
		},
		"events_default": 0,
		"state_default":  50,
		"ban":            50,
		"kick":           50,
		"redact":         50,
		"invite":         0,
	}
}

// formatStreamToken кодирует курсор stream_ordering для /messages. Курсор может
// быть отрицательным: события backfill, перенесенные из Synapse, лежат в начале
// истории с stream_ordering < 0. Раньше он обрезался до 0, и пагинация назад по
// таким событиям зацикливалась на одной странице.
func formatStreamToken(ordering int64) string {
	return "s" + fmt.Sprintf("%d", ordering)
}

// SortEventsByStream сортирует события по stream_ordering (для тестов и отладки).
func SortEventsByStream(events []Event) {
	sort.Slice(events, func(i, j int) bool { return events[i].StreamOrdering < events[j].StreamOrdering })
}

// MembersPage возвращает страницу m.room.member-событий комнаты (по state_key > at).
// ForgetRoom помечает комнату забытой: после выхода она больше не приходит в /sync.
//
// Забыть комнату, в которой пользователь состоит, нельзя: ErrStillJoined (400
// M_UNKNOWN, как в Synapse).
func (s *RoomService) ForgetRoom(ctx context.Context, userID, roomID string) error {
	if membership, err := s.store.Membership(ctx, roomID, userID); err == nil && membership == "join" {
		return ErrStillJoined
	}

	return s.store.ForgetRoom(ctx, userID, roomID)
}

// ErrStillJoined - операция требует выйти из комнаты (POST /forget участником).
var ErrStillJoined = errors.New("core: user is still joined to the room")

// MembersAt - m.room.member комнаты на позиции pos (параметр at в /members, спека):
// вышедший видит не позже своего выхода. Без пагинации.
func (s *RoomService) MembersAt(ctx context.Context, userID, roomID string, pos int64) ([]Event, string, error) {
	bound, err := s.readBound(ctx, roomID, userID)
	if err != nil {
		return nil, "", err
	}

	if bound > 0 && (pos <= 0 || pos > bound) {
		pos = bound
	}

	state, err := s.store.StateAtStream(ctx, roomID, pos)
	if err != nil {
		return nil, "", err
	}

	members := make([]Event, 0, len(state))

	for i := range state {
		if state[i].Type == "m.room.member" {
			members = append(members, state[i])
		}
	}

	sort.Slice(members, func(a, b int) bool { return *members[a].StateKey < *members[b].StateKey })

	return members, "", nil
}

func (s *RoomService) MembersPage(ctx context.Context, userID, roomID, at string, limit int) ([]Event, string, error) {
	bound, err := s.readBound(ctx, roomID, userID)
	if err != nil {
		return nil, "", err
	}

	// Вышедший видит участников на момент выхода.
	if bound > 0 {
		return s.MembersAt(ctx, userID, roomID, bound)
	}

	if limit <= 0 || limit > 100 {
		limit = 100
	}

	events, next, err := s.store.MembersPage(ctx, roomID, at, limit)
	if err != nil {
		return nil, "", err
	}

	return events, next, nil
}

// JoinedMembers отдаёт участников комнаты с membership=join (для /joined_members).
func (s *RoomService) JoinedMembers(ctx context.Context, userID, roomID string) (map[string]Profile, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return nil, err
	}

	return s.store.JoinedMembers(ctx, roomID)
}

// AliasesByRoom возвращает алиасы комнаты (видят участники).
func (s *RoomService) AliasesByRoom(ctx context.Context, userID, roomID string) ([]string, error) {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return nil, err
	}

	return s.store.AliasesByRoom(ctx, roomID)
}

// ResolveAlias резолвит алиас в комнату (без авторизации, как в CS-API).
func (s *RoomService) ResolveAlias(ctx context.Context, alias string) (string, error) {
	return s.store.ResolveAlias(ctx, alias)
}

// SetAlias создаёт алиас комнаты. Как в Synapse (room_alias_creation_rules по
// умолчанию): алиас может завести любой участник комнаты, power levels не нужны
// (Complement TestRoomDeleteAlias).
func (s *RoomService) SetAlias(ctx context.Context, userID, roomID, alias string) error {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return err
	}

	if _, err := s.store.RoomByID(ctx, roomID); err != nil {
		return err
	}

	if _, server, found := cutAlias(alias); !found || server != s.serverName {
		return fmt.Errorf("core: alias %q must be on this server: %w", alias, ErrBadJSON)
	}

	return s.store.CreateAlias(ctx, alias, roomID, userID)
}

// DeleteAlias удаляет алиас. Как в Synapse (_user_can_delete_alias): удалять может
// создатель алиаса либо участник с правом менять m.room.canonical_alias. Если алиас
// был в m.room.canonical_alias (alias или alt_aliases), он убирается оттуда новым
// state-событием от имени удаляющего; нехватка прав на это событие удаление не
// отменяет.
func (s *RoomService) DeleteAlias(ctx context.Context, userID, alias string) error {
	creator, roomID, err := s.store.AliasOwner(ctx, alias)
	if err != nil {
		return err
	}

	if creator != userID {
		if err := s.canChangeCanonicalAlias(ctx, userID, roomID); err != nil {
			return err
		}
	}

	if err := s.store.DeleteAlias(ctx, alias); err != nil {
		return err
	}

	return s.dropCanonicalAlias(ctx, userID, roomID, alias)
}

// canChangeCanonicalAlias - участник может отправить m.room.canonical_alias (по
// power levels); иначе ErrForbidden.
func (s *RoomService) canChangeCanonicalAlias(ctx context.Context, userID, roomID string) error {
	if err := s.requireJoined(ctx, roomID, userID); err != nil {
		return err
	}

	stateEvents, err := s.store.AuthStateEvents(ctx, roomID, []string{userID})
	if err != nil {
		return err
	}

	key := ""
	if err := s.authorizeStateEvent(userID, roomID, "m.room.canonical_alias", &key, []byte(`{}`), stateEvents); err != nil {
		if errors.Is(err, ErrNotAllowed) {
			return fmt.Errorf("%w: %w", ErrForbidden, err)
		}

		return err
	}

	return nil
}

// dropCanonicalAlias убирает удаленный алиас из m.room.canonical_alias комнаты.
func (s *RoomService) dropCanonicalAlias(ctx context.Context, userID, roomID, alias string) error {
	stateEvents, err := s.store.StateEvents(ctx, roomID)
	if err != nil {
		return err
	}

	var current json.RawMessage

	for i := range stateEvents {
		ev := stateEvents[i]
		if ev.Type == "m.room.canonical_alias" && ev.StateKey != nil && *ev.StateKey == "" {
			current = ev.Content
		}
	}

	if len(current) == 0 {
		return nil
	}

	var content map[string]json.RawMessage
	if err := json.Unmarshal(current, &content); err != nil {
		return nil //nolint:nilerr // битый content не мешает удалению алиаса
	}

	changed := false

	var main string
	if raw, ok := content["alias"]; ok && json.Unmarshal(raw, &main) == nil && main == alias {
		delete(content, "alias")

		changed = true
	}

	var alt []string
	if raw, ok := content["alt_aliases"]; ok && json.Unmarshal(raw, &alt) == nil {
		kept := make([]string, 0, len(alt))

		for _, a := range alt {
			if a != alias {
				kept = append(kept, a)
			}
		}

		if len(kept) != len(alt) {
			changed = true

			if len(kept) == 0 {
				delete(content, "alt_aliases")
			} else {
				raw, marshalErr := json.Marshal(kept)
				if marshalErr != nil {
					return marshalErr
				}

				content["alt_aliases"] = raw
			}
		}
	}

	if !changed {
		return nil
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return err
	}

	if _, _, err := s.SendState(ctx, userID, "", roomID, "m.room.canonical_alias", "", "", raw); err != nil &&
		!errors.Is(err, ErrNotAllowed) && !errors.Is(err, ErrForbidden) {
		return err
	}

	return nil
}

// RoomPublished сообщает, опубликована ли комната в каталоге (GET
// /directory/list/room). Неизвестная комната - ErrNotFound.
func (s *RoomService) RoomPublished(ctx context.Context, roomID string) (bool, error) {
	room, err := s.store.RoomByID(ctx, roomID)
	if err != nil {
		return false, err
	}

	return room.IsPublic, nil
}

// SetRoomPublished публикует комнату в каталоге или снимает с публикации (PUT
// /directory/list/room). Права как в Synapse (check_can_change_room_list):
// участник с правом на m.room.canonical_alias.
func (s *RoomService) SetRoomPublished(ctx context.Context, userID, roomID string, public bool) error {
	if _, err := s.store.RoomByID(ctx, roomID); err != nil {
		return err
	}

	if err := s.canChangeCanonicalAlias(ctx, userID, roomID); err != nil {
		return err
	}

	return s.store.SetRoomPublic(ctx, roomID, public)
}

// cutAlias разбирает "#local:server".
func cutAlias(alias string) (local, server string, ok bool) {
	if len(alias) < 3 || alias[0] != '#' {
		return "", "", false
	}

	local, server, found := strings.Cut(alias[1:], ":")

	return local, server, found && local != "" && server != ""
}

// stripContentRedacts убирает content.redacts — для комнат до v11 цель redaction
// живёт только в top-level `redacts`.
func stripContentRedacts(content json.RawMessage) ([]byte, bool) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(content, &body); err != nil {
		return nil, false
	}

	if _, has := body["redacts"]; !has {
		return nil, false
	}

	delete(body, "redacts")

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}

	return raw, true
}
