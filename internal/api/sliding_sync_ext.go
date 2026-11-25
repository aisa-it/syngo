package api

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/aisa-it/syngo/internal/core"
)

// Расширения simplified sliding sync: to_device (MSC3885), e2ee
// (MSC3884), account_data (MSC3959), receipts (MSC3960), typing (MSC3961) - как
// SlidingSyncExtensionHandler Synapse 1.165. Данные - те же сервисы, что у /sync.

// toDeviceAcker - удаление to-device, подтвержденного закладкой (store).
type toDeviceAcker interface {
	DeleteToDeviceUpTo(ctx context.Context, userID, deviceID string, upTo int64) error
}

type slidingExtensionsView struct {
	ToDevice    *slidingToDeviceView    `json:"to_device,omitempty"`
	E2EE        *slidingE2EEView        `json:"e2ee,omitempty"`
	AccountData *slidingAccountDataView `json:"account_data,omitempty"`
	Receipts    *slidingEphemeralView   `json:"receipts,omitempty"`
	Typing      *slidingEphemeralView   `json:"typing,omitempty"`
}

type slidingToDeviceView struct {
	NextBatch string               `json:"next_batch"`
	Events    []slidingToDeviceMsg `json:"events"`
}

// slidingToDeviceMsg - to-device событие: только type, sender и content.
type slidingToDeviceMsg struct {
	Type    string          `json:"type"`
	Sender  string          `json:"sender"`
	Content json.RawMessage `json:"content"`
}

type slidingE2EEView struct {
	DeviceLists *syncDeviceLists `json:"device_lists,omitempty"`
	// Счетчики ключей отдаются всегда (как Synapse: клиент не отличает «нет
	// ключей» от «не изменилось»).
	OTKCount               map[string]int `json:"device_one_time_keys_count"`
	UnusedFallbackKeyTypes []string       `json:"device_unused_fallback_key_types"`
}

type slidingAccountDataView struct {
	Global []accountDataEvent            `json:"global"`
	Rooms  map[string][]accountDataEvent `json:"rooms"`
}

// slidingEphemeralView - receipts и typing: комната -> событие (type, content).
type slidingEphemeralView struct {
	Rooms map[string]ephemeralEvent `json:"rooms"`
}

// slidingExtensions собирает расширения; второй результат - есть ли что отдать
// без ожидания (счетчики ключей не в счет, как в Synapse).
func (s *Server) slidingExtensions(
	ctx context.Context, session core.Session, ext *slidingExtensions, out *core.SlidingSyncOutput,
	from *core.SlidingPos, prev *core.SlidingConnState, upper, ephemeral int64, rooms map[string]*slidingRoomView,
) (slidingExtensionsView, bool) {
	var (
		view     slidingExtensionsView
		nonEmpty bool
	)

	if ext.toDevice {
		view.ToDevice = s.slidingToDevice(ctx, session, ext)
		nonEmpty = nonEmpty || len(view.ToDevice.Events) > 0
	}

	if ext.e2ee {
		view.E2EE = s.slidingE2EE(ctx, session, out, from, upper)
		nonEmpty = nonEmpty || (view.E2EE.DeviceLists != nil &&
			len(view.E2EE.DeviceLists.Changed)+len(view.E2EE.DeviceLists.Left) > 0)
	}

	// Эфемерная позиция из pos новее текущей - токен чужого процесса: считаем
	// ее неизвестной.
	sinceEphemeral := int64(-1)
	if from != nil && from.Ephemeral <= ephemeral {
		sinceEphemeral = from.Ephemeral
	}

	if ext.accountData != nil && s.accountData != nil {
		view.AccountData = s.slidingAccountData(ctx, session.UserID, ext.accountData, out, from, prev, upper)
		nonEmpty = nonEmpty || len(view.AccountData.Global) > 0 || len(view.AccountData.Rooms) > 0
	}

	if ext.receipts != nil && s.ephemeral != nil {
		view.Receipts = s.slidingReceipts(session.UserID, ext.receipts, out, from, prev, sinceEphemeral, rooms)
		nonEmpty = nonEmpty || len(view.Receipts.Rooms) > 0
	}

	if ext.typing != nil && s.ephemeral != nil {
		view.Typing = s.slidingTyping(ext.typing, out, sinceEphemeral)
		nonEmpty = nonEmpty || len(view.Typing.Rooms) > 0
	}

	return view, nonEmpty
}

// extensionRooms - комнаты ответа, к которым применяется расширение: rooms -
// комнаты окон и подписок ("*" - все), lists - комнаты окон этих списков ("*" -
// всех) (find_relevant_room_ids_for_extension Synapse).
func extensionRooms(ext *slidingRoomsExtBody, out *core.SlidingSyncOutput) []string {
	set := map[string]bool{}

	for _, roomID := range ext.Rooms {
		if roomID == "*" {
			for id := range out.Relevant {
				set[id] = true
			}

			break
		}

		if out.Relevant[roomID] {
			set[roomID] = true
		}
	}

	addList := func(list core.SlidingListResult) {
		for _, op := range list.Ops {
			for _, roomID := range op.RoomIDs {
				set[roomID] = true
			}
		}
	}

	for _, name := range ext.Lists {
		if name == "*" {
			for _, list := range out.Lists {
				addList(list)
			}

			break
		}

		if list, ok := out.Lists[name]; ok {
			addList(list)
		}
	}

	ids := make([]string, 0, len(set))
	for roomID := range set {
		ids = append(ids, roomID)
	}

	sort.Strings(ids)

	return ids
}

// slidingToDevice - очередь to-device после закладки since (не больше limit);
// next_batch - последняя отданная позиция, без сообщений - since: свежая
// позиция могла бы перескочить еще не закоммиченное сообщение.
func (s *Server) slidingToDevice(ctx context.Context, session core.Session, ext *slidingExtensions) *slidingToDeviceView {
	view := &slidingToDeviceView{NextBatch: strconv.FormatInt(ext.toDeviceSince, 10), Events: []slidingToDeviceMsg{}}

	pending, _, _, err := s.syncService.PendingToDevice(ctx, session.UserID, session.DeviceID, ext.toDeviceSince)
	if err != nil {
		s.log.WarnContext(ctx, "api: pending to-device failed", "error", err)

		return view
	}

	if len(pending) > ext.toDeviceLimit {
		pending = pending[:ext.toDeviceLimit]
	}

	next := ext.toDeviceSince

	for i := range pending {
		view.Events = append(view.Events, slidingToDeviceMsg{Type: pending[i].Type, Sender: pending[i].Sender, Content: pending[i].Content})
		next = max(next, pending[i].StreamPos)
	}

	view.NextBatch = strconv.FormatInt(next, 10)

	return view
}

// slidingE2EE - счетчики ключей устройства и, в incremental, device_lists:
// changed - пользователи с общими комнатами, менявшие ключи после pos, left -
// переставшие делить комнаты с пользователем.
func (s *Server) slidingE2EE(
	ctx context.Context, session core.Session, out *core.SlidingSyncOutput, from *core.SlidingPos, upper int64,
) *slidingE2EEView {
	view := &slidingE2EEView{
		OTKCount:               s.oneTimeKeyCounts(ctx, session),
		UnusedFallbackKeyTypes: s.unusedFallbackKeyTypes(ctx, session),
	}

	if from != nil {
		since := min(from.Stream, upper)
		view.DeviceLists = &syncDeviceLists{
			Changed: s.deviceListChanges(ctx, session.UserID, true, since, upper),
			Left:    s.deviceListsLeftFor(ctx, session.UserID, out.Left, out.Leavers),
		}
	}

	return view
}

// slidingAccountData - глобальная account data (initial - вся, дальше -
// изменения после pos, m.push_rules - при изменении правил) и по комнатам:
// комнате, которой соединение account data еще не отдавало (или отдало не все),
// - вся, остальным - изменения. Изменения комнат вне окна запоминаются.
func (s *Server) slidingAccountData(
	ctx context.Context, userID string, ext *slidingRoomsExtBody, out *core.SlidingSyncOutput,
	from *core.SlidingPos, prev *core.SlidingConnState, upper int64,
) *slidingAccountDataView {
	view := &slidingAccountDataView{Global: []accountDataEvent{}, Rooms: map[string][]accountDataEvent{}}
	rooms := extensionRooms(ext, out)

	var since int64
	if from != nil {
		since = min(from.Stream, upper)
	}

	full := map[string]bool{}
	relevant := map[string]bool{}

	for _, roomID := range rooms {
		relevant[roomID] = true

		if st, ok := prev.Status(core.SlidingStreamAccountData, roomID); from != nil && (!ok || !st.Live) {
			full[roomID] = true
		}
	}

	global, perRoom, err := s.accountData.SyncChanges(ctx, userID, since, upper, full)
	if err != nil {
		s.log.WarnContext(ctx, "api: account data for sliding sync failed", "error", err)

		return view
	}

	toViews := func(events map[string][]byte) []accountDataEvent {
		types := make([]string, 0, len(events))
		for eventType := range events {
			types = append(types, eventType)
		}

		sort.Strings(types)

		views := make([]accountDataEvent, 0, len(types))
		for _, eventType := range types {
			views = append(views, accountDataEvent{Type: eventType, Content: json.RawMessage(events[eventType])})
		}

		return views
	}

	view.Global = toViews(global)

	unsent := make([]string, 0)

	for roomID, events := range perRoom {
		switch {
		case relevant[roomID] && len(events) > 0:
			view.Rooms[roomID] = toViews(events)
		case !relevant[roomID] && from != nil:
			unsent = append(unsent, roomID)
		}
	}

	out.State.RecordSent(core.SlidingStreamAccountData, rooms...)

	if from != nil {
		sort.Strings(unsent)
		out.State.RecordUnsent(core.SlidingStreamAccountData, since, unsent...)
	}

	return view
}

// slidingReceipts - receipts комнат расширения: комнате, отдаваемой с нуля
// (initial или расширенный timeline), - отметки событий ее timeline и свои; иначе
// - изменения после версии, до которой соединение их отдавало. Изменения в
// комнатах вне окна запоминаются.
func (s *Server) slidingReceipts(
	userID string, ext *slidingRoomsExtBody, out *core.SlidingSyncOutput, from *core.SlidingPos,
	prev *core.SlidingConnState, sinceEphemeral int64, rooms map[string]*slidingRoomView,
) *slidingEphemeralView {
	view := &slidingEphemeralView{Rooms: map[string]ephemeralEvent{}}
	relevantRooms := extensionRooms(ext, out)
	relevant := make(map[string]bool, len(relevantRooms))

	for _, roomID := range relevantRooms {
		relevant[roomID] = true

		since, initial := int64(-1), from == nil

		if room := rooms[roomID]; room != nil && (room.Initial || room.ExpandedTimeline) {
			initial = true
		}

		if !initial {
			switch st, ok := prev.Status(core.SlidingStreamReceipts, roomID); {
			case !ok:
				initial = true
			case st.Live:
				since = sinceEphemeral
			default:
				since = st.Since
			}
		}

		var receipts []core.ReceiptEvent

		if initial {
			receipts = timelineReceipts(s.ephemeral.ReceiptsSince(roomID, userID, -1), userID, rooms[roomID])
		} else {
			receipts = s.ephemeral.ReceiptsSince(roomID, userID, since)
		}

		if len(receipts) > 0 {
			view.Rooms[roomID] = receiptEvent(receipts)
		}
	}

	out.State.RecordSent(core.SlidingStreamReceipts, relevantRooms...)

	if from != nil && sinceEphemeral >= 0 {
		unsent := make([]string, 0)

		for roomID, st := range prev.Receipts {
			if st.Live && !relevant[roomID] && len(s.ephemeral.ReceiptsSince(roomID, userID, sinceEphemeral)) > 0 {
				unsent = append(unsent, roomID)
			}
		}

		sort.Strings(unsent)
		out.State.RecordUnsent(core.SlidingStreamReceipts, sinceEphemeral, unsent...)
	}

	return view
}

// timelineReceipts - отметки событий timeline комнаты ответа и свои: при первой
// отдаче все отметки большой комнаты раздули бы ответ (так требует MSC3960).
func timelineReceipts(all []core.ReceiptEvent, userID string, room *slidingRoomView) []core.ReceiptEvent {
	inTimeline := map[string]bool{}

	if room != nil {
		for i := range room.Timeline {
			inTimeline[room.Timeline[i].EventID] = true
		}
	}

	out := make([]core.ReceiptEvent, 0, len(all))

	for i := range all {
		if all[i].UserID == userID || inTimeline[all[i].EventID] {
			out = append(out, all[i])
		}
	}

	return out
}

// slidingTyping - m.typing комнат расширения, изменившийся после pos (пустой
// список тоже: гаснет индикатор); в initial - где кто-то печатает.
func (s *Server) slidingTyping(ext *slidingRoomsExtBody, out *core.SlidingSyncOutput, sinceEphemeral int64) *slidingEphemeralView {
	view := &slidingEphemeralView{Rooms: map[string]ephemeralEvent{}}

	for _, roomID := range extensionRooms(ext, out) {
		users, changed := s.ephemeral.TypingSince(roomID, sinceEphemeral)
		if sinceEphemeral < 0 {
			changed = len(users) > 0
		}

		if changed {
			sort.Strings(users)
			view.Rooms[roomID] = ephemeralEvent{Type: "m.typing", Content: map[string]any{"user_ids": users}}
		}
	}

	return view
}
