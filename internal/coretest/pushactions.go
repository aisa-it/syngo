package coretest

import (
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushers"
)

// MemPushActionsStore - in-memory core.PushActionsStore и core.NotificationsStore:
// события, участники комнат, правила, отметки прочтения, pushers и
// хранимые действия. Поля заполняются тестом до вызовов сервиса.
type MemPushActionsStore struct {
	mu sync.Mutex

	// Events - события в порядке stream_ordering.
	Events []core.Event
	// Members - локальные участники комнат с позицией входа.
	Members map[string][]core.PushRecipient
	// Rules - сохраненные push-правила пользователей.
	Rules map[string][]core.PushRule
	// RoomStates - срез состояния комнат для push-правил.
	RoomStates map[string]*core.PushRoomState
	// FullyRead - room account data m.fully_read: пользователь -> комната -> content.
	FullyRead map[string]map[string][]byte
	// ReadMarks - сохраненные отметки прочтения (receipts).
	ReadMarks []core.PushReadMark
	// Pushers - pushers пользователей.
	Pushers []pushers.Pusher
	// Actions - хранимые действия.
	Actions []core.PushAction
	// Summary - сводка счетчиков по ключу пользователь + комната + тред
	// (event_push_summary): счетчики - строки Actions выше позиции прочтения.
	Summary map[PushSummaryKey]*PushSummary
	// Processed и BackfillUpto - позиция разбора и граница досчета.
	Processed, BackfillUpto int64
	// Backfilled - пользователи с досчитанными старыми событиями.
	Backfilled map[string]bool
	// Pending - пользователи, которых досчет при старте обходит.
	Pending []string
	// Err - ошибка всех методов (сбой хранилища).
	Err error
	// Applied - число вызовов ApplyPushBatch.
	Applied int
}

// NewMemPushActionsStore создает пустое хранилище.
func NewMemPushActionsStore() *MemPushActionsStore {
	return &MemPushActionsStore{
		Members:    map[string][]core.PushRecipient{},
		Rules:      map[string][]core.PushRule{},
		RoomStates: map[string]*core.PushRoomState{},
		FullyRead:  map[string]map[string][]byte{},
		Backfilled: map[string]bool{},
		Summary:    map[PushSummaryKey]*PushSummary{},
	}
}

// PushSummaryKey - ключ сводки.
type PushSummaryKey struct {
	UserID, RoomID, ThreadID string
}

// PushSummary - строка сводки: счетчики и позиция прочтения.
type PushSummary struct {
	Counts   core.PushCounts
	ReadUpTo int64
}

func summaryKey(a core.PushAction) PushSummaryKey {
	return PushSummaryKey{UserID: a.UserID, RoomID: a.RoomID, ThreadID: a.ThreadID}
}

// recount - счетчики ключа по строкам выше позиции прочтения.
func (m *MemPushActionsStore) recount(k PushSummaryKey, sum *PushSummary) {
	sum.Counts = core.PushCounts{}

	for _, a := range m.Actions {
		if summaryKey(a) == k && a.Stream > sum.ReadUpTo {
			sum.Counts.Notification++

			if a.Highlight {
				sum.Counts.Highlight++
			}
		}
	}
}

// PushRulesForUsers - правила пользователей.
func (m *MemPushActionsStore) PushRulesForUsers(_ context.Context, userIDs []string) (map[string][]core.PushRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := make(map[string][]core.PushRule, len(userIDs))
	for _, userID := range userIDs {
		out[userID] = m.Rules[userID]
	}

	return out, nil
}

// PushRoomStates - срез состояния комнат.
func (m *MemPushActionsStore) PushRoomStates(_ context.Context, roomIDs, _ []string) (map[string]*core.PushRoomState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := make(map[string]*core.PushRoomState, len(roomIDs))

	for _, roomID := range roomIDs {
		if st, ok := m.RoomStates[roomID]; ok {
			out[roomID] = st
		}
	}

	return out, nil
}

// EventsByIDs - события по id.
func (m *MemPushActionsStore) EventsByIDs(_ context.Context, ids []string) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.Event

	for i := range m.Events {
		if slices.Contains(ids, m.Events[i].EventID) {
			out = append(out, m.Events[i])
		}
	}

	return out, nil
}

// RoomAccountDataOfType - m.fully_read пользователя по комнатам (другие типы пусты).
func (m *MemPushActionsStore) RoomAccountDataOfType(_ context.Context, userID, eventType string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := map[string][]byte{}

	if eventType == core.ReceiptFullyRead {
		for roomID, content := range m.FullyRead[userID] {
			out[roomID] = content
		}
	}

	return out, nil
}

func (m *MemPushActionsStore) latest() int64 {
	if len(m.Events) == 0 {
		return 0
	}

	return m.Events[len(m.Events)-1].StreamOrdering
}

// PushStreamState - позиция разбора, граница досчета и последняя позиция.
func (m *MemPushActionsStore) PushStreamState(context.Context) (processed, backfillUpto, latest int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return 0, 0, 0, m.Err
	}

	return m.Processed, m.BackfillUpto, m.latest(), nil
}

// ResetPushStream переносит позицию разбора и границу досчета.
func (m *MemPushActionsStore) ResetPushStream(_ context.Context, position int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.Processed, m.BackfillUpto = position, position

	return nil
}

// PushEventsAfter - события в (after, upper], не больше limit.
func (m *MemPushActionsStore) PushEventsAfter(_ context.Context, after, upper int64, limit int) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.Event

	for i := range m.Events {
		if s := m.Events[i].StreamOrdering; s > after && s <= upper && len(out) < limit {
			out = append(out, m.Events[i])
		}
	}

	return out, nil
}

// PushRecipients - участники комнат.
func (m *MemPushActionsStore) PushRecipients(_ context.Context, roomIDs []string) (map[string][]core.PushRecipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := make(map[string][]core.PushRecipient, len(roomIDs))

	for _, roomID := range roomIDs {
		if list, ok := m.Members[roomID]; ok {
			out[roomID] = list
		}
	}

	return out, nil
}

// PushReadMarks - отметки прочтения пользователей в комнатах.
func (m *MemPushActionsStore) PushReadMarks(_ context.Context, roomIDs, userIDs []string) ([]core.PushReadMark, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []core.PushReadMark

	for _, mark := range m.ReadMarks {
		if slices.Contains(roomIDs, mark.RoomID) && slices.Contains(userIDs, mark.UserID) {
			out = append(out, mark)
		}
	}

	return out, nil
}

// ApplyPushBatch - как транзакция в БД: выходы из комнат, удаленные события,
// вставка с приростом сводки, неявное прочтение (пересчет выше позиции).
func (m *MemPushActionsStore) ApplyPushBatch(_ context.Context, b core.PushBatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.Applied++

	for _, l := range b.Left {
		m.Actions = slices.DeleteFunc(m.Actions, func(a core.PushAction) bool {
			return a.UserID == l.UserID && a.RoomID == l.RoomID
		})

		for k := range m.Summary {
			if k.UserID == l.UserID && k.RoomID == l.RoomID {
				delete(m.Summary, k)
			}
		}
	}

	redacted := map[string]bool{}

	for i := range m.Events {
		if m.Events[i].RedactedBy != "" && slices.Contains(b.RedactedBy, m.Events[i].RedactedBy) {
			redacted[m.Events[i].EventID] = true
		}
	}

	m.Actions = slices.DeleteFunc(m.Actions, func(a core.PushAction) bool {
		if !redacted[a.EventID] {
			return false
		}

		if sum := m.Summary[summaryKey(a)]; sum != nil && a.Stream > sum.ReadUpTo {
			sum.Counts.Notification = max(sum.Counts.Notification-1, 0)

			if a.Highlight {
				sum.Counts.Highlight = max(sum.Counts.Highlight-1, 0)
			}
		}

		return true
	})

	for _, a := range b.Actions {
		if slices.ContainsFunc(m.Actions, func(x core.PushAction) bool { return x.UserID == a.UserID && x.EventID == a.EventID }) {
			continue
		}

		m.Actions = append(m.Actions, a)

		sum := m.Summary[summaryKey(a)]
		if sum == nil {
			sum = &PushSummary{}
			m.Summary[summaryKey(a)] = sum
		}

		sum.Counts.Notification++

		if a.Highlight {
			sum.Counts.Highlight++
		}
	}

	for _, r := range b.ReadUpTo {
		k := PushSummaryKey{UserID: r.UserID, RoomID: r.RoomID, ThreadID: r.ThreadID}
		if sum := m.Summary[k]; sum != nil && sum.ReadUpTo < r.Stream && sum.Counts.Notification > 0 {
			sum.ReadUpTo = r.Stream
			m.recount(k, sum)
		}
	}

	if b.BackfillFor != "" {
		m.Backfilled[b.BackfillFor] = true
	} else {
		m.Processed = max(m.Processed, b.Processed)
	}

	return nil
}

// MarkPushRead поднимает позицию прочтения до события eventID и пересчитывает
// счетчики выше нее.
func (m *MemPushActionsStore) MarkPushRead(
	_ context.Context, userID, roomID, threadID, eventID string, unthreaded, force bool,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	upto, found := int64(0), false

	for i := range m.Events {
		if m.Events[i].EventID == eventID && m.Events[i].RoomID == roomID {
			upto, found = m.Events[i].StreamOrdering, true
		}
	}

	if !found {
		return nil
	}

	for k, sum := range m.Summary {
		if k.UserID != userID || k.RoomID != roomID || (!unthreaded && k.ThreadID != threadID) {
			continue
		}

		if !force && (sum.ReadUpTo >= upto || sum.Counts.Notification == 0) {
			continue
		}

		sum.ReadUpTo = max(sum.ReadUpTo, upto)
		m.recount(k, sum)
	}

	return nil
}

// PrunePushActions удаляет строки не выше позиции прочтения в комнатах keys.
func (m *MemPushActionsStore) PrunePushActions(_ context.Context, keys []core.PushReadUpTo) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return m.Err
	}

	m.Actions = slices.DeleteFunc(m.Actions, func(a core.PushAction) bool {
		if !slices.ContainsFunc(keys, func(k core.PushReadUpTo) bool { return k.UserID == a.UserID && k.RoomID == a.RoomID }) {
			return false
		}

		sum := m.Summary[summaryKey(a)]

		return sum != nil && a.Stream <= sum.ReadUpTo
	})

	return nil
}

// PushBackfilled - досчитаны ли старые события пользователя.
func (m *MemPushActionsStore) PushBackfilled(_ context.Context, userID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return false, m.Err
	}

	return m.Backfilled[userID], nil
}

// PushBackfillPending - пользователи из Pending без отметки досчета.
func (m *MemPushActionsStore) PushBackfillPending(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	var out []string

	for _, userID := range m.Pending {
		if !m.Backfilled[userID] {
			out = append(out, userID)
		}
	}

	return out, nil
}

// PushCountsFor - счетчики комнат по тредам из сводки.
func (m *MemPushActionsStore) PushCountsFor(
	_ context.Context, userID string, roomIDs []string,
) (map[string]map[string]core.PushCounts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := map[string]map[string]core.PushCounts{}

	for k, sum := range m.Summary {
		if k.UserID != userID || !slices.Contains(roomIDs, k.RoomID) || sum.Counts.Notification == 0 {
			continue
		}

		if out[k.RoomID] == nil {
			out[k.RoomID] = map[string]core.PushCounts{}
		}

		out[k.RoomID][k.ThreadID] = sum.Counts
	}

	return out, nil
}

// PushUnreadRooms - число комнат с ненулевой сводкой пользователя.
func (m *MemPushActionsStore) PushUnreadRooms(_ context.Context, userID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return 0, m.Err
	}

	rooms := map[string]bool{}

	for k, sum := range m.Summary {
		if k.UserID == userID && sum.Counts.Notification > 0 {
			rooms[k.RoomID] = true
		}
	}

	return len(rooms), nil
}

// UnreadCandidates - последние perRoom событий комнат в (low, upper].
func (m *MemPushActionsStore) UnreadCandidates(
	_ context.Context, low map[string]int64, upper int64, perRoom int,
) (map[string][]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	out := map[string][]core.Event{}

	for i := range m.Events {
		ev := m.Events[i]
		if from, ok := low[ev.RoomID]; ok && ev.StreamOrdering > from && ev.StreamOrdering <= upper {
			out[ev.RoomID] = append(out[ev.RoomID], ev)
		}
	}

	for roomID, list := range out {
		if len(list) > perRoom {
			out[roomID] = list[len(list)-perRoom:]
		}
	}

	return out, nil
}

// SyncPosition - последняя позиция событий.
func (m *MemPushActionsStore) SyncPosition(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return 0, m.Err
	}

	return m.latest(), nil
}

// RoomPushers - pushers участников комнаты, кроме exceptUser, и extra.
func (m *MemPushActionsStore) RoomPushers(
	_ context.Context, roomID, exceptUser string, extra ...string,
) ([]pushers.Pusher, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	users := slices.Clone(extra)

	for _, r := range m.Members[roomID] {
		if r.UserID != exceptUser {
			users = append(users, r.UserID)
		}
	}

	var out []pushers.Pusher

	for i := range m.Pushers {
		if slices.Contains(users, m.Pushers[i].UserID) {
			out = append(out, m.Pushers[i])
		}
	}

	return out, nil
}

// NotificationCandidates - события комнат пользователя после входа, не от него и
// не удаленные, с позицией меньше before (0 - без границы), от новых к старым.
func (m *MemPushActionsStore) NotificationCandidates(
	_ context.Context, userID string, before int64, limit int,
) ([]core.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}

	if before <= 0 {
		before = 1<<62 - 1
	}

	joined := map[string]int64{}

	for roomID, list := range m.Members {
		for _, r := range list {
			if r.UserID == userID {
				joined[roomID] = r.JoinStream
			}
		}
	}

	var out []core.Event

	for i := range m.Events {
		ev := m.Events[i]

		joinedAt, ok := joined[ev.RoomID]
		if ok && ev.StreamOrdering > joinedAt && ev.Sender != userID && ev.RedactedBy == "" && ev.StreamOrdering < before {
			out = append(out, ev)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StreamOrdering > out[j].StreamOrdering })

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}
