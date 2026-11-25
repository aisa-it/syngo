// Package coretest содержит in-memory реализации доменных интерфейсов для тестов.
//
// Пакет импортируется только из _test.go; в продовый бинарь он не попадает.
package coretest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// MemRoomStore — потокобезопасное in-memory хранилище комнат, событий, состояния
// и членства. Повторяет контракт core.RoomStore, включая идемпотентность txn.
type MemRoomStore struct {
	mu           sync.Mutex
	rooms        map[string]core.Room
	events       []core.Event
	state        map[string]map[string]string // roomID -> type\x00stateKey -> eventID
	memberships  map[string]map[string]string // roomID -> userID -> membership
	aliases      map[string]string            // alias -> roomID
	aliasOwners  map[string]string            // alias -> создатель алиаса
	transactions map[string]string            // ключ txn -> eventID
	blocked      map[string]bool              // заблокированные комнаты
	stream       int64
	now          func() time.Time
}

// NewMemRoomStore создаёт пустое хранилище.
func NewMemRoomStore() *MemRoomStore {
	return &MemRoomStore{
		rooms:        map[string]core.Room{},
		state:        map[string]map[string]string{},
		memberships:  map[string]map[string]string{},
		aliases:      map[string]string{},
		aliasOwners:  map[string]string{},
		transactions: map[string]string{},
		blocked:      map[string]bool{},
		now:          time.Now,
	}
}

// SetBlocked блокирует или разблокирует комнату (реализация core.RoomBlockChecker).
func (m *MemRoomStore) SetBlocked(roomID string, blocked bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.blocked[roomID] = blocked
}

// RoomBlocked сообщает, заблокирована ли комната.
func (m *MemRoomStore) RoomBlocked(_ context.Context, roomID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.blocked[roomID], nil
}

// Rooms отдаёт копию карты комнат (для проверок в тестах).
func (m *MemRoomStore) Rooms() map[string]core.Room {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]core.Room, len(m.rooms))
	for id, room := range m.rooms {
		out[id] = room
	}

	return out
}

// Events отдаёт копию журнала событий.
func (m *MemRoomStore) Events() []core.Event {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]core.Event, len(m.events))
	copy(out, m.events)

	return out
}

// CreateRoom записывает комнату и стартовые события.
func (m *MemRoomStore) CreateRoom(_ context.Context, room core.Room, events []core.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.rooms[room.RoomID]; exists {
		return fmt.Errorf("coretest: room %s уже существует", room.RoomID)
	}

	m.rooms[room.RoomID] = room

	for i := range events {
		m.appendLocked(events[i])
	}

	return nil
}

// RoomByID возвращает комнату.
func (m *MemRoomStore) RoomByID(_ context.Context, roomID string) (core.Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, ok := m.rooms[roomID]
	if !ok {
		return core.Room{}, fmt.Errorf("coretest: room %s: %w", roomID, core.ErrNotFound)
	}

	return room, nil
}

// SetRoomPublic меняет публикацию комнаты в каталоге.
func (m *MemRoomStore) SetRoomPublic(_ context.Context, roomID string, public bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, ok := m.rooms[roomID]
	if !ok {
		return fmt.Errorf("coretest: room %s: %w", roomID, core.ErrNotFound)
	}

	room.IsPublic = public
	m.rooms[roomID] = room

	return nil
}

// LatestEvents отдаёт последние события комнаты.
func (m *MemRoomStore) LatestEvents(_ context.Context, roomID string, limit int) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.Event

	for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
		if m.events[i].RoomID == roomID {
			out = append(out, m.events[i])
		}
	}

	return out, nil
}

// AppendEvent пишет событие с идемпотентностью txn.
func (m *MemRoomStore) AppendEvent(
	_ context.Context,
	event core.Event,
	txn *core.TransactionKey,
) (core.Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if txn != nil {
		key := txnKey(txn)
		if existing, ok := m.transactions[key]; ok {
			return core.Event{EventID: existing, RoomID: event.RoomID}, true, nil
		}

		m.transactions[key] = event.EventID
	}

	written := m.appendLocked(event)

	return written, false, nil
}

// EventByID находит событие комнаты.
func (m *MemRoomStore) EventByID(_ context.Context, roomID, eventID string) (core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.events {
		if m.events[i].RoomID == roomID && m.events[i].EventID == eventID {
			return redactedCopy(m.events[i]), nil
		}
	}

	return core.Event{}, fmt.Errorf("coretest: event %s: %w", eventID, core.ErrNotFound)
}

// RoomEvents отдаёт окно событий от курсора.
func (m *MemRoomStore) RoomEvents(
	_ context.Context, roomID string, from int64, limit int, backward bool,
) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.Event

	if backward {
		for i := len(m.events) - 1; i >= 0 && len(out) < limit; i-- {
			event := m.events[i]
			if event.RoomID == roomID && event.StreamOrdering < from {
				out = append(out, redactedCopy(event))
			}
		}

		return out, nil
	}

	for i := range m.events {
		event := &m.events[i]
		if event.RoomID == roomID && event.StreamOrdering > from && len(out) < limit {
			out = append(out, redactedCopy(*event))
		}
	}

	return out, nil
}

// SetDepth меняет depth события: в тестах так воспроизводится импортированная
// история Synapse с ветками DAG (несколько событий на одной depth).
func (m *MemRoomStore) SetDepth(eventID string, depth int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.events {
		if m.events[i].EventID == eventID {
			m.events[i].Depth = depth
		}
	}
}

// StreamAtDepth - граница топологической позиции Synapse (depth, stream) в
// stream_ordering комнаты, как SQL-реализация.
func (m *MemRoomStore) StreamAtDepth(
	_ context.Context, roomID string, depth, stream int64, backward bool,
) (pos int64, found bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.events {
		event := &m.events[i]
		if event.RoomID != roomID {
			continue
		}

		before := event.Depth < depth || (event.Depth == depth && event.StreamOrdering <= stream)

		switch {
		case backward && before && (!found || event.StreamOrdering > pos):
			pos, found = event.StreamOrdering, true
		case !backward && !before && (!found || event.StreamOrdering < pos):
			pos, found = event.StreamOrdering, true
		}
	}

	return pos, found, nil
}

// MinMaxStream возвращает границы stream_ordering комнаты.
func (m *MemRoomStore) MinMaxStream(_ context.Context, roomID string) (minStream, maxStream int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.events {
		event := &m.events[i]
		if event.RoomID != roomID {
			continue
		}

		if minStream == 0 || event.StreamOrdering < minStream {
			minStream = event.StreamOrdering
		}

		if event.StreamOrdering > maxStream {
			maxStream = event.StreamOrdering
		}
	}

	return minStream, maxStream, nil
}

// Membership возвращает членство пользователя.
func (m *MemRoomStore) Membership(_ context.Context, roomID, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	membership, ok := m.memberships[roomID][userID]
	if !ok {
		return "", fmt.Errorf("coretest: membership %s: %w", userID, core.ErrNotFound)
	}

	return membership, nil
}

// MembershipInfo - членство и позиция последнего события членства пользователя.
func (m *MemRoomStore) MembershipInfo(_ context.Context, roomID, userID string) (core.MembershipInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	membership, ok := m.memberships[roomID][userID]
	if !ok {
		return core.MembershipInfo{}, fmt.Errorf("coretest: membership %s: %w", userID, core.ErrNotFound)
	}

	info := core.MembershipInfo{Membership: membership}

	for i := range m.events {
		ev := m.events[i]
		if ev.RoomID == roomID && ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == userID {
			info.Stream = ev.StreamOrdering
		}
	}

	return info, nil
}

// StateAtStream - состояние комнаты на позиции pos (последнее событие ключа).
func (m *MemRoomStore) StateAtStream(_ context.Context, roomID string, pos int64) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	latest := map[string]core.Event{}
	order := []string{}

	for i := range m.events {
		ev := m.events[i]
		if ev.RoomID != roomID || ev.StateKey == nil || ev.StreamOrdering > pos {
			continue
		}

		key := ev.Type + "\x00" + *ev.StateKey
		if _, seen := latest[key]; !seen {
			order = append(order, key)
		}

		latest[key] = redactedCopy(ev)
	}

	out := make([]core.Event, 0, len(order))
	for _, key := range order {
		out = append(out, latest[key])
	}

	return out, nil
}

// AuthEventIDs отдаёт auth-события для нового события (реализация core.RoomStore).
func (m *MemRoomStore) AuthEventIDs(
	_ context.Context, roomID, sender, eventType string, stateKey *string,
) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	types := [][2]string{
		{"m.room.create", ""},
		{"m.room.power_levels", ""},
		{"m.room.member", sender},
	}

	if eventType == "m.room.member" && stateKey != nil {
		types = append(types, [2]string{"m.room.member", *stateKey}, [2]string{"m.room.join_rules", ""})
	}

	out := make([]string, 0, len(types))

	for _, pair := range types {
		if eventID, ok := m.state[roomID][pair[0]+"\x00"+pair[1]]; ok {
			out = append(out, eventID)
		}
	}

	return out, nil
}

// MembershipRooms отдаёт членства пользователя (реализация core.SyncDataStore).
func (m *MemRoomStore) MembershipRooms(_ context.Context, userID string) ([]core.RoomMembership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.RoomMembership

	// Позиция события членства - как в SQL-реализации: для leave/ban события
	// отдаются только до выхода.
	streamOf := make(map[string]int64, len(m.events))
	for i := range m.events {
		streamOf[m.events[i].EventID] = m.events[i].StreamOrdering
	}

	for roomID, members := range m.memberships {
		if membership, ok := members[userID]; ok {
			out = append(out, core.RoomMembership{
				RoomID: roomID, Membership: membership,
				LeaveStream: streamOf[m.state[roomID]["m.room.member\x00"+userID]],
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RoomID < out[j].RoomID })

	return out, nil
}

// JoinedRoomIDs - комнаты с membership=join по возрастанию room_id (core.MembershipStore).
func (m *MemRoomStore) JoinedRoomIDs(_ context.Context, userID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string

	for roomID, members := range m.memberships {
		if members[userID] == "join" {
			out = append(out, roomID)
		}
	}

	sort.Strings(out)

	return out, nil
}

// EventsSince отдаёт события комнат после позиции (реализация core.SyncDataStore).
func (m *MemRoomStore) EventsSince(
	_ context.Context, roomIDs []string, since int64, perRoom int, upper map[string]int64,
) (map[string][]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	allowed := make(map[string]bool, len(roomIDs))
	for _, roomID := range roomIDs {
		allowed[roomID] = true
	}

	out := map[string][]core.Event{}

	for i := range m.events {
		event := m.events[i]
		if !allowed[event.RoomID] || event.StreamOrdering <= since {
			continue
		}

		// Верхняя граница для leave/ban — как в SQL-реализации.
		if bound, ok := upper[event.RoomID]; ok && event.StreamOrdering > bound {
			continue
		}

		out[event.RoomID] = append(out[event.RoomID], event)
	}

	// Как в SQL-реализации: последние perRoom событий комнаты.
	for roomID, events := range out {
		if perRoom > 0 && len(events) > perRoom {
			out[roomID] = events[len(events)-perRoom:]
		}
	}

	return out, nil
}

// StateForRooms отдаёт материализованное состояние комнат (реализация core.SyncDataStore).
func (m *MemRoomStore) StateForRooms(_ context.Context, roomIDs []string) (map[string][]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	byID := make(map[string]core.Event, len(m.events))
	for i := range m.events {
		byID[m.events[i].EventID] = m.events[i]
	}

	out := map[string][]core.Event{}

	for _, roomID := range roomIDs {
		// Порядок как в SQL: по type, state_key.
		var events []core.Event

		for key := range m.state[roomID] { //nolint:gocritic // ключи — type/state_key
			if event, ok := byID[m.state[roomID][key]]; ok {
				events = append(events, event)
			}
		}

		sort.Slice(events, func(i, j int) bool {
			if events[i].Type != events[j].Type {
				return events[i].Type < events[j].Type
			}

			return derefString(events[i].StateKey) < derefString(events[j].StateKey)
		})
		out[roomID] = events
	}

	return out, nil
}

// StateDeltaForRooms - события текущего состояния после since и члены members
// (реализация core.SyncDataStore, как в SQL).
func (m *MemRoomStore) StateDeltaForRooms(
	ctx context.Context, roomIDs []string, since int64, members []string,
) (map[string][]core.Event, error) {
	full, err := m.StateForRooms(ctx, roomIDs)
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(members))
	for _, member := range members {
		wanted[member] = true
	}

	out := map[string][]core.Event{}

	for roomID, events := range full {
		for i := range events {
			event := events[i]
			isMember := event.Type == "m.room.member" && event.StateKey != nil && wanted[*event.StateKey]

			if event.StreamOrdering > since || isMember {
				out[roomID] = append(out[roomID], event)
			}
		}
	}

	return out, nil
}

// StateMembersForRooms - состояние без чужого членства (реализация
// core.SyncDataStore, как в SQL).
func (m *MemRoomStore) StateMembersForRooms(
	ctx context.Context, roomIDs, members []string,
) (map[string][]core.Event, error) {
	full, err := m.StateForRooms(ctx, roomIDs)
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(members))
	for _, member := range members {
		wanted[member] = true
	}

	out := map[string][]core.Event{}

	for roomID, events := range full {
		for i := range events {
			event := events[i]
			if event.Type != "m.room.member" || (event.StateKey != nil && wanted[*event.StateKey]) {
				out[roomID] = append(out[roomID], event)
			}
		}
	}

	return out, nil
}

// StreamPosition — текущая максимальная позиция потока.
func (m *MemRoomStore) StreamPosition(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stream, nil
}

// derefString безопасно разыменовывает *string.
func derefString(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

// appendLocked добавляет событие, назначает stream_ordering и применяет состояние.
func (m *MemRoomStore) appendLocked(event core.Event) core.Event {
	m.stream++
	event.StreamOrdering = m.stream
	m.events = append(m.events, event)

	if event.Type == "m.room.redaction" {
		m.redactLocked(event)
	}

	if !event.IsState() {
		return event
	}

	if m.state[event.RoomID] == nil {
		m.state[event.RoomID] = map[string]string{}
	}

	m.state[event.RoomID][event.Type+"\x00"+*event.StateKey] = event.EventID

	if event.Type == "m.room.member" {
		var content struct {
			Membership string `json:"membership"`
		}

		if err := json.Unmarshal(event.Content, &content); err == nil {
			if m.memberships[event.RoomID] == nil {
				m.memberships[event.RoomID] = map[string]string{}
			}

			m.memberships[event.RoomID][*event.StateKey] = content.Membership
		}
	}

	return event
}

// txnKey собирает ключ идемпотентности.
func txnKey(txn *core.TransactionKey) string {
	return txn.UserID + "\x00" + txn.DeviceID + "\x00" + txn.Endpoint + "\x00" + txn.TxnID
}

// MembersPage — страница m.room.member-событий из состояния.
// ForgetRoom — пометка комнаты забытой (реализация core.RoomStore).
func (m *MemRoomStore) ForgetRoom(_ context.Context, userID, roomID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.memberships[roomID][userID] == "join" {
		return fmt.Errorf("coretest: cannot forget joined room: %w", core.ErrForbidden)
	}

	return nil
}

func (m *MemRoomStore) MembersPage(_ context.Context, roomID, at string, limit int) ([]core.Event, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.Event

	for key, eventID := range m.state[roomID] {
		typ, stateKey, found := strings.Cut(key, "\x00")
		if !found || typ != "m.room.member" || stateKey <= at {
			continue
		}

		for i := range m.events {
			if m.events[i].EventID == eventID {
				out = append(out, m.events[i])

				break
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return *out[i].StateKey < *out[j].StateKey
	})

	next := ""
	if len(out) > limit {
		out = out[:limit]
	}

	if len(out) == limit && out[len(out)-1].StateKey != nil {
		next = *out[len(out)-1].StateKey
	}

	return out, next, nil
}

// JoinedMembers — участники с membership=join.
func (m *MemRoomStore) JoinedMembers(_ context.Context, roomID string) (map[string]core.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]core.Profile{}

	for userID := range m.memberships[roomID] {
		if m.memberships[roomID][userID] == "join" {
			out[userID] = core.Profile{UserID: userID}
		}
	}

	return out, nil
}

// AliasesByRoom — алиасы комнаты.
func (m *MemRoomStore) AliasesByRoom(_ context.Context, roomID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string

	for alias, id := range m.aliases {
		if id == roomID {
			out = append(out, alias)
		}
	}

	sort.Strings(out)

	return out, nil
}

// ResolveAlias — room_id по алиасу.
func (m *MemRoomStore) ResolveAlias(_ context.Context, alias string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	roomID, ok := m.aliases[alias]
	if !ok {
		return "", fmt.Errorf("coretest: alias %s: %w", alias, core.ErrNotFound)
	}

	return roomID, nil
}

// AliasOwner — создатель и комната алиаса.
func (m *MemRoomStore) AliasOwner(_ context.Context, alias string) (creator, roomID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	roomID, ok := m.aliases[alias]
	if !ok {
		return "", "", fmt.Errorf("coretest: alias %s: %w", alias, core.ErrNotFound)
	}

	return m.aliasOwners[alias], roomID, nil
}

// CreateAlias — добавить алиас (занятый → ErrForbidden).
func (m *MemRoomStore) CreateAlias(_ context.Context, alias, roomID, creator string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.aliases[alias]; ok {
		return fmt.Errorf("coretest: alias %s is taken: %w", alias, core.ErrForbidden)
	}

	m.aliases[alias] = roomID
	m.aliasOwners[alias] = creator

	return nil
}

// DeleteAlias — удалить алиас.
func (m *MemRoomStore) DeleteAlias(_ context.Context, alias string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.aliases[alias]; !ok {
		return fmt.Errorf("coretest: alias %s: %w", alias, core.ErrNotFound)
	}

	delete(m.aliases, alias)
	delete(m.aliasOwners, alias)

	return nil
}

// StateEvents — материализованное состояние комнаты.
func (m *MemRoomStore) StateEvents(_ context.Context, roomID string) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.state[roomID]
	out := make([]core.Event, 0, len(state))

	for _, eventID := range state {
		for i := range m.events {
			if m.events[i].EventID == eventID {
				out = append(out, redactedCopy(m.events[i]))

				break
			}
		}
	}

	return out, nil
}

// CurrentStateEvent - текущее событие состояния по ключу.
func (m *MemRoomStore) CurrentStateEvent(ctx context.Context, roomID, eventType, stateKey string) (core.Event, error) {
	all, err := m.StateEvents(ctx, roomID)
	if err != nil {
		return core.Event{}, err
	}

	for i := range all {
		if all[i].Type == eventType && all[i].StateKey != nil && *all[i].StateKey == stateKey {
			return all[i], nil
		}
	}

	return core.Event{}, fmt.Errorf("coretest: state %s/%s: %w", eventType, stateKey, core.ErrNotFound)
}

// AuthStateEvents - срез состояния для auth rules (как в SQL-реализации).
func (m *MemRoomStore) AuthStateEvents(ctx context.Context, roomID string, members []string) ([]core.Event, error) {
	all, err := m.StateEvents(ctx, roomID)
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]bool, len(members))
	for _, member := range members {
		wanted[member] = true
	}

	out := make([]core.Event, 0, len(members)+3)

	for i := range all {
		event := all[i]
		if event.StateKey == nil {
			continue
		}

		switch event.Type {
		case "m.room.create", "m.room.power_levels", "m.room.join_rules":
			if *event.StateKey == "" {
				out = append(out, event)
			}
		case "m.room.member":
			if wanted[*event.StateKey] {
				out = append(out, event)
			}
		}
	}

	return out, nil
}

// redactLocked помечает цель redaction при записи самого события, как
// store.AppendEvent: первая redaction остается, повторная не переписывает.
func (m *MemRoomStore) redactLocked(redaction core.Event) {
	var content struct {
		Redacts string `json:"redacts"`
	}

	if err := json.Unmarshal(redaction.Content, &content); err != nil || content.Redacts == "" {
		return
	}

	for i := range m.events {
		ev := &m.events[i]
		if ev.RoomID == redaction.RoomID && ev.EventID == content.Redacts && ev.RedactedBy == "" {
			ev.RedactedBy = redaction.EventID

			return
		}
	}
}

// redactedCopy — копия события с вырезанным content (в store content вырезается при записи redaction).
func redactedCopy(event core.Event) core.Event {
	if event.RedactedBy == "" {
		return event
	}

	event.Content = core.RedactContent(event.Type, event.Content)

	return event
}

// Relations отдаёт события комнаты по m.relates_to (реализация core.RelationsStore).
func (m *MemRoomStore) Relations(
	_ context.Context, roomID, eventID, relType, eventType string, from int64, dir string, limit int,
) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []core.Event

	for i := range m.events {
		event := &m.events[i]
		if event.RoomID != roomID || len(out) >= limit {
			continue
		}

		// Эксклюзивный курсор с направлением, как в SQL-реализации.
		if dir == "f" {
			if from > 0 && event.StreamOrdering <= from {
				continue
			}
		} else if from > 0 && event.StreamOrdering >= from {
			continue
		}

		if eventType != "" && event.Type != eventType {
			continue
		}

		// Удаленное событие связью не считается, как в SQL-реализации.
		if event.RedactedBy != "" {
			continue
		}

		var content struct {
			RelatesTo struct {
				EventID string `json:"event_id"`
				RelType string `json:"rel_type"`
			} `json:"m.relates_to"`
		}

		if err := json.Unmarshal(event.Content, &content); err != nil {
			continue
		}

		if content.RelatesTo.EventID != eventID {
			continue
		}

		if relType != "" && content.RelatesTo.RelType != relType {
			continue
		}

		out = append(out, *event)
	}

	if dir != "f" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}

	return out, nil
}

// Threads отдает корни тредов комнаты по последнему ответу (реализация
// core.ThreadsStore), с той же семантикой, что SQL-реализация.
func (m *MemRoomStore) Threads(_ context.Context, q core.ThreadsQuery) ([]core.Event, []int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	visible := func(pos int64) bool { return pos < q.VisibleBefore || pos >= q.VisibleFrom }

	type thread struct {
		latest       int64
		participated bool
	}

	threads := map[string]*thread{}
	byID := map[string]core.Event{}

	for i := range m.events {
		event := m.events[i]
		if event.RoomID != q.RoomID {
			continue
		}

		byID[event.EventID] = event

		if event.RedactedBy != "" || event.StateKey != nil || !visible(event.StreamOrdering) {
			continue
		}

		var content struct {
			RelatesTo struct {
				EventID string `json:"event_id"`
				RelType string `json:"rel_type"`
			} `json:"m.relates_to"`
		}

		if json.Unmarshal(event.Content, &content) != nil || content.RelatesTo.RelType != "m.thread" {
			continue
		}

		th := threads[content.RelatesTo.EventID]
		if th == nil {
			th = &thread{}
			threads[content.RelatesTo.EventID] = th
		}

		th.latest = max(th.latest, event.StreamOrdering)
		th.participated = th.participated || event.Sender == q.UserID
	}

	cursor := q.From
	if cursor <= 0 {
		cursor = 1 << 62
	}

	roots := make([]core.Event, 0, len(threads))

	for rootID, th := range threads {
		root, ok := byID[rootID]
		if !ok || root.StateKey != nil || !visible(root.StreamOrdering) || th.latest >= cursor {
			continue
		}

		if q.Participated && root.Sender != q.UserID && !th.participated {
			continue
		}

		roots = append(roots, redactedCopy(root))
	}

	sort.Slice(roots, func(i, j int) bool {
		return threads[roots[i].EventID].latest > threads[roots[j].EventID].latest
	})

	if q.Limit > 0 && len(roots) > q.Limit {
		roots = roots[:q.Limit]
	}

	latest := make([]int64, len(roots))
	for i := range roots {
		latest[i] = threads[roots[i].EventID].latest
	}

	return roots, latest, nil
}
