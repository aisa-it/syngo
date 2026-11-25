package api

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/aisa-it/syngo/internal/core"
)

// Сводка комнаты и доступ к ней для /hierarchy - клиентского и федеративного.
// Ориентир - synapse/handlers/room_summary.py: _build_room_entry,
// _summarize_local_room, _is_local_room_accessible, _child_events_comparison_key.

const (
	// hierarchyMaxChildrenPerSpace - предел детей пространства в федеративном
	// ответе (MAX_ROOMS_PER_SPACE в Synapse).
	hierarchyMaxChildrenPerSpace = 50
	// hierarchyMaxOrderLen - order длиннее не учитывается (спека m.space.child).
	hierarchyMaxOrderLen = 50
	// roomTypeSpace - room_type пространства в m.room.create.
	roomTypeSpace = "m.space"
)

// spaceSummary - комната в ответе /hierarchy без children_state
// (SpaceHierarchyChildRoomsChunk федерации, поля PublicRoomsChunk).
type spaceSummary struct {
	RoomID           string `json:"room_id"`
	Name             string `json:"name,omitempty"`
	Topic            string `json:"topic,omitempty"`
	AvatarURL        string `json:"avatar_url,omitempty"`
	CanonicalAlias   string `json:"canonical_alias,omitempty"`
	NumJoinedMembers int    `json:"num_joined_members"`
	WorldReadable    bool   `json:"world_readable"`
	GuestCanJoin     bool   `json:"guest_can_join"`
	JoinRule         string `json:"join_rule,omitempty"`
	RoomType         string `json:"room_type,omitempty"`
	RoomVersion      string `json:"room_version,omitempty"`
	Encryption       string `json:"encryption,omitempty"`
	// AllowedRoomIDs - комнаты из allow restricted-комнаты: только в ответе
	// федерации, чтобы запросивший сервер сам проверил доступ своих пользователей.
	AllowedRoomIDs []string `json:"allowed_room_ids,omitempty"`
}

// spaceChildEvent - stripped m.space.child (StrippedChildStateEvent спеки).
type spaceChildEvent struct {
	Type           string          `json:"type"`
	StateKey       string          `json:"state_key"`
	Content        json.RawMessage `json:"content"`
	Sender         string          `json:"sender"`
	OriginServerTS int64           `json:"origin_server_ts"`
}

// spaceRoom - комната с m.space.child: элемент rooms клиентского ответа и room
// федеративного. У комнаты не-пространства children_state пуст.
type spaceRoom struct {
	spaceSummary

	ChildrenState []spaceChildEvent `json:"children_state"`
}

// hierarchyRoomState - текущее состояние комнаты для сводки и проверки доступа.
// Пустое - комната нам неизвестна.
type hierarchyRoomState struct {
	roomID string
	events []core.Event
}

// known - у нас есть состояние комнаты.
func (st hierarchyRoomState) known() bool { return len(st.events) > 0 }

// event - событие состояния по ключу.
func (st hierarchyRoomState) event(eventType, stateKey string) (core.Event, bool) {
	for i := range st.events {
		ev := st.events[i]
		if ev.Type == eventType && ev.StateKey != nil && *ev.StateKey == stateKey {
			return ev, true
		}
	}

	return core.Event{}, false
}

// field - строковое поле content события состояния (type, "").
func (st hierarchyRoomState) field(eventType, name string) string {
	ev, ok := st.event(eventType, "")
	if !ok {
		return ""
	}

	return contentString(ev.Content, name)
}

// contentString - строковое поле JSON-объекта; не строка или битый JSON - "".
func contentString(content json.RawMessage, name string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return ""
	}

	var value string
	if err := json.Unmarshal(fields[name], &value); err != nil {
		return ""
	}

	return value
}

// membership - членство пользователя в текущем состоянии ("" - нет события).
func (st hierarchyRoomState) membership(userID string) string {
	ev, ok := st.event("m.room.member", userID)
	if !ok {
		return ""
	}

	return contentString(ev.Content, "membership")
}

// hostHas - у сервера есть участник с одним из членств.
func (st hierarchyRoomState) hostHas(server string, memberships ...string) bool {
	for i := range st.events {
		ev := st.events[i]
		if ev.Type != "m.room.member" || ev.StateKey == nil || domainOf(*ev.StateKey) != server {
			continue
		}

		if slices.Contains(memberships, contentString(ev.Content, "membership")) {
			return true
		}
	}

	return false
}

// joinedCount - число участников с membership=join.
func (st hierarchyRoomState) joinedCount() int {
	n := 0

	for i := range st.events {
		ev := st.events[i]
		if ev.Type == "m.room.member" && ev.StateKey != nil && contentString(ev.Content, "membership") == "join" {
			n++
		}
	}

	return n
}

// allowedRoomIDs - комнаты из allow (type m.room_membership) restricted и
// knock_restricted комнаты; иначе nil (get_rooms_that_allow_join в Synapse).
func (st hierarchyRoomState) allowedRoomIDs() []string {
	ev, ok := st.event("m.room.join_rules", "")
	if !ok {
		return nil
	}

	var content struct {
		JoinRule string            `json:"join_rule"`
		Allow    []json.RawMessage `json:"allow"`
	}

	if err := json.Unmarshal(ev.Content, &content); err != nil {
		return nil
	}

	if content.JoinRule != "restricted" && content.JoinRule != "knock_restricted" {
		return nil
	}

	var rooms []string

	for _, raw := range content.Allow {
		if contentString(raw, "type") != "m.room_membership" {
			continue
		}

		if roomID := contentString(raw, "room_id"); roomID != "" {
			rooms = append(rooms, roomID)
		}
	}

	return rooms
}

// summary собирает сводку комнаты (_build_room_entry в Synapse). allowed_room_ids -
// только для федерации.
func (st hierarchyRoomState) summary(forFederation bool) spaceSummary {
	summary := spaceSummary{
		RoomID:           st.roomID,
		Name:             st.field("m.room.name", "name"),
		Topic:            st.field("m.room.topic", "topic"),
		AvatarURL:        st.field("m.room.avatar", "url"),
		CanonicalAlias:   st.field("m.room.canonical_alias", "alias"),
		NumJoinedMembers: st.joinedCount(),
		WorldReadable:    st.field("m.room.history_visibility", "history_visibility") == "world_readable",
		GuestCanJoin:     st.field("m.room.guest_access", "guest_access") == "can_join",
		JoinRule:         st.field("m.room.join_rules", "join_rule"),
		RoomType:         st.field("m.room.create", "type"),
		RoomVersion:      st.field("m.room.create", "room_version"),
		Encryption:       st.field("m.room.encryption", "algorithm"),
	}

	// Без room_version в m.room.create - версия 1 (спека).
	if _, ok := st.event("m.room.create", ""); ok && summary.RoomVersion == "" {
		summary.RoomVersion = "1"
	}

	if forFederation {
		summary.AllowedRoomIDs = st.allowedRoomIDs()
	}

	return summary
}

// spaceChildren - m.space.child пространства с непустым via из строк, по
// порядку спеки: с валидным order раньше, по order, затем по origin_server_ts.
// suggestedOnly оставляет только suggested=true.
func (st hierarchyRoomState) spaceChildren(suggestedOnly bool) []spaceChildEvent {
	type child struct {
		event spaceChildEvent
		order string
		valid bool
	}

	var children []child

	for i := range st.events {
		ev := st.events[i]
		if ev.Type != "m.space.child" || ev.StateKey == nil || *ev.StateKey == "" {
			continue
		}

		var content struct {
			Via       []json.RawMessage `json:"via"`
			Order     any               `json:"order"`
			Suggested any               `json:"suggested"`
		}

		if err := json.Unmarshal(ev.Content, &content); err != nil || !validVia(content.Via) {
			continue
		}

		if suggestedOnly && content.Suggested != true {
			continue
		}

		order, valid := spaceChildOrder(content.Order)
		children = append(children, child{
			event: spaceChildEvent{
				Type: ev.Type, StateKey: *ev.StateKey, Content: ev.Content,
				Sender: ev.Sender, OriginServerTS: ev.OriginServerTS,
			},
			order: order, valid: valid,
		})
	}

	slices.SortStableFunc(children, func(a, b child) int {
		if a.valid != b.valid {
			if a.valid {
				return -1
			}

			return 1
		}

		return cmp.Or(
			cmp.Compare(a.order, b.order),
			cmp.Compare(a.event.OriginServerTS, b.event.OriginServerTS),
			cmp.Compare(a.event.StateKey, b.event.StateKey),
		)
	})

	out := make([]spaceChildEvent, 0, len(children))
	for _, c := range children {
		out = append(out, c.event)
	}

	return out
}

// validVia - via непустой список строк (_has_valid_via в Synapse); иначе
// m.space.child считается удаленным.
func validVia(via []json.RawMessage) bool {
	if len(via) == 0 {
		return false
	}

	for _, raw := range via {
		var server string
		if err := json.Unmarshal(raw, &server); err != nil {
			return false
		}
	}

	return true
}

// spaceChildOrder - order m.space.child, если он валиден по спеке: строка до 50
// символов из диапазона 0x20-0x7E.
func spaceChildOrder(value any) (string, bool) {
	order, ok := value.(string)
	if !ok || len(order) > hierarchyMaxOrderLen {
		return "", false
	}

	for i := range len(order) {
		if order[i] < 0x20 || order[i] > 0x7e {
			return "", false
		}
	}

	return order, true
}

// hierarchyWalker - чтение состояния комнат на время одного запроса /hierarchy
// (одна комната нужна несколько раз: узел, ребенок, комната из allow).
type hierarchyWalker struct {
	s      *Server
	states map[string]hierarchyRoomState
}

func (s *Server) newHierarchyWalker() *hierarchyWalker {
	return &hierarchyWalker{s: s, states: map[string]hierarchyRoomState{}}
}

// state - текущее состояние комнаты (пустое - неизвестна).
func (w *hierarchyWalker) state(ctx context.Context, roomID string) (hierarchyRoomState, error) {
	if st, ok := w.states[roomID]; ok {
		return st, nil
	}

	events, err := w.s.rooms.CurrentState(ctx, roomID)
	if err != nil {
		return hierarchyRoomState{}, err
	}

	st := hierarchyRoomState{roomID: roomID, events: events}
	w.states[roomID] = st

	return st, nil
}

// hostJoined - наш сервер в комнате: сводку даем по своему состоянию, иначе
// спрашиваем по федерации (is_host_joined в Synapse).
func (w *hierarchyWalker) hostJoined(st hierarchyRoomState) bool {
	return st.hostHas(w.s.cfg.Matrix.ServerName, "join")
}

// localEntry - сводка комнаты по нашему состоянию; m.space.child - только у
// пространства и при includeChildren (_summarize_local_room в Synapse).
func (w *hierarchyWalker) localEntry(st hierarchyRoomState, suggestedOnly, includeChildren, forFederation bool) spaceRoom {
	room := spaceRoom{spaceSummary: st.summary(forFederation), ChildrenState: []spaceChildEvent{}}

	if includeChildren && room.RoomType == roomTypeSpace {
		room.ChildrenState = st.spaceChildren(suggestedOnly)
	}

	return room
}

// localAccessible - комнату можно показать пользователю userID или серверу
// origin (_is_local_room_accessible в Synapse): public/knock/knock_restricted,
// world_readable; пользователю - участник или приглашенный, либо участник
// комнаты из allow restricted; серверу - есть участник или приглашенный, либо
// участник комнаты из allow. Неизвестная комната недоступна.
func (w *hierarchyWalker) localAccessible(ctx context.Context, st hierarchyRoomState, userID, origin string) (bool, error) {
	if !st.known() {
		return false, nil
	}

	switch st.field("m.room.join_rules", "join_rule") {
	case "public", "knock", "knock_restricted":
		return true, nil
	}

	if st.field("m.room.history_visibility", "history_visibility") == "world_readable" {
		return true, nil
	}

	switch {
	case userID != "":
		if membership := st.membership(userID); membership == "join" || membership == "invite" {
			return true, nil
		}

		return w.userInAny(ctx, st.allowedRoomIDs(), userID)
	case origin != "":
		if st.hostHas(origin, "join", "invite") {
			return true, nil
		}

		for _, roomID := range st.allowedRoomIDs() {
			allowed, err := w.state(ctx, roomID)
			if err != nil {
				return false, err
			}

			if allowed.hostHas(origin, "join") {
				return true, nil
			}
		}
	}

	return false, nil
}

// userInAny - пользователь участник одной из комнат (по нашему состоянию).
func (w *hierarchyWalker) userInAny(ctx context.Context, roomIDs []string, userID string) (bool, error) {
	for _, roomID := range roomIDs {
		st, err := w.state(ctx, roomID)
		if err != nil {
			return false, err
		}

		if st.membership(userID) == "join" {
			return true, nil
		}
	}

	return false, nil
}

// serverAllowed - m.room.server_acl комнаты пускает origin (нет ACL - пускает).
func (st hierarchyRoomState) serverAllowed(origin string) bool {
	ev, ok := st.event("m.room.server_acl", "")
	if !ok {
		return true
	}

	return serverACLAllows(ev.Content, origin)
}
