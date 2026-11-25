package core

import (
	"context"
	"encoding/json"

	"github.com/aisa-it/syngo/internal/pushrules"
)

// Вычисление push-правил и отметки прочтения для счетчиков непрочитанного,
// /notifications и рассылки push. Перенесено из
// HTTP-обработчиков.

// UnreadScanLimit - сколько последних непрочитанных событий комнаты
// просматривается при досчете счетчиков. Больше - счетчик упирается в предел,
// но история комнаты, где пользователь ничего не читал, не сканируется целиком.
const UnreadScanLimit = 100

// PushRoomState - срез состояния комнаты для вычисления push-правил.
type PushRoomState struct {
	MemberCount int
	// DisplayNames - displayname запрошенных пользователей в комнате.
	DisplayNames map[string]string
	// PowerLevels - content m.room.power_levels (nil - события нет).
	PowerLevels json.RawMessage
	// Creator - отправитель m.room.create.
	Creator string
}

// PushEvalStore - правила пользователей и срез состояния комнат для push-правил.
type PushEvalStore interface {
	// PushRulesForUsers - сохраненные правила пользователей одним запросом.
	PushRulesForUsers(ctx context.Context, userIDs []string) (map[string][]PushRule, error)
	// PushRoomStates - срез состояния комнат и имена пользователей userIDs.
	PushRoomStates(ctx context.Context, roomIDs, userIDs []string) (map[string]*PushRoomState, error)
}

// PushEvaluator вычисляет push-правила для набора пользователей и комнат:
// правила и срез состояния загружаются один раз.
type PushEvaluator struct {
	rules map[string]pushrules.Ruleset
	rooms map[string]*PushRoomState
}

// NewPushEvaluator загружает правила userIDs и состояние roomIDs (два запроса).
func NewPushEvaluator(ctx context.Context, store PushEvalStore, userIDs, roomIDs []string) (*PushEvaluator, error) {
	stored, err := store.PushRulesForUsers(ctx, userIDs)
	if err != nil {
		return nil, err
	}

	rooms, err := store.PushRoomStates(ctx, roomIDs, userIDs)
	if err != nil {
		return nil, err
	}

	ev := &PushEvaluator{rules: make(map[string]pushrules.Ruleset, len(userIDs)), rooms: rooms}

	for _, userID := range userIDs {
		ev.rules[userID] = BuildPushRules(userID, stored[userID])
	}

	return ev, nil
}

// Evaluate - действия для события у получателя userID. Свои и удаленные
// события не уведомляют (как в Synapse).
func (e *PushEvaluator) Evaluate(userID string, ev Event) pushrules.Result {
	if ev.Sender == userID || ev.RedactedBy != "" {
		return pushrules.Result{}
	}

	rules, ok := e.rules[userID]
	if !ok {
		return pushrules.Result{}
	}

	info := pushrules.RoomInfo{}
	if st := e.rooms[ev.RoomID]; st != nil {
		info = pushrules.RoomInfo{
			MemberCount: st.MemberCount, DisplayName: st.DisplayNames[userID],
			PowerLevels: st.PowerLevels, Creator: st.Creator,
		}
	}

	return rules.Evaluate(pushrules.Event{
		Type: ev.Type, Sender: ev.Sender, RoomID: ev.RoomID, StateKey: ev.StateKey, Content: ev.Content,
	}, info.Context(ev.Sender))
}

// ThreadRoot - корень треда события ("" - основная лента).
func ThreadRoot(content json.RawMessage) string {
	var c struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}

	if json.Unmarshal(content, &c) != nil || c.RelatesTo.RelType != "m.thread" {
		return ""
	}

	return c.RelatesTo.EventID
}

// ReadState - до какой позиции пользователь прочитал комнату: receipt без
// треда покрывает все, "main" - основную ленту, receipt треда - сам тред
// (спецификация, «Threaded read receipts»).
type ReadState struct {
	unthreaded int64
	main       int64
	threads    map[string]int64
}

func (rs *ReadState) raise(thread string, stream int64) {
	if thread == "" {
		rs.main = max(rs.main, stream)

		return
	}

	if rs.threads == nil {
		rs.threads = map[string]int64{}
	}

	rs.threads[thread] = max(rs.threads[thread], stream)
}

// IsRead - прочитано ли событие с позицией stream в треде thread.
func (rs *ReadState) IsRead(stream int64, thread string) bool {
	if stream <= rs.unthreaded {
		return true
	}

	if thread == "" {
		return stream <= rs.main
	}

	return stream <= rs.threads[thread]
}

// ReadPositionStore - позиции событий отметок прочтения и m.fully_read.
type ReadPositionStore interface {
	EventsByIDs(ctx context.Context, ids []string) ([]Event, error)
	// RoomAccountDataOfType - room account data типа eventType по комнатам.
	RoomAccountDataOfType(ctx context.Context, userID, eventType string) (map[string][]byte, error)
}

// ReadStates собирает отметки прочтения пользователя по комнатам.
type ReadStates struct {
	store     ReadPositionStore
	ephemeral *EphemeralStore
	warn      func(ctx context.Context, msg string, err error)
}

// NewReadStates создает сборщик; ephemeral - receipts в памяти процесса (может
// быть nil), warn - сбои, после которых отметки считаются пустыми.
func NewReadStates(
	store ReadPositionStore, ephemeral *EphemeralStore, warn func(ctx context.Context, msg string, err error),
) *ReadStates {
	if warn == nil {
		warn = func(context.Context, string, error) {}
	}

	return &ReadStates{store: store, ephemeral: ephemeral, warn: warn}
}

// For - отметки прочтения комнат roomIDs (запись есть для каждой комнаты).
// Receipts живут в памяти процесса; после рестарта их нет, и тогда границей
// служит m.fully_read из room account data (она в БД) - иначе все комнаты стали
// бы непрочитанными с момента входа.
func (r *ReadStates) For(ctx context.Context, userID string, roomIDs []string) map[string]*ReadState {
	return r.states(ctx, userID, roomIDs, r.fullyReadMarkers(ctx, userID))
}

func (r *ReadStates) states(
	ctx context.Context, userID string, roomIDs []string, fullyRead map[string]string,
) map[string]*ReadState {
	type mark struct {
		roomID, thread, eventID string
		unthreaded              bool
	}

	var (
		marks []mark
		ids   []string
	)

	for _, roomID := range roomIDs {
		var receipts []ReceiptEvent
		if r.ephemeral != nil {
			receipts = r.ephemeral.UserReceipts(roomID, userID)
		}

		for _, rc := range receipts {
			marks = append(marks, mark{roomID: roomID, thread: rc.ThreadID, eventID: rc.EventID, unthreaded: rc.ThreadID == ""})
			ids = append(ids, rc.EventID)
		}

		if len(receipts) == 0 && fullyRead[roomID] != "" {
			marks = append(marks, mark{roomID: roomID, eventID: fullyRead[roomID], unthreaded: true})
			ids = append(ids, fullyRead[roomID])
		}
	}

	out := make(map[string]*ReadState, len(roomIDs))
	for _, roomID := range roomIDs {
		out[roomID] = &ReadState{}
	}

	if len(ids) == 0 {
		return out
	}

	events, err := r.store.EventsByIDs(ctx, ids)
	if err != nil {
		r.warn(ctx, "receipt positions failed", err)

		return out
	}

	streams := make(map[string]int64, len(events))
	for i := range events {
		streams[events[i].RoomID+"\x00"+events[i].EventID] = events[i].StreamOrdering
	}

	for _, m := range marks {
		stream, ok := streams[m.roomID+"\x00"+m.eventID]
		if !ok {
			continue
		}

		st := out[m.roomID]

		switch {
		case m.unthreaded:
			st.unthreaded = max(st.unthreaded, stream)
		case m.thread == "main":
			st.raise("", stream)
		default:
			st.raise(m.thread, stream)
		}
	}

	return out
}

// fullyReadMarkers - m.fully_read всех комнат пользователя из room account data.
// Берется из БД, а не из ответа /sync: инкрементальный /sync отдает только
// измененную account data, и без отметки комната после рестарта
// считалась бы непрочитанной целиком.
func (r *ReadStates) fullyReadMarkers(ctx context.Context, userID string) map[string]string {
	out := map[string]string{}

	rows, err := r.store.RoomAccountDataOfType(ctx, userID, ReceiptFullyRead)
	if err != nil {
		r.warn(ctx, "fully_read markers failed", err)

		return out
	}

	for roomID, raw := range rows {
		var content struct {
			EventID string `json:"event_id"`
		}

		if json.Unmarshal(raw, &content) == nil && content.EventID != "" {
			out[roomID] = content.EventID
		}
	}

	return out
}
