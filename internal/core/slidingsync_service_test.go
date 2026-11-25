package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// memSliding - in-memory хранилища SlidingSyncService: лента событий всех
// комнат с позициями, состояние выводится из нее.
type memSliding struct {
	events  []core.Event
	account map[string][]byte // user|room|type
	conns   map[int64][]byte
	connKey map[int64]string
	nextPos int64
	queries int
}

func newMemSliding() *memSliding {
	return &memSliding{account: map[string][]byte{}, conns: map[int64][]byte{}, connKey: map[int64]string{}}
}

func (m *memSliding) add(roomID, eventType, sender string, stateKey *string, content string) core.Event {
	ev := core.Event{
		EventID: fmt.Sprintf("$e%d", len(m.events)+1), RoomID: roomID, Type: eventType, Sender: sender,
		StateKey: stateKey, Content: json.RawMessage(content), StreamOrdering: int64(len(m.events) + 1),
	}
	m.events = append(m.events, ev)

	return ev
}

func (m *memSliding) position() int64 { return int64(len(m.events)) }

func (m *memSliding) createRoom(roomID, creator, extra string) {
	empty := ""
	m.add(roomID, "m.room.create", creator, &empty, `{"creator":"`+creator+`"`+extra+`}`)
	m.member(roomID, creator, creator, "join", "")
}

func (m *memSliding) member(roomID, sender, target, membership, name string) core.Event {
	content := `{"membership":"` + membership + `"`
	if name != "" {
		content += `,"displayname":"` + name + `"`
	}

	return m.add(roomID, "m.room.member", sender, &target, content+`}`)
}

func (m *memSliding) state(roomID, eventType, sender, stateKey, content string) core.Event {
	return m.add(roomID, eventType, sender, &stateKey, content)
}

func (m *memSliding) message(roomID, sender, body string) core.Event {
	return m.add(roomID, "m.room.message", sender, nil, `{"msgtype":"m.text","body":"`+body+`"}`)
}

// stateAt - состояние комнаты на позицию (последнее событие по ключу).
func (m *memSliding) stateAt(roomID string, pos int64) map[[2]string]core.Event {
	out := map[[2]string]core.Event{}

	for i := range m.events {
		ev := m.events[i]
		if ev.RoomID == roomID && ev.StateKey != nil && ev.StreamOrdering <= pos {
			out[[2]string{ev.Type, *ev.StateKey}] = ev
		}
	}

	return out
}

func membershipOfContent(content []byte) string {
	var body struct {
		Membership string `json:"membership"`
	}

	_ = json.Unmarshal(content, &body)

	return body.Membership
}

func (m *memSliding) rooms() []string {
	seen := map[string]bool{}
	out := []string{}

	for i := range m.events {
		if !seen[m.events[i].RoomID] {
			seen[m.events[i].RoomID] = true
			out = append(out, m.events[i].RoomID)
		}
	}

	return out
}

func (m *memSliding) MembershipRooms(_ context.Context, userID string) ([]core.RoomMembership, error) {
	m.queries++

	out := []core.RoomMembership{}

	for _, roomID := range m.rooms() {
		ev, ok := m.stateAt(roomID, m.position())[[2]string{"m.room.member", userID}]
		if !ok {
			continue
		}

		out = append(out, core.RoomMembership{
			RoomID: roomID, Membership: membershipOfContent(ev.Content), LeaveStream: ev.StreamOrdering, Sender: ev.Sender,
		})
	}

	return out, nil
}

func (m *memSliding) EventsSince(context.Context, []string, int64, int, map[string]int64) (map[string][]core.Event, error) {
	return nil, nil
}

func (m *memSliding) StateForRooms(context.Context, []string) (map[string][]core.Event, error) {
	return nil, nil
}

func (m *memSliding) StateDeltaForRooms(context.Context, []string, int64, []string) (map[string][]core.Event, error) {
	return nil, nil
}

func (m *memSliding) StateMembersForRooms(context.Context, []string, []string) (map[string][]core.Event, error) {
	return nil, nil
}

func (m *memSliding) StateAtRoomsStream(ctx context.Context, positions map[string]int64) (map[string][]core.Event, error) {
	return m.StateAtRoomsStreamMembers(ctx, positions, nil)
}

func (m *memSliding) StateAtRoomsStreamMembers(
	_ context.Context, positions map[string]int64, members []string,
) (map[string][]core.Event, error) {
	m.queries++

	out := map[string][]core.Event{}

	for roomID, pos := range positions {
		state := m.stateAt(roomID, pos)
		for key := range state {
			if members == nil || key[0] != "m.room.member" || slices.Contains(members, key[1]) {
				out[roomID] = append(out[roomID], state[key])
			}
		}
	}

	return out, nil
}

func (m *memSliding) LoadSlidingConnection(
	_ context.Context, userID, deviceID, connID string, position int64,
) (*core.SlidingConnState, error) {
	m.queries++

	raw, ok := m.conns[position]
	if !ok || m.connKey[position] != userID+"|"+deviceID+"|"+connID {
		return nil, core.ErrUnknownPos
	}

	state := core.NewSlidingConnState()

	return state, json.Unmarshal(raw, state)
}

func (m *memSliding) SaveSlidingConnection(
	_ context.Context, userID, deviceID, connID string, prev int64, state *core.SlidingConnState,
) (int64, error) {
	m.queries++

	key := userID + "|" + deviceID + "|" + connID
	if prev > 0 && m.connKey[prev] != key {
		return 0, core.ErrUnknownPos
	}

	raw, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}

	m.nextPos++
	m.conns[m.nextPos], m.connKey[m.nextPos] = raw, key

	return m.nextPos, nil
}

func (m *memSliding) SlidingRoomActivity(_ context.Context, roomIDs []string, upper int64) (map[string]core.SlidingActivity, error) {
	m.queries++

	out := map[string]core.SlidingActivity{}

	for i := range m.events {
		ev := m.events[i]
		if !slices.Contains(roomIDs, ev.RoomID) || ev.StreamOrdering > upper {
			continue
		}

		a := out[ev.RoomID]
		a.LastEvent = ev.StreamOrdering

		if slices.Contains(core.SlidingBumpEventTypes, ev.Type) {
			a.LastBump = ev.StreamOrdering
		}

		out[ev.RoomID] = a
	}

	return out, nil
}

func (m *memSliding) SlidingRoomInfo(_ context.Context, roomIDs []string) (map[string]core.SlidingRoomInfo, error) {
	m.queries++

	out := map[string]core.SlidingRoomInfo{}

	for _, roomID := range roomIDs {
		state := m.stateAt(roomID, m.position())

		create, ok := state[[2]string{"m.room.create", ""}]
		if !ok {
			continue
		}

		var body struct {
			Type *string `json:"type"`
		}

		_ = json.Unmarshal(create.Content, &body)
		_, encrypted := state[[2]string{"m.room.encryption", ""}]
		out[roomID] = core.SlidingRoomInfo{Known: true, RoomType: body.Type, Encrypted: encrypted}
	}

	return out, nil
}

func (m *memSliding) SpaceChildren(_ context.Context, spaceIDs []string) (map[string][]string, error) {
	m.queries++

	out := map[string][]string{}

	for _, space := range spaceIDs {
		for key := range m.stateAt(space, m.position()) {
			if key[0] == "m.space.child" {
				out[space] = append(out[space], key[1])
			}
		}
	}

	return out, nil
}

func (m *memSliding) TimelineWindows(_ context.Context, windows []core.TimelineWindow) (map[string][]core.Event, error) {
	m.queries++

	out := map[string][]core.Event{}

	for _, w := range windows {
		var events []core.Event

		for i := range m.events {
			ev := m.events[i]
			if ev.RoomID == w.RoomID && ev.StreamOrdering > w.Since && ev.StreamOrdering <= w.Upper {
				events = append(events, ev)
			}
		}

		if len(events) > w.Limit {
			events = events[len(events)-w.Limit:]
		}

		if len(events) > 0 {
			out[w.RoomID] = events
		}
	}

	return out, nil
}

func (m *memSliding) StateChangedSince(_ context.Context, since map[string]int64) (map[string][]core.Event, error) {
	m.queries++

	out := map[string][]core.Event{}

	for roomID, pos := range since {
		state := m.stateAt(roomID, m.position())
		for key := range state {
			if state[key].StreamOrdering > pos {
				out[roomID] = append(out[roomID], state[key])
			}
		}
	}

	return out, nil
}

func (m *memSliding) StateByKeys(_ context.Context, req core.StateKeysRequest) (map[string][]core.Event, error) {
	m.queries++

	out := map[string][]core.Event{}
	seen := map[string]bool{}

	add := func(ev core.Event) {
		if !seen[ev.EventID] {
			seen[ev.EventID] = true
			out[ev.RoomID] = append(out[ev.RoomID], ev)
		}
	}

	for _, ref := range req.Exact {
		if ev, ok := m.stateAt(ref.RoomID, m.position())[[2]string{ref.Type, ref.StateKey}]; ok {
			add(ev)
		}
	}

	for _, ref := range req.Types {
		state := m.stateAt(ref.RoomID, m.position())
		for key := range state {
			if key[0] == ref.Type {
				add(state[key])
			}
		}
	}

	for _, roomID := range req.All {
		state := m.stateAt(roomID, m.position())
		for key := range state {
			add(state[key])
		}
	}

	return out, nil
}

func (m *memSliding) RoomSummaries(_ context.Context, userID string, roomIDs []string) (map[string]core.RoomSummary, error) {
	m.queries++

	out := map[string]core.RoomSummary{}

	for _, roomID := range roomIDs {
		var summary core.RoomSummary

		users := []string{}

		state := m.stateAt(roomID, m.position())
		for key := range state {
			if key[0] != "m.room.member" {
				continue
			}

			switch membershipOfContent(state[key].Content) {
			case "join":
				summary.Joined++
			case "invite":
				summary.Invited++
			default:
				continue
			}

			if key[1] != userID {
				users = append(users, key[1])
			}
		}

		sort.Strings(users)
		summary.Heroes = users[:min(len(users), 5)]
		out[roomID] = summary
	}

	return out, nil
}

func (m *memSliding) AccountData(_ context.Context, userID, roomID, eventType string) ([]byte, error) {
	m.queries++

	raw, ok := m.account[userID+"|"+roomID+"|"+eventType]
	if !ok {
		return nil, core.ErrNotFound
	}

	return raw, nil
}

func (m *memSliding) RoomAccountDataOfType(_ context.Context, userID, eventType string) (map[string][]byte, error) {
	m.queries++

	out := map[string][]byte{}

	for key, raw := range m.account {
		parts := splitKey(key)
		if parts[0] == userID && parts[1] != "" && parts[2] == eventType {
			out[parts[1]] = raw
		}
	}

	return out, nil
}

func splitKey(key string) [3]string {
	var out [3]string

	i := 0

	for _, r := range key {
		if r == '|' && i < 2 {
			i++

			continue
		}

		out[i] += string(r)
	}

	return out
}

// slidingHarness - сервис поверх memSliding и шаги соединения.
type slidingHarness struct {
	t     *testing.T
	m     *memSliding
	svc   *core.SlidingSyncService
	user  string
	pos   *core.SlidingPos
	state *core.SlidingConnState
}

func newSlidingHarness(t *testing.T, user string) *slidingHarness {
	t.Helper()

	m := newMemSliding()

	return &slidingHarness{t: t, m: m, svc: core.NewSlidingSyncService(m, m, m, m, nil), user: user}
}

// step - запрос с текущим pos; состояние сохраняется, как это делает транспорт.
func (h *slidingHarness) step(req *core.SlidingRequest, leavers bool) *core.SlidingSyncOutput {
	h.t.Helper()

	ctx := context.Background()

	prev := core.NewSlidingConnState()

	if h.pos != nil {
		var err error

		prev, err = h.svc.LoadConnection(ctx, h.user, "DEV", req.ConnID, *h.pos)
		if err != nil {
			h.t.Fatalf("load: %v", err)
		}
	}

	out, err := h.svc.Compute(ctx, core.SlidingSyncInput{
		UserID: h.user, Request: req, From: h.pos, Upper: h.m.position(), Prev: prev, NeedLeavers: leavers,
	})
	if err != nil {
		h.t.Fatalf("compute: %v", err)
	}

	var prevConn int64
	if h.pos != nil {
		prevConn = h.pos.Conn
	}

	conn, err := h.svc.SaveConnection(ctx, h.user, "DEV", req.ConnID, prevConn, out.State)
	if err != nil {
		h.t.Fatalf("save: %v", err)
	}

	h.pos = &core.SlidingPos{Conn: conn, Stream: h.m.position()}
	h.state = out.State

	return out
}

func listRequest(ranges [][2]int, limit int, state [][2]string) *core.SlidingRequest {
	return &core.SlidingRequest{
		ConnID: "c",
		Lists: map[string]core.SlidingList{"all": {
			Ranges: ranges, Config: core.SlidingRoomConfig{TimelineLimit: limit, RequiredState: core.NewRequiredState(state)},
		}},
	}
}

func eventTypes(events []core.Event) []string {
	out := make([]string, 0, len(events))
	for i := range events {
		out = append(out, events[i].Type)
	}

	return out
}

func members(events []core.Event) []string {
	out := []string{}

	for i := range events {
		if events[i].Type == "m.room.member" {
			out = append(out, *events[i].StateKey)
		}
	}

	sort.Strings(out)

	return out
}

var lazyState = [][2]string{{"m.room.member", "$LAZY"}, {"m.room.member", "$ME"}, {"m.room.name", ""}}

func TestSlidingComputeInitialAndIncremental(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m

	m.createRoom("!dm", "@a:s", "")
	m.member("!dm", "@b:s", "@b:s", "join", "Bob")
	m.createRoom("!named", "@a:s", "")
	m.state("!named", "m.room.name", "@a:s", "", `{"name":"Комната"}`)
	m.state("!named", "m.room.avatar", "@a:s", "", `{"url":"mxc://s/a"}`)
	m.member("!named", "@c:s", "@c:s", "join", "")
	m.message("!named", "@c:s", "c1")
	m.message("!dm", "@b:s", "b1")
	m.account["@a:s||m.direct"] = []byte(`{"@b:s":["!dm"]}`)

	out := h.step(listRequest([][2]int{{0, 9}}, 1, lazyState), false)

	if out.Lists["all"].Count != 2 || !slices.Equal(out.Lists["all"].Ops[0].RoomIDs, []string{"!dm", "!named"}) {
		t.Fatalf("список: %+v", out.Lists)
	}

	dm := out.Rooms["!dm"]
	if !dm.Initial || !dm.IsDM || dm.Name != "" || len(dm.Heroes) != 1 || dm.Heroes[0].DisplayName != "Bob" ||
		dm.Counts == nil || dm.Counts.Joined != 2 || len(dm.Timeline) != 1 || !dm.Limited || dm.HasNumLive {
		t.Fatalf("dm: %+v", dm)
	}

	if got := members(dm.RequiredState); !slices.Equal(got, []string{"@a:s", "@b:s"}) {
		t.Fatalf("lazy: %v", got)
	}

	named := out.Rooms["!named"]
	if named.Name != "Комната" || named.Avatar != "mxc://s/a" || len(named.Heroes) != 0 || named.IsDM ||
		named.BumpStamp >= dm.BumpStamp || named.PrevBatch != named.Timeline[0].StreamOrdering {
		t.Fatalf("named: %+v", named)
	}

	// Без изменений - пусто.
	if out := h.step(listRequest([][2]int{{0, 9}}, 1, lazyState), false); len(out.Rooms) != 0 {
		t.Fatalf("без изменений: %+v", out.Rooms)
	}

	m.member("!named", "@d:s", "@d:s", "join", "")
	m.message("!named", "@d:s", "d1")
	m.message("!named", "@c:s", "c2")

	out = h.step(listRequest([][2]int{{0, 9}}, 5, lazyState), false)

	named = out.Rooms["!named"]
	if named == nil || named.Initial || !named.ExpandedTimeline || named.NumLive != 3 || !named.HasNumLive {
		t.Fatalf("incremental с ростом timeline: %+v", named)
	}

	if got := members(named.RequiredState); !slices.Equal(got, []string{"@d:s"}) {
		t.Fatalf("новый участник: %v", got)
	}

	if named.Counts == nil || named.Counts.Joined != 3 || named.Name != "" || len(named.Heroes) != 0 {
		t.Fatalf("счетчики после входа: %+v", named)
	}

	// Рост timeline_limit отдает и комнату без изменений - с расширенным timeline.
	if dm := out.Rooms["!dm"]; dm == nil || !dm.ExpandedTimeline || dm.Initial || len(dm.Timeline) != 4 || dm.NumLive != 0 {
		t.Fatalf("dm с ростом timeline: %+v", out.Rooms["!dm"])
	}

	m.message("!named", "@c:s", "c3")

	out = h.step(listRequest([][2]int{{0, 9}}, 5, lazyState), false)
	if named := out.Rooms["!named"]; named == nil || len(named.RequiredState) != 0 || len(named.Timeline) != 1 || named.Limited ||
		named.ExpandedTimeline {
		t.Fatalf("известный участник: %+v", out.Rooms["!named"])
	}

	if _, ok := out.Rooms["!dm"]; ok {
		t.Fatal("dm без изменений отдана")
	}
}

func TestSlidingComputeOutsideWindowAndSubscriptions(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m

	m.createRoom("!a", "@a:s", "")
	m.createRoom("!b", "@a:s", "")
	m.createRoom("!c", "@a:s", "")

	window := listRequest([][2]int{{0, 0}}, 2, nil)

	out := h.step(window, false)
	if !slices.Equal(out.Lists["all"].Ops[0].RoomIDs, []string{"!c"}) {
		t.Fatalf("окно: %+v", out.Lists)
	}

	// Сообщение в !a вне окна... !a поднимается в окно и отдается целиком (NEVER).
	m.message("!a", "@a:s", "a1")

	out = h.step(window, false)
	if a := out.Rooms["!a"]; a == nil || !a.Initial {
		t.Fatalf("!a в окне впервые: %+v", out.Rooms)
	}

	// !c (LIVE) получает сообщение вне окна - PREVIOUSLY.
	m.state("!c", "m.room.topic", "@a:s", "", `{"topic":"t"}`)

	out = h.step(window, false)
	if len(out.Rooms) != 0 {
		t.Fatalf("изменение вне окна отдано: %+v", out.Rooms)
	}

	if st, ok := h.state.Status(core.SlidingStreamRooms, "!c"); !ok || st.Live {
		t.Fatalf("!c должна стать PREVIOUSLY: %+v", st)
	}

	// Подписка на !c: отдается не с нуля, а изменения после запомненной позиции.
	sub := listRequest([][2]int{{0, 0}}, 2, [][2]string{{"m.room.topic", ""}})
	sub.Subscriptions = map[string]core.SlidingRoomConfig{
		"!c":       {TimelineLimit: 2, RequiredState: core.NewRequiredState([][2]string{{"m.room.topic", ""}})},
		"!missing": {TimelineLimit: 1},
	}

	out = h.step(sub, false)

	c := out.Rooms["!c"]
	if c == nil || c.Initial || len(c.Timeline) != 1 || c.Timeline[0].Type != "m.room.topic" ||
		!slices.Equal(eventTypes(c.RequiredState), []string{"m.room.topic"}) {
		t.Fatalf("подписка на PREVIOUSLY: %+v", c)
	}

	if _, ok := out.Rooms["!missing"]; ok || out.Relevant["!missing"] {
		t.Fatal("подписка без членства")
	}

	if st, _ := h.state.Status(core.SlidingStreamRooms, "!c"); !st.Live {
		t.Fatal("после отдачи - LIVE")
	}
}

func TestSlidingComputeMembershipsAndFilters(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m
	space := `,"type":"m.space"`

	m.createRoom("!space", "@a:s", space)
	m.createRoom("!child", "@a:s", "")
	m.state("!space", "m.space.child", "@a:s", "!child", `{"via":["s"]}`)
	m.state("!child", "m.room.encryption", "@a:s", "", `{"algorithm":"m.megolm.v1.aes-sha2"}`)
	m.createRoom("!invite", "@b:s", "")
	m.member("!invite", "@b:s", "@a:s", "invite", "")
	m.createRoom("!kicked", "@b:s", "")
	m.member("!kicked", "@a:s", "@a:s", "join", "")
	m.member("!kicked", "@b:s", "@a:s", "leave", "")
	m.createRoom("!leftold", "@b:s", "")
	m.member("!leftold", "@a:s", "@a:s", "join", "")
	m.member("!leftold", "@a:s", "@a:s", "leave", "")
	m.account["@a:s|!child|m.tag"] = []byte(`{"tags":{"m.favourite":{}}}`)

	f := func(filters *core.SlidingFilters) *core.SlidingRequest {
		req := listRequest([][2]int{{0, 9}}, 0, nil)
		list := req.Lists["all"]
		list.Filters = filters
		req.Lists["all"] = list

		return req
	}

	count := func(filters *core.SlidingFilters) []string {
		t.Helper()

		hh := newSlidingHarness(t, "@a:s")
		hh.m = m
		hh.svc = core.NewSlidingSyncService(m, m, m, m, nil)

		ids := hh.step(f(filters), false).Lists["all"].Ops[0].RoomIDs
		sort.Strings(ids)

		return ids
	}

	if got := count(nil); !slices.Equal(got, []string{"!child", "!invite", "!kicked", "!space"}) {
		t.Fatalf("без фильтров (свой старый выход скрыт, кик виден): %v", got)
	}

	for name, tc := range map[string]struct {
		filters *core.SlidingFilters
		want    []string
	}{
		"not space": {&core.SlidingFilters{NotRoomTypes: []*string{ptr("m.space")}, HasNotRoomTypes: true}, []string{"!child", "!invite", "!kicked"}},
		"encrypted": {&core.SlidingFilters{IsEncrypted: ptr(true)}, []string{"!child"}},
		"tags":      {&core.SlidingFilters{Tags: []string{"m.favourite"}, HasTags: true}, []string{"!child"}},
		"spaces":    {&core.SlidingFilters{Spaces: []string{"!space", "!unknown"}}, []string{"!child"}},
		"invites":   {&core.SlidingFilters{IsInvite: ptr(true)}, []string{"!invite"}},
	} {
		if got := count(tc.filters); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %v, ожидали %v", name, got, tc.want)
		}
	}

	out := h.step(listRequest([][2]int{{0, 9}}, 3, [][2]string{{"m.room.name", ""}}), false)

	invite := out.Rooms["!invite"]
	if invite == nil || invite.HasTimeline || len(invite.InviteState) == 0 || invite.BumpStamp == 0 {
		t.Fatalf("приглашение: %+v", invite)
	}

	kicked := out.Rooms["!kicked"]
	if kicked == nil || len(kicked.Timeline) == 0 || membershipOfContent(kicked.Timeline[len(kicked.Timeline)-1].Content) != "leave" {
		t.Fatalf("кик: %+v", kicked)
	}

	// Свой выход: в окне - отдается, затем пропадает; уход участника - leavers.
	m.member("!child", "@c:s", "@c:s", "join", "")
	h.step(listRequest([][2]int{{0, 9}}, 3, nil), true)

	m.member("!child", "@a:s", "@a:s", "leave", "")
	m.member("!space", "@c:s", "@c:s", "join", "")
	m.member("!space", "@c:s", "@c:s", "leave", "")

	out = h.step(listRequest([][2]int{{0, 9}}, 3, nil), true)
	if child := out.Rooms["!child"]; child == nil || child.Membership != "leave" || !slices.Equal(out.Left, []string{"!child"}) {
		t.Fatalf("выход в окне: %+v %v", out.Rooms["!child"], out.Left)
	}

	if !slices.Equal(out.Leavers, []string{"@c:s"}) {
		t.Fatalf("leavers: %v", out.Leavers)
	}

	out = h.step(listRequest([][2]int{{0, 9}}, 3, nil), true)
	if out.Relevant["!child"] || len(out.Rooms) != 0 {
		t.Fatalf("после выхода комната осталась: %+v", out.Rooms)
	}
}

func TestSlidingComputeRequiredStateChanges(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m

	m.createRoom("!r", "@a:s", "")
	m.state("!r", "m.room.topic", "@a:s", "", `{"topic":"t"}`)
	m.member("!r", "@b:s", "@b:s", "join", "")
	m.message("!r", "@b:s", "hi")

	h.step(listRequest([][2]int{{0, 0}}, 1, [][2]string{{"m.room.name", ""}}), false)

	out := h.step(listRequest([][2]int{{0, 0}}, 1, [][2]string{{"m.room.name", ""}, {"m.room.topic", ""}, {"m.room.member", "*"}}), false)

	r := out.Rooms["!r"]
	if r == nil || r.Initial || len(r.Timeline) != 0 {
		t.Fatalf("расширение required_state: %+v", r)
	}

	if got := eventTypes(r.RequiredState); !slices.Equal(got, []string{"m.room.member", "m.room.member", "m.room.topic"}) {
		t.Fatalf("дочитано: %v", got)
	}

	out = h.step(listRequest([][2]int{{0, 0}}, 1, [][2]string{{"*", "*"}}), false)
	if r := out.Rooms["!r"]; r == nil || len(r.RequiredState) != 4 {
		t.Fatalf("[*,*]: %+v", out.Rooms["!r"])
	}

	// Сужение не требует ничего читать.
	if out := h.step(listRequest([][2]int{{0, 0}}, 1, [][2]string{{"m.room.name", ""}}), false); len(out.Rooms) != 0 {
		t.Fatalf("сужение: %+v", out.Rooms)
	}

	// Неизвестная позиция соединения.
	if _, err := h.svc.LoadConnection(context.Background(), "@a:s", "DEV", "other", *h.pos); err == nil {
		t.Fatal("чужой conn_id должен давать ErrUnknownPos")
	}

	// Без списков и подписок - только членства.
	empty, err := h.svc.Compute(context.Background(), core.SlidingSyncInput{UserID: "@a:s", Request: &core.SlidingRequest{}, Upper: m.position()})
	if err != nil || len(empty.Rooms) != 0 || len(empty.Memberships) != 1 {
		t.Fatalf("без списков: %+v %v", empty, err)
	}
}

func TestSlidingComputeQueriesDoNotGrowWithRooms(t *testing.T) {
	t.Parallel()

	queries := func(n int) int {
		h := newSlidingHarness(t, "@a:s")

		for i := range n {
			id := fmt.Sprintf("!r%d", i)
			h.m.createRoom(id, "@a:s", "")
			h.m.message(id, "@a:s", "x")
		}

		h.step(listRequest([][2]int{{0, 99}}, 3, lazyState), true)

		for i := range n {
			h.m.message(fmt.Sprintf("!r%d", i), "@a:s", "y")
		}

		before := h.m.queries
		h.step(listRequest([][2]int{{0, 99}}, 3, lazyState), true)

		return h.m.queries - before
	}

	if small, big := queries(2), queries(20); small != big {
		t.Fatalf("запросов incremental: %d на 2 комнаты, %d на 20", small, big)
	}
}

// TestSlidingComputeStickySubscriptions - подписка действует, пока ее не снимут
// unsubscribe_rooms (matrix-js-sdk присылает ее один раз), и переживает чтение
// состояния соединения.
func TestSlidingComputeStickySubscriptions(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m

	m.createRoom("!a", "@a:s", "")
	m.createRoom("!b", "@a:s", "")

	sub := core.SlidingRoomConfig{TimelineLimit: 10, RequiredState: core.NewRequiredState([][2]string{{"*", "*"}})}
	req := listRequest([][2]int{{0, 0}}, 1, nil)
	req.Subscriptions = map[string]core.SlidingRoomConfig{"!a": sub}

	if out := h.step(req, false); !out.Relevant["!a"] {
		t.Fatal("подписка не действует")
	}

	m.member("!a", "@b:s", "@b:s", "join", "")

	out := h.step(listRequest([][2]int{{0, 0}}, 1, nil), false)

	a := out.Rooms["!a"]
	if a == nil || !slices.Equal(members(a.RequiredState), []string{"@b:s"}) {
		t.Fatalf("подписка без повтора в запросе: %+v", a)
	}

	unsub := listRequest([][2]int{{0, 0}}, 1, nil)
	unsub.Unsubscribe = []string{"!a"}

	if out := h.step(unsub, false); out.Relevant["!a"] || len(h.state.Subscriptions) != 0 {
		t.Fatalf("снятая подписка действует: %v", h.state.Subscriptions)
	}
}

// TestSlidingComputePeekPublicRoom - подписка на публичную комнату без членства
// отдает stripped state один раз (Element Web ждет комнату, чтобы показать
// вход); закрытая комната без членства не отдается; после входа - с нуля.
func TestSlidingComputePeekPublicRoom(t *testing.T) {
	t.Parallel()

	h := newSlidingHarness(t, "@a:s")
	m := h.m

	m.createRoom("!pub", "@b:s", "")
	m.state("!pub", "m.room.join_rules", "@b:s", "", `{"join_rule":"public"}`)
	m.state("!pub", "m.room.name", "@b:s", "", `{"name":"Публичная"}`)
	m.message("!pub", "@b:s", "до входа")
	m.createRoom("!private", "@b:s", "")
	m.state("!private", "m.room.join_rules", "@b:s", "", `{"join_rule":"invite"}`)

	cfg := core.SlidingRoomConfig{TimelineLimit: 10, RequiredState: core.NewRequiredState([][2]string{{"*", "*"}})}
	req := &core.SlidingRequest{ConnID: "c", Subscriptions: map[string]core.SlidingRoomConfig{"!pub": cfg, "!private": cfg}}

	out := h.step(req, false)

	pub := out.Rooms["!pub"]
	if pub == nil || pub.HasTimeline || len(pub.Timeline) != 0 || pub.Name != "Публичная" || len(pub.InviteState) == 0 {
		t.Fatalf("просмотр публичной: %+v", pub)
	}

	if _, ok := out.Rooms["!private"]; ok {
		t.Fatal("закрытая комната без членства отдана")
	}

	m.message("!pub", "@b:s", "еще")

	if out := h.step(&core.SlidingRequest{ConnID: "c"}, false); len(out.Rooms) != 0 {
		t.Fatalf("просмотр повторно: %+v", out.Rooms)
	}

	m.member("!pub", "@a:s", "@a:s", "join", "")

	out = h.step(&core.SlidingRequest{ConnID: "c"}, false)
	if pub := out.Rooms["!pub"]; pub == nil || !pub.Initial || len(pub.Timeline) == 0 || len(pub.InviteState) != 0 {
		t.Fatalf("после входа: %+v", out.Rooms["!pub"])
	}
}
