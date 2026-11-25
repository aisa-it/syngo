package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Сборка ответа simplified sliding sync: какие комнаты в окнах
// списков, что по ним отдать и что запомнить в состоянии соединения. Порядок и
// правила - как SlidingSyncHandler.current_sync_for_user и get_room_sync_data в
// Synapse 1.165. Число запросов к БД не зависит от числа комнат: данные всех
// комнат читаются пачками.

// RoomSummary - сводка комнаты: число участников join и invite и до пяти героев
// (участники, кроме самого пользователя), по которым клиент называет комнату без
// имени.
type RoomSummary struct {
	Joined  int
	Invited int
	Heroes  []string
}

// SlidingActivity - последние позиции комнаты не позже верхней границы ответа.
type SlidingActivity struct {
	// LastEvent - последнее событие любого типа: были ли изменения после pos.
	LastEvent int64
	// LastBump - последнее событие из SlidingBumpEventTypes.
	LastBump int64
}

// SlidingRoomInfo - то, по чему фильтруются списки: тип и шифрование комнаты.
type SlidingRoomInfo struct {
	// Known - есть m.room.create (состояние комнаты на сервере известно).
	Known     bool
	RoomType  *string
	Encrypted bool
}

// TimelineWindow - выборка timeline комнаты: последние Limit событий из
// (Since, Upper].
type TimelineWindow struct {
	RoomID string
	Since  int64
	Upper  int64
	Limit  int
}

// StateKeyRef - ключ текущего состояния комнаты.
type StateKeyRef struct {
	RoomID   string
	Type     string
	StateKey string
}

// StateKeysRequest - выборка текущего состояния комнат одним запросом: точные
// ключи, типы целиком и комнаты целиком.
type StateKeysRequest struct {
	Exact []StateKeyRef
	// Types - (комната, тип): StateKey не используется.
	Types []StateKeyRef
	All   []string
}

func (r *StateKeysRequest) empty() bool {
	return len(r.Exact) == 0 && len(r.Types) == 0 && len(r.All) == 0
}

// SlidingSyncStore - данные sliding sync сверх SyncDataStore (store.SyncStore).
type SlidingSyncStore interface {
	// LoadSlidingConnection - состояние соединения на позицию; чужая или
	// удаленная позиция - ErrUnknownPos. Остальные позиции соединения удаляются:
	// клиент подтвердил эту.
	LoadSlidingConnection(ctx context.Context, userID, deviceID, connID string, position int64) (*SlidingConnState, error)
	// SaveSlidingConnection сохраняет состояние новой позицией. prev == 0 -
	// соединение начинается заново (прежнее с тем же conn_id удаляется).
	SaveSlidingConnection(ctx context.Context, userID, deviceID, connID string, prev int64,
		state *SlidingConnState) (int64, error)
	SlidingRoomActivity(ctx context.Context, roomIDs []string, upper int64) (map[string]SlidingActivity, error)
	SlidingRoomInfo(ctx context.Context, roomIDs []string) (map[string]SlidingRoomInfo, error)
	// SpaceChildren - m.space.child пространств (с непустым via).
	SpaceChildren(ctx context.Context, spaceIDs []string) (map[string][]string, error)
	TimelineWindows(ctx context.Context, windows []TimelineWindow) (map[string][]Event, error)
	// StateChangedSince - события текущего состояния комнат с позицией после
	// since комнаты.
	StateChangedSince(ctx context.Context, since map[string]int64) (map[string][]Event, error)
	StateByKeys(ctx context.Context, req StateKeysRequest) (map[string][]Event, error)
	RoomSummaries(ctx context.Context, userID string, roomIDs []string) (map[string]RoomSummary, error)
}

// SlidingAccountData - account data для is_dm (m.direct) и фильтров по тегам.
type SlidingAccountData interface {
	AccountData(ctx context.Context, userID, roomID, eventType string) ([]byte, error)
	RoomAccountDataOfType(ctx context.Context, userID, eventType string) (map[string][]byte, error)
}

// SlidingSyncService - доменный сервис simplified sliding sync. Переиспользует
// слой данных обычного /sync: членства (SyncDataStore), состояние на позицию
// выхода (SyncStateStore), видимость истории.
type SlidingSyncService struct {
	data     SyncDataStore
	state    SyncStateStore
	store    SlidingSyncStore
	accounts SlidingAccountData
	vis      *VisibilityService
}

// NewSlidingSyncService создает сервис; vis может быть nil (без фильтра видимости).
func NewSlidingSyncService(
	data SyncDataStore, state SyncStateStore, store SlidingSyncStore, accounts SlidingAccountData, vis *VisibilityService,
) *SlidingSyncService {
	return &SlidingSyncService{data: data, state: state, store: store, accounts: accounts, vis: vis}
}

// LoadConnection - состояние соединения на pos; pos без сохраненного состояния -
// пустое.
func (s *SlidingSyncService) LoadConnection(
	ctx context.Context, userID, deviceID, connID string, pos SlidingPos,
) (*SlidingConnState, error) {
	if pos.Conn == 0 {
		return NewSlidingConnState(), nil
	}

	return s.store.LoadSlidingConnection(ctx, userID, deviceID, connID, pos.Conn)
}

// SaveConnection сохраняет измененное состояние и возвращает его позицию; без
// изменений - prev.
func (s *SlidingSyncService) SaveConnection(
	ctx context.Context, userID, deviceID, connID string, prev int64, state *SlidingConnState,
) (int64, error) {
	if !state.Dirty() {
		return prev, nil
	}

	return s.store.SaveSlidingConnection(ctx, userID, deviceID, connID, prev, state)
}

// SlidingSyncInput - запрос к сервису.
type SlidingSyncInput struct {
	UserID  string
	Request *SlidingRequest
	// From - pos запроса; nil - первый запрос соединения.
	From *SlidingPos
	// Upper - верхняя граница ответа (позиция потока).
	Upper int64
	Prev  *SlidingConnState
	// NeedLeavers - собрать участников, вышедших из join-комнат после pos
	// (device_lists.left расширения e2ee).
	NeedLeavers bool
}

// SlidingListResult - список ответа: число комнат после фильтров и окна.
type SlidingListResult struct {
	Count int
	Ops   []SlidingListOp
}

// SlidingHero - герой комнаты без имени.
type SlidingHero struct {
	UserID      string
	DisplayName string
	AvatarURL   string
}

// SlidingRoomResult - данные комнаты для ответа.
type SlidingRoomResult struct {
	RoomID     string
	Membership string
	// Name, Avatar и Heroes - только при первой отдаче или изменении.
	Name   string
	Avatar string
	Heroes []SlidingHero
	// Initial - комната отдается этому соединению с нуля.
	Initial bool
	// ExpandedTimeline - timeline_limit вырос и timeline отдан без нижней
	// границы (unstable_expanded_timeline Synapse, нужен Element X).
	ExpandedTimeline bool
	RequiredState    []Event
	// HasTimeline - timeline запрашивался (не приглашение и timeline_limit > 0);
	// тогда в ответе есть limited, prev_batch и num_live.
	HasTimeline bool
	Timeline    []Event
	Limited     bool
	// PrevBatch - позиция токена prev_batch (события строго раньше нее).
	PrevBatch int64
	// HasNumLive - num_live отдается только в incremental.
	HasNumLive bool
	NumLive    int
	BumpStamp  int64
	// Counts - joined_count и invited_count (только join, при первой отдаче или
	// изменении членства).
	Counts *RoomSummary
	IsDM   bool
	// InviteState - состояние приглашенной комнаты: транспорт отбирает из него
	// stripped state.
	InviteState []Event
}

// HasUpdates - комнату нужно отдать: в incremental пустые комнаты не отдаются
// (RoomResult.__bool__ Synapse; bump_stamp и счетчики непрочитанного не в счет).
func (r *SlidingRoomResult) HasUpdates() bool {
	return r.Initial || r.Name != "" || r.Avatar != "" || len(r.Heroes) > 0 || r.Counts != nil ||
		len(r.RequiredState) > 0 || len(r.Timeline) > 0 || len(r.InviteState) > 0
}

// SlidingSyncOutput - результат сборки.
type SlidingSyncOutput struct {
	Lists map[string]SlidingListResult
	Rooms map[string]*SlidingRoomResult
	// Relevant - комнаты окон и подписок: по ним работают расширения.
	Relevant map[string]bool
	// All - комнаты всех списков (и вне окон) и подписок.
	All         map[string]bool
	Memberships map[string]RoomMembership
	NewlyJoined map[string]bool
	// Left - комнаты, из которых пользователь вышел после pos.
	Left []string
	// Leavers - участники, вышедшие из join-комнат пользователя после pos.
	Leavers []string
	// Visibility - границы видимости комнат с timeline (агрегации).
	Visibility map[string]Visibility
	// State - новое состояние соединения.
	State *SlidingConnState
}

// Compute собирает ответ: списки, комнаты и новое состояние соединения.
func (s *SlidingSyncService) Compute(ctx context.Context, in SlidingSyncInput) (*SlidingSyncOutput, error) {
	prev := in.Prev
	if prev == nil {
		prev = NewSlidingConnState()
	}

	c := &slidingCompute{
		s: s, in: in, prev: prev,
		out: &SlidingSyncOutput{
			Lists: map[string]SlidingListResult{}, Rooms: map[string]*SlidingRoomResult{},
			Relevant: map[string]bool{}, All: map[string]bool{}, Memberships: map[string]RoomMembership{},
			NewlyJoined: map[string]bool{}, State: prev.Clone(),
		},
		relevant: map[string]SlidingRoomConfig{},
	}

	if in.From != nil {
		c.incremental = true
		c.since = min(in.From.Stream, in.Upper)
	}

	// Подписки «липкие» (см. SlidingRequest.Subscriptions): действуют сохраненные
	// соединением плюс новые из запроса.
	if in.Request != nil {
		req := *in.Request
		req.Subscriptions = in.Request.EffectiveSubscriptions(prev.Subscriptions)
		c.in.Request = &req
		c.out.State.SetSubscriptions(req.Subscriptions)
	}

	if err := c.run(ctx); err != nil {
		return nil, err
	}

	return c.out, nil
}

// slidingCompute - промежуточные данные одной сборки.
type slidingCompute struct {
	s           *SlidingSyncService
	in          SlidingSyncInput
	prev        *SlidingConnState
	out         *SlidingSyncOutput
	incremental bool
	since       int64

	rooms map[string]*SlidingRoom
	// peek - подписки на комнаты без членства, которые можно просмотреть до
	// входа (публичные или world_readable).
	peek     map[string]bool
	activity map[string]SlidingActivity
	relevant map[string]SlidingRoomConfig
	plans    []*slidingPlan
	deltas   map[string][]Event
}

// slidingPlan - что отдать по комнате.
type slidingPlan struct {
	roomID     string
	membership RoomMembership
	cfg        SlidingRoomConfig
	prevCfg    SlidingRoomConfig
	hasPrevCfg bool
	initial    bool
	expanded   bool
	fromBound  int64
	toBound    int64

	timeline    []Event
	hasTimeline bool
	limited     bool
	members     []string
	summary     *RoomSummary
	nameChanged bool
	avatarChg   bool
	memberChg   bool
	// keys - прочитанное состояние комнаты; в incremental из него отдается только
	// дочитанное по addAll/addTypes/addKeys, остальное (имя и герои) - для полей
	// комнаты.
	keys     map[[2]string]*Event
	addAll   bool
	addTypes map[string]bool
	addKeys  map[[2]string]bool
}

func (c *slidingCompute) run(ctx context.Context) error {
	memberships, err := c.s.data.MembershipRooms(ctx, c.in.UserID)
	if err != nil {
		return fmt.Errorf("sliding sync: memberships: %w", err)
	}

	for i := range memberships {
		m := memberships[i]
		c.out.Memberships[m.RoomID] = m

		if !c.incremental || m.LeaveStream <= c.since || m.LeaveStream > c.in.Upper {
			continue
		}

		switch m.Membership {
		case msJoin:
			c.out.NewlyJoined[m.RoomID] = true
		case msLeave, msBan:
			c.out.Left = append(c.out.Left, m.RoomID)
		}
	}

	sort.Strings(c.out.Left)

	req := c.in.Request
	if req != nil && (len(req.Lists) > 0 || len(req.Subscriptions) > 0) {
		if err := c.roomLists(ctx); err != nil {
			return err
		}
	}

	if c.in.NeedLeavers && c.incremental {
		if err := c.leavers(ctx); err != nil {
			return err
		}
	}

	return nil
}

// listCandidate - комната может быть в списках: все, кроме своего выхода вне
// окна и забытых (filter_membership_for_sync Synapse). Кик (leave от другого) и
// бан остаются, пока комнату не забудут.
func (c *slidingCompute) listCandidate(m *RoomMembership) bool {
	if m.Membership == msJoin {
		return true
	}

	if m.Forgotten {
		return false
	}

	if m.Membership != msLeave {
		return true
	}

	newlyLeft := c.incremental && m.LeaveStream > c.since

	return newlyLeft || (m.Sender != "" && m.Sender != c.in.UserID)
}

// roomLists - списки, подписки и комнаты ответа.
func (c *slidingCompute) roomLists(ctx context.Context) error {
	req := c.in.Request
	c.rooms = map[string]*SlidingRoom{}

	listIDs := make([]string, 0, len(c.out.Memberships))

	for roomID, m := range c.out.Memberships {
		if len(req.Lists) > 0 && c.listCandidate(&m) {
			listIDs = append(listIDs, roomID)
		}
	}

	sort.Strings(listIDs)

	ids := slices.Clone(listIDs)

	var noMembership []string

	for roomID := range req.Subscriptions {
		if _, ok := c.out.Memberships[roomID]; !ok {
			noMembership = append(noMembership, roomID)
		} else if !slices.Contains(listIDs, roomID) {
			ids = append(ids, roomID)
		}
	}

	if err := c.peekable(ctx, noMembership); err != nil {
		return err
	}

	for roomID := range c.peek {
		ids = append(ids, roomID)
	}

	if len(ids) == 0 {
		c.emptyLists()

		return nil
	}

	activity, err := c.s.store.SlidingRoomActivity(ctx, ids, c.in.Upper)
	if err != nil {
		return fmt.Errorf("sliding sync: room activity: %w", err)
	}

	c.activity = activity

	for _, roomID := range ids {
		m := c.out.Memberships[roomID]
		c.rooms[roomID] = &SlidingRoom{
			RoomID: roomID, Membership: m.Membership, MembershipPos: m.LeaveStream,
			LastBump: activity[roomID].LastBump,
		}
	}

	if err := c.decorate(ctx, listIDs); err != nil {
		return err
	}

	c.lists(listIDs)
	c.subscriptions()
	c.selectRooms()

	return c.buildRooms(ctx)
}

// peekable отбирает подписки без членства, которые можно показать до входа:
// join_rules public или history_visibility world_readable. Element Web,
// открывая такую комнату (каталог, ссылка), подписывается на нее и ждет ее в
// ответе, иначе висит «Loading...»; комната отдается stripped state без
// timeline, как приглашение, и клиент показывает кнопку входа. В Synapse
// такие подписки пока игнорируются (TODO world_readable).
func (c *slidingCompute) peekable(ctx context.Context, roomIDs []string) error {
	c.peek = map[string]bool{}

	if len(roomIDs) == 0 {
		return nil
	}

	sort.Strings(roomIDs)

	var req StateKeysRequest

	for _, roomID := range roomIDs {
		req.Exact = append(req.Exact,
			StateKeyRef{RoomID: roomID, Type: "m.room.join_rules"},
			StateKeyRef{RoomID: roomID, Type: "m.room.history_visibility"})
	}

	events, err := c.s.store.StateByKeys(ctx, req)
	if err != nil {
		return fmt.Errorf("sliding sync: peekable rooms: %w", err)
	}

	for roomID, state := range events {
		for i := range state {
			var content struct {
				JoinRule          string `json:"join_rule"`
				HistoryVisibility string `json:"history_visibility"`
			}

			_ = json.Unmarshal(state[i].Content, &content)

			if content.JoinRule == "public" || content.HistoryVisibility == "world_readable" {
				c.peek[roomID] = true
			}
		}
	}

	return nil
}

// emptyLists - у пользователя нет комнат: списки с нулем.
func (c *slidingCompute) emptyLists() {
	for name, list := range c.in.Request.Lists {
		c.out.Lists[name] = SlidingListResult{Ops: SlidingWindows(nil, list.Ranges)}
	}
}

// decorate - is_dm, теги, тип, шифрование и пространства для фильтров.
func (c *slidingCompute) decorate(ctx context.Context, listIDs []string) error {
	needInfo, needTags := false, false

	var spaces []string

	for _, list := range c.in.Request.Lists {
		needInfo = needInfo || list.Filters.NeedsRoomInfo()
		needTags = needTags || list.Filters.NeedsTags()

		if list.Filters != nil {
			for _, space := range list.Filters.Spaces {
				if !slices.Contains(spaces, space) {
					spaces = append(spaces, space)
				}
			}
		}
	}

	dm, err := c.dmRooms(ctx)
	if err != nil {
		return err
	}

	for roomID, room := range c.rooms {
		room.DM = dm[roomID]
	}

	if needInfo && len(listIDs) > 0 {
		info, err := c.s.store.SlidingRoomInfo(ctx, listIDs)
		if err != nil {
			return fmt.Errorf("sliding sync: room info: %w", err)
		}

		for roomID, ri := range info {
			if room := c.rooms[roomID]; room != nil {
				room.Known, room.RoomType, room.Encrypted = ri.Known, ri.RoomType, ri.Encrypted
			}
		}
	}

	if needTags {
		if err := c.tags(ctx); err != nil {
			return err
		}
	}

	if len(spaces) > 0 {
		return c.spaces(ctx, spaces)
	}

	return nil
}

// dmRooms - комнаты из m.direct (как Synapse: is_direct в членстве есть только у
// приглашенного).
func (c *slidingCompute) dmRooms(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}

	if c.s.accounts == nil {
		return out, nil
	}

	raw, err := c.s.accounts.AccountData(ctx, c.in.UserID, "", "m.direct")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return out, nil
		}

		return nil, fmt.Errorf("sliding sync: m.direct: %w", err)
	}

	// Битый m.direct - нет личных комнат (клиент пишет account data сам).
	var direct map[string]json.RawMessage

	_ = json.Unmarshal(raw, &direct)

	for _, value := range direct {
		var rooms []json.RawMessage
		if json.Unmarshal(value, &rooms) != nil {
			continue
		}

		for _, item := range rooms {
			var roomID string
			if json.Unmarshal(item, &roomID) == nil {
				out[roomID] = true
			}
		}
	}

	return out, nil
}

// tags - теги комнат из m.tag.
func (c *slidingCompute) tags(ctx context.Context) error {
	if c.s.accounts == nil {
		return nil
	}

	byRoom, err := c.s.accounts.RoomAccountDataOfType(ctx, c.in.UserID, AccountDataTag)
	if err != nil {
		return fmt.Errorf("sliding sync: tags: %w", err)
	}

	for roomID, raw := range byRoom {
		room := c.rooms[roomID]
		if room == nil {
			continue
		}

		var content struct {
			Tags map[string]json.RawMessage `json:"tags"`
		}

		if json.Unmarshal(raw, &content) != nil {
			continue
		}

		room.Tags = make(map[string]bool, len(content.Tags))
		for tag := range content.Tags {
			room.Tags[tag] = true
		}
	}

	return nil
}

// spaces - родительские пространства фильтра: только те, где пользователь
// состоит (вложенные пространства не обходятся, как требует MSC).
func (c *slidingCompute) spaces(ctx context.Context, spaces []string) error {
	joined := make([]string, 0, len(spaces))

	for _, space := range spaces {
		if m, ok := c.out.Memberships[space]; ok && m.Membership == msJoin {
			joined = append(joined, space)
		}
	}

	if len(joined) == 0 {
		return nil
	}

	children, err := c.s.store.SpaceChildren(ctx, joined)
	if err != nil {
		return fmt.Errorf("sliding sync: space children: %w", err)
	}

	for space, rooms := range children {
		for _, roomID := range rooms {
			room := c.rooms[roomID]
			if room == nil {
				continue
			}

			if room.Parents == nil {
				room.Parents = map[string]bool{}
			}

			room.Parents[space] = true
		}
	}

	return nil
}

// lists - фильтры, сортировка по bump_stamp и окна списков.
func (c *slidingCompute) lists(listIDs []string) {
	names := make([]string, 0, len(c.in.Request.Lists))
	for name := range c.in.Request.Lists {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		list := c.in.Request.Lists[name]
		filtered := make([]SlidingRoom, 0, len(listIDs))

		for _, roomID := range listIDs {
			if room := c.rooms[roomID]; list.Filters.Match(room) {
				filtered = append(filtered, *room)
			}
		}

		for i := range filtered {
			c.out.All[filtered[i].RoomID] = true
		}

		SortSlidingRooms(filtered)
		ops := SlidingWindows(filtered, list.Ranges)

		for _, op := range ops {
			for _, roomID := range op.RoomIDs {
				c.addRelevant(roomID, list.Config)
			}
		}

		c.out.Lists[name] = SlidingListResult{Count: len(filtered), Ops: ops}
	}
}

// subscriptions - room_subscriptions: только комнаты, где у пользователя есть
// членство (world_readable не поддерживается и в Synapse).
func (c *slidingCompute) subscriptions() {
	for roomID, cfg := range c.in.Request.Subscriptions {
		if c.rooms[roomID] == nil {
			continue
		}

		c.out.All[roomID] = true
		c.addRelevant(roomID, cfg)
	}
}

func (c *slidingCompute) addRelevant(roomID string, cfg SlidingRoomConfig) {
	if old, ok := c.relevant[roomID]; ok {
		cfg = old.Combine(cfg)
	}

	c.relevant[roomID] = cfg
	c.out.Relevant[roomID] = true
}

// updated - в комнате были события или изменилось членство после pos.
func (c *slidingCompute) updated(roomID string) bool {
	return c.activity[roomID].LastEvent > c.since || c.out.Memberships[roomID].LeaveStream > c.since
}

// selectRooms - какие комнаты окон отдавать (_filter_relevant_rooms_to_send
// Synapse) и что запомнить о комнатах вне окон.
func (c *slidingCompute) selectRooms() {
	ids := make([]string, 0, len(c.relevant))
	for roomID := range c.relevant {
		ids = append(ids, roomID)
	}

	sort.Strings(ids)

	send := make([]string, 0, len(ids))

	for _, roomID := range ids {
		cfg := c.relevant[roomID]

		if c.incremental {
			prevCfg, hasPrev := c.prev.Config(roomID)
			st, sent := c.prev.Status(SlidingStreamRooms, roomID)

			// Просмотр до входа отдается один раз: дальше клиент ждет входа.
			if c.peek[roomID] && sent {
				continue
			}

			expandedCfg := hasPrev && !prevCfg.Combine(cfg).Equal(prevCfg)

			if !expandedCfg && sent && st.Live && !c.updated(roomID) {
				continue
			}
		}

		send = append(send, roomID)
		c.plans = append(c.plans, c.plan(roomID, cfg))
	}

	if c.incremental {
		unsent := make([]string, 0)

		for roomID := range c.out.All {
			if !c.out.Relevant[roomID] && c.updated(roomID) {
				unsent = append(unsent, roomID)
			}
		}

		sort.Strings(unsent)
		c.out.State.RecordUnsent(SlidingStreamRooms, c.since, unsent...)
	}

	c.out.State.RecordSent(SlidingStreamRooms, send...)
}

// plan - границы timeline и признак первой отдачи комнаты.
func (c *slidingCompute) plan(roomID string, cfg SlidingRoomConfig) *slidingPlan {
	m := c.out.Memberships[roomID]
	p := &slidingPlan{roomID: roomID, membership: m, cfg: cfg, initial: true, toBound: c.in.Upper}

	if m.Membership == msLeave || m.Membership == msBan {
		p.toBound = min(c.in.Upper, m.LeaveStream)
	}

	p.prevCfg, p.hasPrevCfg = c.prev.Config(roomID)

	if c.incremental && !c.out.NewlyJoined[roomID] {
		if st, ok := c.prev.Status(SlidingStreamRooms, roomID); ok {
			p.initial = false
			p.fromBound = c.since

			if !st.Live {
				p.fromBound = st.Since
			}
		}

		// timeline_limit вырос (Element X открыл комнату из списка): timeline без
		// нижней границы и unstable_expanded_timeline, как Synapse.
		p.expanded = p.hasPrevCfg && p.prevCfg.TimelineLimit < cfg.TimelineLimit
	}

	p.hasTimeline = m.Membership != msInvite && !c.peek[roomID] && cfg.TimelineLimit > 0

	return p
}

// buildRooms читает данные отдаваемых комнат пачками и собирает результат.
func (c *slidingCompute) buildRooms(ctx context.Context) error {
	if len(c.plans) == 0 {
		return nil
	}

	if err := c.timelines(ctx); err != nil {
		return err
	}

	if err := c.loadDeltas(ctx); err != nil {
		return err
	}

	if err := c.summaries(ctx); err != nil {
		return err
	}

	if err := c.state(ctx); err != nil {
		return err
	}

	for _, p := range c.plans {
		room := c.assemble(p)
		if room.HasUpdates() || !c.incremental {
			c.out.Rooms[p.roomID] = room
		}
	}

	return nil
}

// timelines - последние события комнат одним запросом, затем фильтр видимости.
func (c *slidingCompute) timelines(ctx context.Context) error {
	windows := make([]TimelineWindow, 0, len(c.plans))

	for _, p := range c.plans {
		if !p.hasTimeline {
			continue
		}

		since := p.fromBound
		if p.initial || p.expanded {
			since = 0
		}

		// +1 событие: отличить «ровно limit» от «обрезано» (limited).
		windows = append(windows, TimelineWindow{RoomID: p.roomID, Since: since, Upper: p.toBound, Limit: p.cfg.TimelineLimit + 1})
	}

	if len(windows) == 0 {
		return nil
	}

	events, err := c.s.store.TimelineWindows(ctx, windows)
	if err != nil {
		return fmt.Errorf("sliding sync: timelines: %w", err)
	}

	withTimeline := make([]string, 0, len(windows))

	for _, p := range c.plans {
		if !p.hasTimeline {
			continue
		}

		p.timeline = events[p.roomID]
		if len(p.timeline) > p.cfg.TimelineLimit {
			p.timeline = p.timeline[len(p.timeline)-p.cfg.TimelineLimit:]
			p.limited = true
		}

		withTimeline = append(withTimeline, p.roomID)
	}

	if c.s.vis != nil {
		c.out.Visibility = c.s.vis.Cutoffs(ctx, c.in.UserID, withTimeline)
	}

	for _, p := range c.plans {
		if v, ok := c.out.Visibility[p.roomID]; ok && p.hasTimeline {
			p.timeline, _ = v.FilterVisible(c.in.UserID, p.timeline)
		}

		p.members = timelineMembers(p.timeline)
	}

	return nil
}

// loadDeltas - изменения состояния после нижней границы комнаты: join-комнаты
// одним запросом по текущему состоянию, выходы - по состоянию на момент выхода
// (вместе с первой отдачей таких комнат в state).
func (c *slidingCompute) loadDeltas(ctx context.Context) error {
	since := map[string]int64{}

	for _, p := range c.plans {
		if !p.initial && p.membership.Membership == msJoin {
			since[p.roomID] = p.fromBound
		}
	}

	if len(since) == 0 {
		return nil
	}

	deltas, err := c.s.store.StateChangedSince(ctx, since)
	if err != nil {
		return fmt.Errorf("sliding sync: state deltas: %w", err)
	}

	c.deltas = deltas

	return nil
}

// summaries - сводки join-комнат для героев и счетчиков: при первой отдаче и
// изменении членства.
func (c *slidingCompute) summaries(ctx context.Context) error {
	need := make([]string, 0)
	needed := map[string]bool{}

	for _, p := range c.plans {
		if p.membership.Membership != msJoin {
			continue
		}

		for i := range c.deltas[p.roomID] {
			switch c.deltas[p.roomID][i].Type {
			case evMember:
				p.memberChg = true
			case evName:
				p.nameChanged = true
			case evAvatar:
				p.avatarChg = true
			}
		}

		if p.initial || p.memberChg {
			need = append(need, p.roomID)
			needed[p.roomID] = true
		}
	}

	if len(need) == 0 {
		return nil
	}

	summaries, err := c.s.store.RoomSummaries(ctx, c.in.UserID, need)
	if err != nil {
		return fmt.Errorf("sliding sync: room summaries: %w", err)
	}

	for _, p := range c.plans {
		if needed[p.roomID] {
			summary := summaries[p.roomID]
			p.summary = &summary
		}
	}

	return nil
}

// state - нужное состояние комнат: текущее одним запросом по ключам, у выходов -
// на момент выхода.
func (c *slidingCompute) state(ctx context.Context) error {
	var req StateKeysRequest

	leave := map[string]int64{}
	leaveMembers := []string{c.in.UserID}
	leaveAllMembers := false

	for _, p := range c.plans {
		p.keys = map[[2]string]*Event{}

		switch {
		case p.membership.Membership == msInvite || c.peek[p.roomID]:
			c.inviteKeys(&req, p)
		case p.membership.Membership == msLeave || p.membership.Membership == msBan:
			leave[p.roomID] = p.membership.LeaveStream
			leaveAllMembers = leaveAllMembers || p.cfg.RequiredState.allMembers()
			leaveMembers = append(leaveMembers, p.cfg.RequiredState.ExplicitMembers(c.in.UserID)...)
			leaveMembers = append(leaveMembers, p.members...)
		default:
			if p.initial {
				c.initialKeys(&req, p)
			} else {
				c.incrementalKeys(&req, p)
			}
		}
	}

	if !req.empty() {
		events, err := c.s.store.StateByKeys(ctx, req)
		if err != nil {
			return fmt.Errorf("sliding sync: state: %w", err)
		}

		c.indexState(events)
	}

	if len(leave) == 0 {
		return nil
	}

	if leaveAllMembers {
		leaveMembers = nil
	}

	events, err := c.s.state.StateAtRoomsStreamMembers(ctx, leave, leaveMembers)
	if err != nil {
		return fmt.Errorf("sliding sync: leave state: %w", err)
	}

	c.indexState(events)

	return nil
}

func (c *slidingCompute) indexState(events map[string][]Event) {
	for _, p := range c.plans {
		for i := range events[p.roomID] {
			ev := &events[p.roomID][i]
			if ev.StateKey != nil {
				p.keys[[2]string{ev.Type, *ev.StateKey}] = ev
			}
		}
	}
}

// inviteKeys - stripped state приглашения (как filterInviteState обычного /sync):
// create, name, topic, avatar, join_rules, canonical_alias, encryption, свое
// членство и членство пригласившего.
func (c *slidingCompute) inviteKeys(req *StateKeysRequest, p *slidingPlan) {
	for _, eventType := range []string{
		"m.room.create", evName, "m.room.topic", evAvatar, "m.room.join_rules", "m.room.canonical_alias", "m.room.encryption",
	} {
		req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: eventType})
	}

	req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: evMember, StateKey: c.in.UserID})
	if p.membership.Sender != "" && p.membership.Sender != c.in.UserID {
		req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: evMember, StateKey: p.membership.Sender})
	}
}

// initialKeys - required_state, имя, аватар и герои комнаты при первой отдаче.
func (c *slidingCompute) initialKeys(req *StateKeysRequest, p *slidingPlan) {
	rs := p.cfg.RequiredState
	if rs.All() {
		req.All = append(req.All, p.roomID)

		return
	}

	add := func(eventType, stateKey string) {
		req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: eventType, StateKey: stateKey})
	}

	for eventType, keys := range rs {
		for _, key := range keys {
			switch key {
			case StateWildcard:
				req.Types = append(req.Types, StateKeyRef{RoomID: p.roomID, Type: eventType})
			case StateLazy:
				if eventType == evMember {
					for _, member := range p.members {
						add(evMember, member)
					}
				}
			case StateMe:
				add(eventType, c.in.UserID)
			default:
				add(eventType, key)
			}
		}
	}

	add(evName, "")
	add(evAvatar, "")
	c.heroKeys(req, p)
}

// heroKeys - членство героев (displayname, avatar_url).
func (c *slidingCompute) heroKeys(req *StateKeysRequest, p *slidingPlan) {
	if p.summary == nil {
		return
	}

	for _, hero := range p.summary.Heroes {
		req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: evMember, StateKey: hero})
	}
}

// incrementalKeys - что дочитать к изменениям: расширение required_state,
// lazy-участников, которых клиент еще не получал, и героев при изменении
// членства.
func (c *slidingCompute) incrementalKeys(req *StateKeysRequest, p *slidingPlan) {
	rs := p.cfg.RequiredState

	p.addTypes, p.addKeys = map[string]bool{}, map[[2]string]bool{}

	if p.hasPrevCfg && !p.prevCfg.RequiredState.Equal(rs) {
		added := rs.AddedSince(p.prevCfg.RequiredState, c.in.UserID)
		if added.All {
			p.addAll = true
			req.All = append(req.All, p.roomID)
		}

		for _, eventType := range added.Types {
			p.addTypes[eventType] = true
			req.Types = append(req.Types, StateKeyRef{RoomID: p.roomID, Type: eventType})
		}

		for _, pair := range added.Pairs {
			p.addKeys[pair] = true
			req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: pair[0], StateKey: pair[1]})
		}
	}

	if rs.Lazy() {
		for _, member := range c.newLazyMembers(p) {
			p.addKeys[[2]string{evMember, member}] = true
			req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: evMember, StateKey: member})
		}
	}

	if p.memberChg {
		req.Exact = append(req.Exact, StateKeyRef{RoomID: p.roomID, Type: evName})
		c.heroKeys(req, p)
	}
}

// newLazyMembers - участники timeline, чье членство клиент еще не получал и
// которое не придет изменением.
func (c *slidingCompute) newLazyMembers(p *slidingPlan) []string {
	known := map[string]bool{}

	for _, member := range c.prev.LazyMembers[p.roomID] {
		known[member] = true
	}

	for _, member := range p.cfg.RequiredState.ExplicitMembers(c.in.UserID) {
		known[member] = true
	}

	for i := range c.deltas[p.roomID] {
		if ev := c.deltas[p.roomID][i]; ev.Type == evMember && ev.StateKey != nil {
			known[*ev.StateKey] = true
		}
	}

	out := make([]string, 0, len(p.members))

	for _, member := range p.members {
		if !known[member] {
			out = append(out, member)
		}
	}

	return out
}

// assemble собирает результат комнаты и обновляет состояние соединения.
func (c *slidingCompute) assemble(p *slidingPlan) *SlidingRoomResult {
	room := &SlidingRoomResult{
		RoomID: p.roomID, Membership: p.membership.Membership, Initial: p.initial,
		ExpandedTimeline: p.expanded, HasTimeline: p.hasTimeline, Limited: p.limited,
		IsDM: c.rooms[p.roomID] != nil && c.rooms[p.roomID].DM,
	}

	if p.hasTimeline {
		room.Timeline = p.timeline
		room.PrevBatch = p.toBound + 1

		if len(p.timeline) > 0 {
			room.PrevBatch = p.timeline[0].StreamOrdering
		}

		if c.incremental {
			room.HasNumLive = true

			for i := len(p.timeline) - 1; i >= 0 && p.timeline[i].StreamOrdering > c.since; i-- {
				room.NumLive++
			}
		}
	}

	if p.membership.Membership == msInvite || c.peek[p.roomID] {
		room.InviteState = sortedState(p.keys)
		room.Name, room.Avatar = stateName(p.keys), stateAvatar(p.keys)
	} else {
		c.requiredState(p, room)
	}

	room.BumpStamp = c.bumpStamp(p)

	if p.membership.Membership == msJoin && p.summary != nil {
		room.Counts = &RoomSummary{Joined: p.summary.Joined, Invited: p.summary.Invited}
	}

	c.recordConfig(p)

	return room
}

// requiredState - required_state, имя, аватар и герои join- и покинутой комнаты.
func (c *slidingCompute) requiredState(p *slidingPlan, room *SlidingRoomResult) {
	rs := p.cfg.RequiredState
	lazy := map[string]bool{}

	for _, member := range p.members {
		lazy[member] = true
	}

	changes := p.keys

	if !p.initial {
		changes = map[[2]string]*Event{}

		// Изменения: у join - из текущего состояния, у выхода - состояние на
		// момент выхода новее нижней границы.
		source := c.deltas[p.roomID]
		if p.membership.Membership != msJoin {
			source = sortedState(p.keys)
		}

		for i := range source {
			if ev := &source[i]; ev.StateKey != nil && ev.StreamOrdering > p.fromBound {
				changes[[2]string{ev.Type, *ev.StateKey}] = ev
			}
		}

		if p.membership.Membership == msJoin {
			for key, ev := range p.keys {
				if _, ok := changes[key]; !ok && (p.addAll || p.addTypes[key[0]] || p.addKeys[key]) {
					changes[key] = ev
				}
			}
		}
	}

	// Без обрезки timeline отдаются все изменения членства (клиент, загрузивший
	// весь список, поддерживает его по ним), с обрезкой - только участников
	// timeline, как Synapse.
	allMemberChanges := !p.initial && !p.limited
	invalidated := map[string]bool{}
	sent := make([]string, 0)

	for key, ev := range changes {
		include := rs.Includes(key[0], key[1], c.in.UserID)
		if !include && key[0] == evMember && rs.Lazy() {
			include = lazy[key[1]] || allMemberChanges
		}

		switch {
		case include && key[0] == evMember:
			sent = append(sent, key[1])
			room.RequiredState = append(room.RequiredState, *ev)
		case include:
			room.RequiredState = append(room.RequiredState, *ev)
		case key[0] == evMember && !p.initial:
			invalidated[key[1]] = true
		}
	}

	sort.Strings(sent)

	sortEvents(room.RequiredState)

	if p.initial || p.nameChanged {
		room.Name = stateName(changes)
	}

	if p.initial || p.avatarChg {
		room.Avatar = stateAvatar(changes)
	}

	// Герои нужны комнате без имени (имя из прочитанного текущего состояния или
	// изменений).
	if p.membership.Membership == msJoin && p.summary != nil && stateName(p.keys) == "" && stateName(changes) == "" {
		room.Heroes = c.heroes(p)
	}

	c.recordLazy(p, sent, invalidated)
}

// heroes - герои с displayname и avatar_url из их членства.
func (c *slidingCompute) heroes(p *slidingPlan) []SlidingHero {
	out := make([]SlidingHero, 0, len(p.summary.Heroes))

	for _, userID := range p.summary.Heroes {
		hero := SlidingHero{UserID: userID}

		ev, ok := p.keys[[2]string{evMember, userID}]
		if !ok {
			for i := range c.deltas[p.roomID] {
				if d := &c.deltas[p.roomID][i]; d.Type == evMember && d.StateKey != nil && *d.StateKey == userID {
					ev, ok = d, true
				}
			}
		}

		if ok {
			var content struct {
				DisplayName string `json:"displayname"`
				AvatarURL   string `json:"avatar_url"`
			}

			_ = json.Unmarshal(ev.Content, &content)
			hero.DisplayName, hero.AvatarURL = content.DisplayName, content.AvatarURL
		}

		out = append(out, hero)
	}

	return out
}

// recordLazy - каких lazy-участников клиент теперь знает: участники этого
// timeline, отданное членство и прежние, кроме тех, чье членство изменилось и не
// отдано.
func (c *slidingCompute) recordLazy(p *slidingPlan, sent []string, invalidated map[string]bool) {
	if !p.cfg.RequiredState.Lazy() {
		c.out.State.SetLazyMembers(p.roomID, nil)

		return
	}

	explicit := p.cfg.RequiredState.ExplicitMembers(c.in.UserID)
	seen := map[string]bool{}
	out := make([]string, 0, len(p.members))

	add := func(member string) {
		if !seen[member] && !invalidated[member] && !slices.Contains(explicit, member) {
			seen[member] = true
			out = append(out, member)
		}
	}

	for _, member := range p.members {
		add(member)
	}

	for _, member := range sent {
		add(member)
	}

	if !p.initial {
		for _, member := range c.prev.LazyMembers[p.roomID] {
			add(member)
		}
	}

	c.out.State.SetLazyMembers(p.roomID, out)
}

// recordConfig - с какими параметрами комната отдана (правила Synapse: при
// расширении timeline, первой отдаче, сужении с обрезкой и смене required_state).
func (c *slidingCompute) recordConfig(p *slidingPlan) {
	if !p.hasPrevCfg || p.expanded || (p.limited && p.prevCfg.TimelineLimit > p.cfg.TimelineLimit) ||
		!p.prevCfg.RequiredState.Equal(p.cfg.RequiredState) {
		c.out.State.SetConfig(p.roomID, p.cfg)
	}
}

// bumpStamp - позиция последнего «значимого» события; для не-join - события
// членства.
func (c *slidingCompute) bumpStamp(p *slidingPlan) int64 {
	if p.membership.Membership != msJoin {
		return max(p.membership.LeaveStream, 0)
	}

	for i := len(p.timeline) - 1; i >= 0; i-- {
		if isBumpEvent(p.timeline[i].Type) && p.timeline[i].StreamOrdering > 0 {
			return p.timeline[i].StreamOrdering
		}
	}

	if room := c.rooms[p.roomID]; room != nil {
		return room.BumpStamp()
	}

	return max(p.membership.LeaveStream, 0)
}

// leavers - участники, вышедшие или забаненные в join-комнатах после pos (по
// изменениям текущего состояния; вернувшиеся не в счет).
func (c *slidingCompute) leavers(ctx context.Context) error {
	since := map[string]int64{}

	for roomID, m := range c.out.Memberships {
		if m.Membership == msJoin {
			since[roomID] = c.since
		}
	}

	if len(since) == 0 {
		return nil
	}

	deltas, err := c.s.store.StateChangedSince(ctx, since)
	if err != nil {
		return fmt.Errorf("sliding sync: leavers: %w", err)
	}

	seen := map[string]bool{}

	for roomID := range since {
		for i := range deltas[roomID] {
			ev := deltas[roomID][i]
			if ev.Type != evMember || ev.StateKey == nil || *ev.StateKey == c.in.UserID || ev.StreamOrdering <= c.since {
				continue
			}

			if ms := membershipOf(ev.Content); (ms == msLeave || ms == msBan) && !seen[*ev.StateKey] {
				seen[*ev.StateKey] = true
				c.out.Leavers = append(c.out.Leavers, *ev.StateKey)
			}
		}
	}

	sort.Strings(c.out.Leavers)

	return nil
}

// timelineMembers - отправители событий timeline и цели m.room.member (по ним
// $LAZY отдает членство).
func timelineMembers(events []Event) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(events))

	add := func(userID string) {
		if userID != "" && !seen[userID] {
			seen[userID] = true
			out = append(out, userID)
		}
	}

	for i := range events {
		add(events[i].Sender)

		if events[i].Type == evMember && events[i].StateKey != nil {
			add(*events[i].StateKey)
		}
	}

	return out
}

// sortedState - события состояния по (тип, ключ).
func sortedState(keys map[[2]string]*Event) []Event {
	out := make([]Event, 0, len(keys))
	for _, ev := range keys {
		out = append(out, *ev)
	}

	sortEvents(out)

	return out
}

func sortEvents(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Type != events[j].Type {
			return events[i].Type < events[j].Type
		}

		return stateKeyOf(&events[i]) < stateKeyOf(&events[j])
	})
}

func stateKeyOf(ev *Event) string {
	if ev.StateKey == nil {
		return ""
	}

	return *ev.StateKey
}

// stateName - имя комнаты из m.room.name (пустое и не строка - нет имени).
func stateName(keys map[[2]string]*Event) string {
	ev, ok := keys[[2]string{evName, ""}]
	if !ok {
		return ""
	}

	var content struct {
		Name any `json:"name"`
	}

	_ = json.Unmarshal(ev.Content, &content)
	name, _ := content.Name.(string)

	return name
}

// stateAvatar - url из m.room.avatar.
func stateAvatar(keys map[[2]string]*Event) string {
	ev, ok := keys[[2]string{evAvatar, ""}]
	if !ok {
		return ""
	}

	var content struct {
		URL any `json:"url"`
	}

	_ = json.Unmarshal(ev.Content, &content)
	url, _ := content.URL.(string)

	return url
}
