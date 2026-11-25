package core

import (
	"encoding/json"
	"slices"
	"sort"
)

// Simplified sliding sync (MSC4186): модель запроса, required_state,
// фильтры и сортировка списков, состояние соединения. Эталон поведения -
// Synapse 1.165 (synapse/handlers/sliding_sync, synapse/types/handlers/sliding_sync.py).
// Element X работает только через этот протокол.

// Значения state_key в required_state с особым смыслом.
const (
	// StateWildcard - все ключи типа; тип "*" - все состояние комнаты.
	StateWildcard = "*"
	// StateLazy - m.room.member отправителей и целей событий timeline.
	StateLazy = "$LAZY"
	// StateMe - ключ, равный user_id запрашивающего.
	StateMe = "$ME"
)

// Типы и членства, которые разбирает sliding sync.
const (
	evMember = "m.room.member"
	evName   = "m.room.name"
	evAvatar = "m.room.avatar"
	msJoin   = "join"
	msInvite = "invite"
	msLeave  = "leave"
	msBan    = "ban"
)

// SlidingMaxTimelineLimit - потолок timeline_limit (Synapse отвергает больше 1000).
const SlidingMaxTimelineLimit = 1000

// slidingMaxLazyMembers - сколько отданных lazy-участников помнить на комнату
// соединения (MAX_NUMBER_PREVIOUS_STATE_KEYS_TO_REMEMBER Synapse): больше -
// растет состояние, а повтор участника клиенту не вредит.
const slidingMaxLazyMembers = 100

// SlidingBumpEventTypes - события, поднимающие комнату в списке (bump_stamp и
// сортировка), как SLIDING_SYNC_DEFAULT_BUMP_EVENT_TYPES Synapse: реакции,
// правки состояния и receipts комнату не поднимают.
var SlidingBumpEventTypes = []string{
	"m.room.create",
	"m.room.message",
	"m.room.encrypted",
	"m.sticker",
	"m.call.invite",
	"m.poll.start",
	"m.beacon_info",
}

// isBumpEvent - событие поднимает комнату.
func isBumpEvent(eventType string) bool {
	return slices.Contains(SlidingBumpEventTypes, eventType)
}

// RequiredState - required_state комнаты: тип события -> state_key (отсортированы,
// без повторов). Ключ "*" - все события типа, $LAZY у m.room.member - участники
// событий timeline, $ME - сам пользователь. Тип "*" - все состояние комнаты:
// фильтр-исключение MSC3575 Synapse тоже не поддерживает и отдает все.
type RequiredState map[string][]string

// NewRequiredState собирает required_state из пар [тип, state_key].
func NewRequiredState(pairs [][2]string) RequiredState {
	out := RequiredState{}

	for _, pair := range pairs {
		out.add(pair[0], pair[1])
	}

	return out
}

func (r RequiredState) add(eventType, stateKey string) {
	keys := r[eventType]
	if i, found := slices.BinarySearch(keys, stateKey); !found {
		r[eventType] = slices.Insert(keys, i, stateKey)
	}
}

// All - запрошено все состояние (тип "*").
func (r RequiredState) All() bool {
	return len(r[StateWildcard]) > 0
}

// Lazy - участники загружаются лениво ($LAZY у m.room.member).
func (r RequiredState) Lazy() bool {
	return !r.All() && slices.Contains(r[evMember], StateLazy)
}

// allMembers - нужны все m.room.member комнаты.
func (r RequiredState) allMembers() bool {
	return r.All() || slices.Contains(r[evMember], StateWildcard)
}

// Includes - событие состояния (тип, ключ) входит в required_state без учета
// $LAZY.
func (r RequiredState) Includes(eventType, stateKey, userID string) bool {
	if r.All() {
		return true
	}

	keys := r[eventType]

	return slices.Contains(keys, StateWildcard) || slices.Contains(keys, stateKey) ||
		(stateKey == userID && slices.Contains(keys, StateMe))
}

// ExplicitMembers - участники, запрошенные явно (с $ME -> userID).
func (r RequiredState) ExplicitMembers(userID string) []string {
	out := make([]string, 0, len(r[evMember]))

	for _, key := range r[evMember] {
		switch key {
		case StateWildcard, StateLazy:
		case StateMe:
			out = append(out, userID)
		default:
			out = append(out, key)
		}
	}

	return out
}

// Combine - объединение двух required_state (комната в нескольких списках и
// подписке).
func (r RequiredState) Combine(o RequiredState) RequiredState {
	out := make(RequiredState, len(r)+len(o))

	for eventType, keys := range r {
		out[eventType] = slices.Clone(keys)
	}

	for eventType, keys := range o {
		for _, key := range keys {
			out.add(eventType, key)
		}
	}

	return out
}

// Equal - одинаковые required_state.
func (r RequiredState) Equal(o RequiredState) bool {
	if len(r) != len(o) {
		return false
	}

	for eventType, keys := range r {
		if !slices.Equal(keys, o[eventType]) {
			return false
		}
	}

	return true
}

// StateAddition - что дочитать из текущего состояния, когда required_state
// комнаты расширился между запросами (_required_state_changes Synapse,
// упрощенно): клиенту отдаются только изменения, а для новых ключей изменений
// может не быть.
type StateAddition struct {
	// All - нужно все состояние.
	All bool
	// Types - типы, запрошенные целиком ("*").
	Types []string
	// Pairs - отдельные (тип, ключ).
	Pairs [][2]string
}

// Empty - дочитывать нечего.
func (a StateAddition) Empty() bool {
	return !a.All && len(a.Types) == 0 && len(a.Pairs) == 0
}

// AddedSince - что добавилось в required_state относительно prev. $LAZY здесь не
// учитывается: недостающих участников дочитывает ленивая загрузка.
func (r RequiredState) AddedSince(prev RequiredState, userID string) StateAddition {
	if prev.All() {
		return StateAddition{}
	}

	if r.All() {
		return StateAddition{All: true}
	}

	var added StateAddition

	eventTypes := make([]string, 0, len(r))
	for eventType := range r {
		eventTypes = append(eventTypes, eventType)
	}

	sort.Strings(eventTypes)

	for _, eventType := range eventTypes {
		old := prev[eventType]
		if slices.Contains(old, StateWildcard) {
			continue
		}

		keys := r[eventType]
		if slices.Contains(keys, StateWildcard) {
			added.Types = append(added.Types, eventType)

			continue
		}

		for _, key := range keys {
			if slices.Contains(old, key) {
				continue
			}

			switch key {
			case StateLazy:
			case StateMe:
				if !slices.Contains(old, userID) {
					added.Pairs = append(added.Pairs, [2]string{eventType, userID})
				}
			default:
				if key != userID || !slices.Contains(old, StateMe) {
					added.Pairs = append(added.Pairs, [2]string{eventType, key})
				}
			}
		}
	}

	return added
}

// SlidingRoomConfig - параметры комнаты: timeline_limit и required_state.
type SlidingRoomConfig struct {
	TimelineLimit int
	RequiredState RequiredState
}

// Combine - надмножество параметров (комната попала в несколько списков или еще и
// в подписку): больший timeline_limit, объединение required_state.
func (c SlidingRoomConfig) Combine(o SlidingRoomConfig) SlidingRoomConfig {
	return SlidingRoomConfig{
		TimelineLimit: max(c.TimelineLimit, o.TimelineLimit),
		RequiredState: c.RequiredState.Combine(o.RequiredState),
	}
}

// Equal - одинаковые параметры.
func (c SlidingRoomConfig) Equal(o SlidingRoomConfig) bool {
	return c.TimelineLimit == o.TimelineLimit && c.RequiredState.Equal(o.RequiredState)
}

// SlidingFilters - filters списка (MSC4186): все условия через И, отсутствующее
// поле - без условия. room_name_like не поддерживается и в Synapse.
type SlidingFilters struct {
	IsDM        *bool
	IsEncrypted *bool
	IsInvite    *bool
	// Spaces - комнаты, перечисленные как m.space.child этих пространств (любого).
	Spaces []string
	// RoomTypes и NotRoomTypes: nil-элемент - комната без типа.
	RoomTypes    []*string
	NotRoomTypes []*string
	// HasRoomTypes и HasNotRoomTypes - поле задано (пустой список - тоже условие).
	HasRoomTypes    bool
	HasNotRoomTypes bool
	Tags            []string
	NotTags         []string
	HasTags         bool
	HasNotTags      bool
}

// NeedsRoomInfo - фильтру нужны тип комнаты или шифрование.
func (f *SlidingFilters) NeedsRoomInfo() bool {
	return f != nil && (f.IsEncrypted != nil || f.HasRoomTypes || f.HasNotRoomTypes)
}

// NeedsTags - фильтру нужны теги комнат.
func (f *SlidingFilters) NeedsTags() bool {
	return f != nil && (f.HasTags || f.HasNotTags)
}

// SlidingList - список (sliding window): окна ranges по отсортированным комнатам.
type SlidingList struct {
	Ranges  [][2]int
	Config  SlidingRoomConfig
	Filters *SlidingFilters
}

// SlidingRequest - тело запроса без расширений (их разбирает транспорт).
type SlidingRequest struct {
	ConnID string
	Lists  map[string]SlidingList
	// Subscriptions - room_subscriptions запроса. Подписки «липкие»: matrix-js-sdk
	// (Element Web) присылает подписку один раз и снимает ее unsubscribe_rooms,
	// поэтому они хранятся в состоянии соединения и действуют до снятия.
	Subscriptions map[string]SlidingRoomConfig
	// Unsubscribe - unsubscribe_rooms: снять подписки.
	Unsubscribe []string
}

// EffectiveSubscriptions - подписки соединения после запроса: сохраненные,
// дополненные и замененные подписками запроса, без снятых.
func (r *SlidingRequest) EffectiveSubscriptions(prev map[string]SlidingRoomConfig) map[string]SlidingRoomConfig {
	out := make(map[string]SlidingRoomConfig, len(prev)+len(r.Subscriptions))

	for roomID, cfg := range prev {
		out[roomID] = cfg
	}

	for roomID, cfg := range r.Subscriptions {
		out[roomID] = cfg
	}

	for _, roomID := range r.Unsubscribe {
		delete(out, roomID)
	}

	return out
}

// SlidingRoom - комната-кандидат списка: членство и то, по чему фильтруются и
// сортируются списки.
type SlidingRoom struct {
	RoomID     string
	Membership string
	// MembershipPos - позиция события членства.
	MembershipPos int64
	// LastBump - позиция последнего события из SlidingBumpEventTypes (0 - нет).
	LastBump int64
	// Known - состояние комнаты известно (есть m.room.create): без него фильтры по
	// типу и шифрованию комнату отбрасывают, как Synapse.
	Known     bool
	RoomType  *string
	Encrypted bool
	DM        bool
	Tags      map[string]bool
	// Parents - пространства фильтра spaces, где комната - m.space.child.
	Parents map[string]bool
}

// BumpStamp - позиция для сортировки и bump_stamp: для join - последнее
// «значимое» событие, для остальных - событие членства (дальше него
// пользователь не видит).
func (r *SlidingRoom) BumpStamp() int64 {
	stamp := r.MembershipPos
	if r.Membership == msJoin && r.LastBump > 0 {
		stamp = r.LastBump
	}

	return max(stamp, 0)
}

// Match - комната проходит фильтры списка.
func (f *SlidingFilters) Match(room *SlidingRoom) bool {
	if f == nil {
		return true
	}

	if f.IsDM != nil && *f.IsDM != room.DM {
		return false
	}

	if f.IsInvite != nil && *f.IsInvite != (room.Membership == msInvite) {
		return false
	}

	if f.IsEncrypted != nil && (!room.Known || room.Encrypted != *f.IsEncrypted) {
		return false
	}

	if f.HasRoomTypes || f.HasNotRoomTypes {
		if !room.Known {
			return false
		}

		if f.HasRoomTypes && !roomTypeIn(room.RoomType, f.RoomTypes) {
			return false
		}

		if f.HasNotRoomTypes && roomTypeIn(room.RoomType, f.NotRoomTypes) {
			return false
		}
	}

	if f.Spaces != nil && !anyKey(room.Parents, f.Spaces) {
		return false
	}

	if f.HasTags && !anyKey(room.Tags, f.Tags) {
		return false
	}

	return !f.HasNotTags || !anyKey(room.Tags, f.NotTags)
}

// roomTypeIn - тип комнаты есть в списке (nil - комната без типа).
func roomTypeIn(roomType *string, types []*string) bool {
	for _, t := range types {
		if (t == nil && roomType == nil) || (t != nil && roomType != nil && *t == *roomType) {
			return true
		}
	}

	return false
}

func anyKey(set map[string]bool, keys []string) bool {
	for _, key := range keys {
		if set[key] {
			return true
		}
	}

	return false
}

// SortSlidingRooms сортирует комнаты по bump_stamp от новых к старым; при равенстве -
// по room_id, чтобы окна были стабильны между запросами.
func SortSlidingRooms(rooms []SlidingRoom) {
	sort.SliceStable(rooms, func(i, j int) bool {
		a, b := rooms[i].BumpStamp(), rooms[j].BumpStamp()
		if a != b {
			return a > b
		}

		return rooms[i].RoomID < rooms[j].RoomID
	})
}

// SlidingListOp - операция SYNC окна списка: диапазон и комнаты в нем.
type SlidingListOp struct {
	Range   [2]int
	RoomIDs []string
}

// SlidingWindows режет отсортированный список по ranges (границы включительно,
// как в MSC4186). Диапазон за концом списка дает пустое окно.
func SlidingWindows(sorted []SlidingRoom, ranges [][2]int) []SlidingListOp {
	ops := make([]SlidingListOp, 0, len(ranges))

	for _, rng := range ranges {
		op := SlidingListOp{Range: rng, RoomIDs: []string{}}

		start, end := rng[0], rng[1]
		if start < 0 {
			start = 0
		}

		for i := start; i <= end && i < len(sorted); i++ {
			op.RoomIDs = append(op.RoomIDs, sorted[i].RoomID)
		}

		ops = append(ops, op)
	}

	return ops
}

// SlidingStream - поток, по которому соединение помнит, что отдано по комнате.
type SlidingStream int

// Потоки состояния соединения (как streams в sliding_sync_connection_streams).
const (
	SlidingStreamRooms SlidingStream = iota
	SlidingStreamAccountData
	SlidingStreamReceipts
)

// SlidingRoomStatus - что клиент получил по комнате в потоке. Отсутствие записи -
// никогда не отдавали (NEVER); Live - отдано все до позиции соединения (LIVE);
// иначе - отдано до Since, а после были изменения (PREVIOUSLY).
type SlidingRoomStatus struct {
	Live  bool  `json:"live,omitempty"`
	Since int64 `json:"since,omitempty"`
}

// SlidingConnState - состояние соединения (user, device, conn_id) на позицию:
// что отдано по комнатам, с какими параметрами и каких lazy-участников клиент уже
// знает. По нему incremental отдает только изменения.
type SlidingConnState struct {
	Rooms       map[string]SlidingRoomStatus
	AccountData map[string]SlidingRoomStatus
	Receipts    map[string]SlidingRoomStatus
	Configs     map[string]SlidingRoomConfig
	LazyMembers map[string][]string
	// Subscriptions - действующие room_subscriptions соединения.
	Subscriptions map[string]SlidingRoomConfig

	dirty bool
}

// NewSlidingConnState - пустое состояние (первый запрос соединения).
func NewSlidingConnState() *SlidingConnState {
	return &SlidingConnState{
		Rooms:         map[string]SlidingRoomStatus{},
		AccountData:   map[string]SlidingRoomStatus{},
		Receipts:      map[string]SlidingRoomStatus{},
		Configs:       map[string]SlidingRoomConfig{},
		LazyMembers:   map[string][]string{},
		Subscriptions: map[string]SlidingRoomConfig{},
	}
}

// Clone - независимая копия: новое состояние строится поверх прежнего, прежнее
// нужно для решений этого запроса.
func (s *SlidingConnState) Clone() *SlidingConnState {
	out := NewSlidingConnState()

	for roomID, st := range s.Rooms {
		out.Rooms[roomID] = st
	}

	for roomID, st := range s.AccountData {
		out.AccountData[roomID] = st
	}

	for roomID, st := range s.Receipts {
		out.Receipts[roomID] = st
	}

	for roomID, cfg := range s.Configs {
		out.Configs[roomID] = cfg
	}

	for roomID, users := range s.LazyMembers {
		out.LazyMembers[roomID] = users
	}

	for roomID, cfg := range s.Subscriptions {
		out.Subscriptions[roomID] = cfg
	}

	return out
}

// SetSubscriptions запоминает действующие подписки.
func (s *SlidingConnState) SetSubscriptions(subs map[string]SlidingRoomConfig) {
	same := len(subs) == len(s.Subscriptions)

	for roomID, cfg := range subs {
		if old, ok := s.Subscriptions[roomID]; !ok || !old.Equal(cfg) {
			same = false
		}
	}

	if same {
		return
	}

	s.Subscriptions = make(map[string]SlidingRoomConfig, len(subs))
	for roomID, cfg := range subs {
		s.Subscriptions[roomID] = cfg
	}

	s.dirty = true
}

// Dirty - состояние менялось и его нужно сохранить новой позицией.
func (s *SlidingConnState) Dirty() bool {
	return s.dirty
}

func (s *SlidingConnState) statuses(stream SlidingStream) map[string]SlidingRoomStatus {
	switch stream {
	case SlidingStreamAccountData:
		return s.AccountData
	case SlidingStreamReceipts:
		return s.Receipts
	default:
		return s.Rooms
	}
}

// Status - статус комнаты в потоке; ok=false - никогда не отдавали.
func (s *SlidingConnState) Status(stream SlidingStream, roomID string) (SlidingRoomStatus, bool) {
	st, ok := s.statuses(stream)[roomID]

	return st, ok
}

// RecordSent - по комнатам отдано все (LIVE).
func (s *SlidingConnState) RecordSent(stream SlidingStream, roomIDs ...string) {
	statuses := s.statuses(stream)

	for _, roomID := range roomIDs {
		if st, ok := statuses[roomID]; ok && st.Live {
			continue
		}

		statuses[roomID] = SlidingRoomStatus{Live: true}
		s.dirty = true
	}
}

// RecordUnsent - в комнатах есть изменения после token, которые клиенту не
// отданы: LIVE становится PREVIOUSLY(token), PREVIOUSLY сохраняет свою позицию,
// NEVER остается NEVER (комнату отдадут целиком, когда она попадет в окно).
func (s *SlidingConnState) RecordUnsent(stream SlidingStream, token int64, roomIDs ...string) {
	statuses := s.statuses(stream)

	for _, roomID := range roomIDs {
		if st, ok := statuses[roomID]; ok && st.Live {
			statuses[roomID] = SlidingRoomStatus{Since: token}
			s.dirty = true
		}
	}
}

// Config - параметры, с которыми комнату отдавали.
func (s *SlidingConnState) Config(roomID string) (SlidingRoomConfig, bool) {
	cfg, ok := s.Configs[roomID]

	return cfg, ok
}

// SetConfig запоминает параметры комнаты.
func (s *SlidingConnState) SetConfig(roomID string, cfg SlidingRoomConfig) {
	if old, ok := s.Configs[roomID]; ok && old.Equal(cfg) {
		return
	}

	s.Configs[roomID] = cfg
	s.dirty = true
}

// SetLazyMembers запоминает lazy-участников комнаты, которых клиент знает.
func (s *SlidingConnState) SetLazyMembers(roomID string, users []string) {
	if len(users) > slidingMaxLazyMembers {
		users = users[:slidingMaxLazyMembers]
	}

	if slices.Equal(s.LazyMembers[roomID], users) {
		return
	}

	if len(users) == 0 {
		delete(s.LazyMembers, roomID)
	} else {
		s.LazyMembers[roomID] = users
	}

	s.dirty = true
}

// slidingConnStateJSON - формат хранения: одинаковые required_state (обычно у
// всех комнат списка) хранятся один раз, комната ссылается на индекс.
type slidingConnStateJSON struct {
	Rooms       map[string]SlidingRoomStatus `json:"rooms,omitempty"`
	AccountData map[string]SlidingRoomStatus `json:"account_data,omitempty"`
	Receipts    map[string]SlidingRoomStatus `json:"receipts,omitempty"`
	States      []RequiredState              `json:"states,omitempty"`
	// Configs: комната -> [timeline_limit, индекс в States].
	Configs       map[string][2]int   `json:"configs,omitempty"`
	LazyMembers   map[string][]string `json:"lazy_members,omitempty"`
	Subscriptions map[string][2]int   `json:"subscriptions,omitempty"`
}

// MarshalJSON - формат хранения в БД.
func (s *SlidingConnState) MarshalJSON() ([]byte, error) {
	out := slidingConnStateJSON{
		Rooms: s.Rooms, AccountData: s.AccountData, Receipts: s.Receipts,
		Configs: make(map[string][2]int, len(s.Configs)), LazyMembers: s.LazyMembers,
	}

	index := map[string]int{}

	ref := func(cfg SlidingRoomConfig) ([2]int, error) {
		key, err := json.Marshal(cfg.RequiredState)
		if err != nil {
			return [2]int{}, err
		}

		i, ok := index[string(key)]
		if !ok {
			i = len(out.States)
			index[string(key)] = i
			out.States = append(out.States, cfg.RequiredState)
		}

		return [2]int{cfg.TimelineLimit, i}, nil
	}

	for roomID, cfg := range s.Configs {
		r, err := ref(cfg)
		if err != nil {
			return nil, err
		}

		out.Configs[roomID] = r
	}

	if len(s.Subscriptions) > 0 {
		out.Subscriptions = make(map[string][2]int, len(s.Subscriptions))
	}

	for roomID, cfg := range s.Subscriptions {
		r, err := ref(cfg)
		if err != nil {
			return nil, err
		}

		out.Subscriptions[roomID] = r
	}

	return json.Marshal(out)
}

// UnmarshalJSON читает формат хранения; ссылки на несуществующий required_state
// дают пустой (комнату отдадут заново по новым параметрам).
func (s *SlidingConnState) UnmarshalJSON(data []byte) error {
	var in slidingConnStateJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}

	*s = *NewSlidingConnState()

	for roomID, st := range in.Rooms {
		s.Rooms[roomID] = st
	}

	for roomID, st := range in.AccountData {
		s.AccountData[roomID] = st
	}

	for roomID, st := range in.Receipts {
		s.Receipts[roomID] = st
	}

	for roomID, users := range in.LazyMembers {
		s.LazyMembers[roomID] = users
	}

	config := func(ref [2]int) SlidingRoomConfig {
		state := RequiredState{}
		if ref[1] >= 0 && ref[1] < len(in.States) && in.States[ref[1]] != nil {
			state = in.States[ref[1]]
		}

		return SlidingRoomConfig{TimelineLimit: ref[0], RequiredState: state}
	}

	for roomID, ref := range in.Configs {
		s.Configs[roomID] = config(ref)
	}

	for roomID, ref := range in.Subscriptions {
		s.Subscriptions[roomID] = config(ref)
	}

	return nil
}
