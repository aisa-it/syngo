package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// syncPositionStore — минимальный контракт хранилища для позиции потока: нужен, чтобы
// тесты могли подставить in-memory реализацию вместо PostgreSQL.
type syncPositionStore interface {
	StreamPosition(ctx context.Context) (int64, error)
}

// syncNotifier будит ожидающие long-poll /sync после записи события.
//
// Реализация в процессе: для одного инстанса этого достаточно, а БД-соединение
// на время ожидания не удерживается (ADR-0005 §7). Межинстансовое оповещение
// (LISTEN/NOTIFY) появится вместе с горизонтальным масштабированием.
type syncNotifier struct {
	mu sync.Mutex
	// waiters: канал ожидания → пользователь. Пустой пользователь — слушатель
	// широковещательных событий (федерация, общие комнаты).
	waiters map[chan struct{}]string
}

func newSyncNotifier() *syncNotifier {
	return &syncNotifier{waiters: map[chan struct{}]string{}}
}

// Waiters — число ожидающих long-poll (для метрик).
func (n *syncNotifier) Waiters() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return len(n.waiters)
}

// Notify разбуживает всех ожидающих (событие неизвестного адресата:
// федерация, изменения комнат, профили).
func (n *syncNotifier) Notify() {
	n.mu.Lock()
	defer n.mu.Unlock()

	for ch := range n.waiters {
		close(ch)
		delete(n.waiters, ch)
	}
}

// NotifyUser будит только ожидания этого пользователя (account data, to-device,
// ключи) — адресное пробуждение вместо «разбудить всех».
func (n *syncNotifier) NotifyUser(userID string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for ch, owner := range n.waiters {
		if owner == userID {
			close(ch)
			delete(n.waiters, ch)
		}
	}
}

// Wait ждёт уведомления не дольше timeout и уважает отмену контекста.
// Возвращает true, если пришло уведомление.
func (n *syncNotifier) Wait(ctx context.Context, timeout time.Duration) bool {
	return n.WaitFor(ctx, timeout, "")
}

// WaitFor — ожидание с адресацией по пользователю: будит NotifyUser(этот) и Notify.
func (n *syncNotifier) WaitFor(ctx context.Context, timeout time.Duration, userID string) bool {
	if timeout <= 0 {
		return false
	}

	ch := make(chan struct{})

	n.mu.Lock()
	n.waiters[ch] = userID
	n.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	defer func() {
		n.mu.Lock()
		delete(n.waiters, ch)
		n.mu.Unlock()
	}()

	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// syncResponse — ответ GET /sync (подмножество CS-API v1.11).
type syncResponse struct {
	NextBatch   string            `json:"next_batch"`
	Rooms       syncRooms         `json:"rooms"`
	Presence    ephemeralEvents   `json:"presence"`
	AccountData accountDataEvents `json:"account_data"`
	ToDevice    syncEvents        `json:"to_device"`
	DeviceLists syncDeviceLists   `json:"device_lists"`
	OTKCount    map[string]int    `json:"device_one_time_keys_count"`
	// UnusedFallbackKeyTypes - алгоритмы fallback keys устройства, еще не выданных
	// через /keys/claim; клиент по нему решает, когда загрузить новый.
	UnusedFallbackKeyTypes []string `json:"device_unused_fallback_key_types"`
}

type syncRooms struct {
	Join   map[string]syncJoinedRoom  `json:"join"`
	Invite map[string]syncInvitedRoom `json:"invite"`
	Leave  map[string]syncLeftRoom    `json:"leave"`
}

type syncJoinedRoom struct {
	Timeline    syncTimeline      `json:"timeline"`
	State       syncEvents        `json:"state"`
	Ephemeral   ephemeralEvents   `json:"ephemeral"`
	AccountData accountDataEvents `json:"account_data"`
	// UnreadNotifications - счетчики по push-правилам после отметки прочтения:
	// без них Element не показывает число непрочитанных.
	UnreadNotifications *unreadCounts `json:"unread_notifications,omitempty"`
	// UnreadThreadNotifications - счетчики тредов, если фильтр просит
	// unread_thread_notifications.
	UnreadThreadNotifications map[string]unreadCounts `json:"unread_thread_notifications,omitempty"`
	// Summary - сводка комнаты при lazy_load_members.
	Summary *roomSummaryView `json:"summary,omitempty"`
}

// roomSummaryView - summary комнаты в /sync.
type roomSummaryView struct {
	Heroes  []string `json:"m.heroes"`
	Joined  int      `json:"m.joined_member_count"`
	Invited int      `json:"m.invited_member_count"`
}

type syncInvitedRoom struct {
	InviteState syncEvents `json:"invite_state"`
}

type syncLeftRoom struct {
	Timeline syncTimeline `json:"timeline"`
	State    syncEvents   `json:"state"`
}

type syncTimeline struct {
	Events    []eventView `json:"events"`
	Limited   bool        `json:"limited"`
	PrevBatch string      `json:"prev_batch,omitempty"`
}

type syncEvents struct {
	Events []eventView `json:"events"`
}

// ephemeralEvent — эфемерное событие (m.typing, m.receipt, presence): без event_id и room_id.
// Sender есть только у m.presence.
type ephemeralEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender,omitempty"`
	Content any    `json:"content"`
}

// ephemeralEvents — секция ephemeral/presence в /sync.
type ephemeralEvents struct {
	Events []ephemeralEvent `json:"events"`
}

type syncDeviceLists struct {
	Changed []string `json:"changed"`
	Left    []string `json:"left"`
}

// syncFilter — поддерживаемая часть фильтра (inline JSON или сохраненный по id).
type syncFilter struct {
	Room syncRoomFilter `json:"room"`
}

// syncRoomFilter — room-часть фильтра /sync.
type syncRoomFilter struct {
	Timeline struct {
		eventTypeFilter
		Limit int `json:"limit"`
		// UnreadThreadNotifications - счетчики тредов отдельно.
		UnreadThreadNotifications bool `json:"unread_thread_notifications"`
	} `json:"timeline"`
	State struct {
		LazyLoadMembers bool `json:"lazy_load_members"`
		eventTypeFilter
	} `json:"state"`
}

// applyTypes применяет types/not_types к timeline и state комнат ответа.
// Вызывается после всего, что читает события ответа (presence, unsigned).
func (f syncRoomFilter) applyTypes(resp *syncResponse) {
	for roomID := range resp.Rooms.Join {
		room := resp.Rooms.Join[roomID]
		room.Timeline.Events = f.Timeline.filterEvents(room.Timeline.Events)
		room.State.Events = f.State.filterEvents(room.State.Events)
		resp.Rooms.Join[roomID] = room
	}

	for roomID, room := range resp.Rooms.Leave {
		room.Timeline.Events = f.Timeline.filterEvents(room.Timeline.Events)
		room.State.Events = f.State.filterEvents(room.State.Events)
		resp.Rooms.Leave[roomID] = room
	}
}

// storedFilterCacheLimit - сколько разобранных сохраненных фильтров держать в
// памяти: фильтр неизменяем (новый POST - новый id), а Element шлет id в каждом
// /sync - без кэша это лишний SQL-запрос на каждый sync.
const storedFilterCacheLimit = 10000

// storedSyncFilter - сохраненный фильтр пользователя по id; нет или не
// разбирается - пустой фильтр.
func (s *Server) storedSyncFilter(ctx context.Context, userID, filterID string) syncFilter {
	key := userID + "\x00" + filterID

	if cached, ok := s.syncFilterCache.Load(key); ok {
		if filter, isFilter := cached.(syncFilter); isFilter {
			return filter
		}
	}

	var filter syncFilter

	if s.filters == nil {
		return filter
	}

	raw, err := s.filters.Get(ctx, userID, filterID)
	if err != nil {
		return filter
	}

	if err := json.Unmarshal(raw, &filter); err != nil {
		s.log.WarnContext(ctx, "api: stored sync filter is malformed", "filter_id", filterID, "error", err)

		return syncFilter{}
	}

	if s.syncFilterCount.Add(1) > storedFilterCacheLimit {
		s.syncFilterCache.Clear()
		s.syncFilterCount.Store(1)
	}

	s.syncFilterCache.Store(key, filter)

	return filter
}

// eventTypeFilter - types и not_types фильтра событий: nil types - все типы,
// пустой список - ни одного (спека «Filtering»).
type eventTypeFilter struct {
	Types    []string `json:"types"`
	NotTypes []string `json:"not_types"`
}

// allows - тип события проходит фильтр.
func (f eventTypeFilter) allows(eventType string) bool {
	if f.Types != nil && !eventTypeMatches(eventType, f.Types) {
		return false
	}

	return !eventTypeMatches(eventType, f.NotTypes)
}

// filterEvents оставляет события, проходящие фильтр.
func (f eventTypeFilter) filterEvents(events []eventView) []eventView {
	if f.Types == nil && len(f.NotTypes) == 0 {
		return events
	}

	kept := make([]eventView, 0, len(events))

	for i := range events {
		if f.allows(events[i].Type) {
			kept = append(kept, events[i])
		}
	}

	return kept
}

// handleSync обрабатывает GET /sync: initial и incremental, long-poll, lazy-loading.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s.syncCount.Add(1)

	// Ожидание long-poll не входит в длительность /sync: иначе p99
	// равен timeout клиента и алерт на задержку бесполезен.
	var waited time.Duration

	defer func() { s.syncDuration.observe(time.Since(started) - waited) }()

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.syncStore == nil || s.syncLoader == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Sync storage is not configured")

		return
	}

	query := r.URL.Query()

	// Пустой since — initial sync (позиция 0), как в спеке.
	// sinceEphemeral - версия typing и receipts, которую клиент уже видел; -1 -
	// неизвестна (initial или токен старого формата): тогда отдаются все комнаты.
	var (
		since          int64
		sinceToDevice  int64
		sinceEphemeral int64 = -1
	)

	rawSince := query.Get("since")

	// since от Synapse (клиент пережил переезд со старым токеном): позицию не
	// перевести, ответ - initial sync без событий timeline, как разрыв
	// (synapseSinceGap).
	synapseSince := core.IsSynapseSyncToken(rawSince)
	if synapseSince {
		rawSince = ""

		s.log.InfoContext(r.Context(), "api: sync with Synapse since token, answering with timeline gap",
			"user_id", session.UserID, "device_id", session.DeviceID)
	}

	if rawSince != "" {
		decoded, toDevice, ephemeral, err := s.syncTokens.Decode3(rawSince)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknownPos, "Unknown sync position")

			return
		}

		since = decoded
		sinceToDevice = toDevice
		sinceEphemeral = ephemeral
	}

	// set_presence (по умолчанию online, как в Synapse): статус меняется до
	// снятия версии эфемерных событий, чтобы свое изменение не вернулось следующим
	// long-poll; другие участники будятся только при реальном изменении.
	setPresence := query.Get("set_presence")
	if setPresence != "" && !core.ValidPresence(setPresence) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid set_presence")

		return
	}

	if s.ephemeral != nil && s.ephemeral.SyncPresence(session.UserID, setPresence) {
		s.notifyPresence(r.Context(), session.UserID)
	}

	filter := syncFilter{}

	if raw := query.Get("filter"); raw != "" {
		if strings.HasPrefix(strings.TrimSpace(raw), "{") {
			if err := json.Unmarshal([]byte(raw), &filter); err != nil {
				writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed filter")

				return
			}
		} else {
			// Сохраненный фильтр (POST /user/{userId}/filter) - по id, как в спеке и
			// Synapse: им пользуется Element. Неизвестный или битый id -
			// без фильтра, как раньше, чтобы клиент не застревал на ошибке.
			filter = s.storedSyncFilter(r.Context(), session.UserID, raw)
		}
	}

	perRoom := s.cfg.Sync.PerRoomLimit
	if filter.Room.Timeline.Limit > 0 && filter.Room.Timeline.Limit < perRoom {
		perRoom = filter.Room.Timeline.Limit
	}

	timeout := parseSyncTimeout(query.Get("timeout"), s.cfg.Sync.MaxTimeoutMilliseconds)

	// При завершении работы long-poll не ждёт: отдаём текущую позицию.
	if s.draining.Load() {
		timeout = 0
	}

	// +1 событие, чтобы отличить «ровно limit» от «обрезано»: limited=true только
	// когда в комнате есть еще непрочитанные события (как Synapse).
	// Верхняя граница фиксируется ДО чтения: событие, записанное между чтением
	// данных и позицией, не должно теряться (next_batch не перескочит его).
	// Позиция, прочитанная для long-poll, и есть такая граница: второй
	// запрос позиции нужен только после ожидания.
	upper := int64(-1)

	// Версия эфемерных событий на момент сборки ответа уходит в next_batch: typing
	// и receipts до нее клиент видел. Версия из токена
	// новее текущей - токен чужого процесса со сбитыми часами: считаем ее
	// неизвестной и отдаем все.
	ephemeralVersion := s.ephemeralVersion()
	if sinceEphemeral > ephemeralVersion {
		sinceEphemeral = -1
	}

	// Long-poll: ждём событие без удержания соединения с БД.
	if timeout > 0 {
		if position := s.syncPosition(r.Context()); position > since {
			upper = position
		} else if sinceEphemeral >= 0 && (s.ephemeralActivity() > sinceEphemeral ||
			s.presenceChangedFor(r.Context(), session.UserID, sinceEphemeral)) {
			// Эфемерное изменение пришло, пока клиент был между запросами: отвечаем
			// сразу, а не ждем полный таймаут.
			upper = position
		} else {
			// Ожидание long-poll не входит в метрику длительности /sync.
			waitStarted := time.Now()
			woken := s.syncNotifier.WaitFor(r.Context(), timeout, session.UserID)
			waited = time.Since(waitStarted)

			if !woken {
				// Таймаут или отмена: отдаем пустой ответ с тем же токеном.
				s.writeSyncEmpty(r.Context(), w, since, sinceToDevice, sinceEphemeral, session)

				return
			}

			// Разбудило, возможно, эфемерное изменение: версия - на момент после
			// ожидания, иначе следующий long-poll сразу вернул бы то же еще раз.
			ephemeralVersion = s.ephemeralVersion()
		}
	}

	if upper < 0 {
		upper = s.syncPosition(r.Context())
	}

	lazyMembers := filter.Room.State.LazyLoadMembers

	// На since Synapse события timeline не отдаются: одно на комнату - только
	// чтобы лоадер взял state комнат (он читает его для комнат с событиями).
	loadLimit := perRoom + 1
	if synapseSince {
		loadLimit = 1
	}

	data, err := s.syncLoader.LoadWith(r.Context(), session.UserID, since, loadLimit,
		core.SyncLoadOptions{LazyMembers: lazyMembers})
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// Данные могли содержать события с позицией выше зафиксированной верхней
	// границы (запись прошла между снимком upper и чтением). Их нельзя отдавать:
	// next_batch = upper, и на следующем /sync они пришли бы повторно.
	// Отбрасываем всё, что новее upper, — как это делает Synapse.
	filterTimelineAboveUpper(data, upper)

	// До FillStateAtStart: при пустом timeline state остается текущим.
	if synapseSince {
		clear(data.Timeline)
	}
	syncVis := s.applySyncVisibility(r.Context(), session.UserID, &data)

	// Покинутые/забаненные комнаты: state на момент выхода, иначе клиент с
	// переустановкой не отрисует комнату. Initial sync с
	// обрезанным timeline: state на начало отданных событий, а не текущий (как в
	// Synapse); то же для комнат, куда пользователь только что вошел.
	if s.syncService != nil {
		s.syncService.FillLeaveState(r.Context(), &data, session.UserID)
		s.syncService.FillStateAtStart(r.Context(), &data, session.UserID, since, perRoom, lazyMembers)
	}

	// next_batch — зафиксированная верхняя граница, а не текущая позиция:
	// иначе события, пришедшие во время сборки ответа, выпадают навсегда.
	position := upper

	// Выдача to-device сериализуется по устройству: два параллельных /sync одного
	// устройства иначе получают одну и ту же пачку дважды.
	unlock := s.lockDeviceSync(session.UserID, session.DeviceID)
	defer unlock()

	// to-device и изменения ключей отдаются в этом же ответе; очередь чистится
	// после записи ответа клиенту.
	toDevice, toDeviceIDs, deliveredMax := s.collectToDevice(r.Context(), session, sinceToDevice)
	changedUsers := s.deviceListChanges(r.Context(), session.UserID, rawSince != "", since, upper)
	otk := s.oneTimeKeyCounts(r.Context(), session)

	// next_batch всегда глобальная позиция: закладка to-device живёт отдельно в
	// токене, поэтому опускать позицию событий не нужно и таймлайн не повторяется.
	position = s.nextBatchPosition(r.Context(), session, position)

	resp := s.buildSyncResponse(session.UserID, since, sinceEphemeral, data, filter, toDevice, changedUsers, otk, perRoom)
	if synapseSince {
		s.synapseSinceGap(&resp, upper)
	}

	resp.UnusedFallbackKeyTypes = s.unusedFallbackKeyTypes(r.Context(), session)
	resp.NextBatch = s.syncTokens.Encode3(position,
		s.toDeviceBookmark(r.Context(), sinceToDevice, deliveredMax), ephemeralVersion)
	s.fillAccountData(r.Context(), &resp, session.UserID, since, upper, data.NewlyJoined)
	resp.Presence = s.presenceSection(r.Context(), session.UserID, since, sinceEphemeral, &resp, data.NewlyJoined)
	if rawSince != "" {
		resp.DeviceLists.Left = s.deviceListsLeft(r.Context(), session.UserID, &resp)
	}

	if lazyMembers {
		s.fillRoomSummaries(r.Context(), session.UserID, since, &resp, data.NewlyJoined)
	}

	filter.Room.applyTypes(&resp)

	// Тихие комнаты не отдаются: клиент их уже знает, а раньше
	// пустой incremental нес все join-комнаты (около 200 КБ на 1000 комнат).
	if since > 0 && sinceEphemeral >= 0 {
		pruneQuietRooms(&resp, data.NewlyJoined)
	}

	s.fillUnreadNotifications(r.Context(), &resp, session.UserID, upper, filter.Room.Timeline.UnreadThreadNotifications)

	// unsigned.redacted_because у удаленных событий timeline: без него Element
	// показывает «Unable to decrypt» вместо «Message deleted». Один
	// запрос на все комнаты ответа; срезы делят массивы с ответом.
	timelines := make([][]eventView, 0, len(resp.Rooms.Join)+len(resp.Rooms.Leave))

	for roomID := range resp.Rooms.Join {
		timelines = append(timelines, resp.Rooms.Join[roomID].Timeline.Events)
	}

	for roomID := range resp.Rooms.Leave {
		timelines = append(timelines, resp.Rooms.Leave[roomID].Timeline.Events)
	}

	s.attachRedactions(r.Context(), timelines...)
	s.attachTransactionIDs(r.Context(), session, timelines...)
	attachSyncMembership(session.UserID, &resp)
	// unsigned.m.relations (треды, правки, ссылки) - пачкой на все комнаты.
	s.attachAggregations(r.Context(), session.UserID, syncVis, timelines...)

	writeJSON(w, http.StatusOK, resp)

	// Только теперь, когда ответ ушёл: ошибка удаления даст дубль при повторе,
	// а не потерю сообщения.
	if len(toDeviceIDs) > 0 {
		if err := s.syncService.DeleteToDevice(r.Context(), toDeviceIDs); err != nil {
			s.log.WarnContext(r.Context(), "api: delete delivered to-device failed", "error", err)
		}
	}
}

// deviceListChanges - device_lists.changed для окна (since, upper]: только сам
// пользователь и те, с кем есть общая комната, фильтр - в SQL
// одним запросом.
//
// Окно - позиция событий, а не закладка to-device: раньше изменения ключей
// читались от закладки, которая при пустой очереди остается на 0, и каждый /sync
// заново сканировал всю таблицу и отдавал всех, у кого когда-либо менялись
// ключи. Initial sync device_lists не отдает, как Synapse: клиент после него
// сам запрашивает ключи всех, кого отслеживает. Incremental - это наличие since
// в запросе: на пустом сервере токен бывает и с позицией 0.
func (s *Server) deviceListChanges(ctx context.Context, userID string, incremental bool, since, upper int64) []string {
	if s.syncService == nil || !incremental || upper <= since {
		return []string{}
	}

	changed, err := s.syncService.DeviceListChanges(ctx, userID, since, upper)
	if err != nil {
		s.log.WarnContext(ctx, "api: device list changes failed", "error", err)

		return []string{}
	}

	return changed
}

// writeSyncEmpty отдаёт пустой ответ /sync с текущим токеном (long-poll таймаут).
//
// Закладка to-device - своя, из токена (sinceToDevice): раньше сюда шла позиция
// событий, и закладка по таймауту перескакивала на нее.
func (s *Server) writeSyncEmpty(
	ctx context.Context, w http.ResponseWriter, since, sinceToDevice, sinceEphemeral int64, session core.Session,
) {
	// Таймаут long-poll: next_batch остаётся since (как обещает комментарий и
	// как в Synapse). Свежая позиция «съедала» события, пришедшие во время
	// ожидания, но не попавшие в этот ответ.
	position := since
	if position <= 0 {
		position = s.syncPosition(ctx)
	}

	unlock := s.lockDeviceSync(session.UserID, session.DeviceID)
	defer unlock()

	toDevice, toDeviceIDs, deliveredMax := s.collectToDevice(ctx, session, sinceToDevice)
	otk := s.oneTimeKeyCounts(ctx, session)

	resp := syncResponse{
		NextBatch:   s.syncTokens.Encode3(position, s.toDeviceBookmark(ctx, sinceToDevice, deliveredMax), sinceEphemeral),
		Rooms:       emptySyncRooms(),
		Presence:    ephemeralEvents{Events: []ephemeralEvent{}},
		AccountData: accountDataEvents{Events: []accountDataEvent{}},
		ToDevice:    syncEvents{Events: toDevice},
		// next_batch остается since: изменения ключей после него придут следующим
		// /sync, здесь их не читаем.
		DeviceLists: syncDeviceLists{Changed: []string{}, Left: []string{}},
		OTKCount:    otk,

		UnusedFallbackKeyTypes: s.unusedFallbackKeyTypes(ctx, session),
	}

	// Account data в ответе по таймауту нет: next_batch остается since, и все, что
	// записано после него, придет следующим /sync (раньше здесь каждый раз
	// отдавались все глобальные account data).

	writeJSON(w, http.StatusOK, resp)

	if len(toDeviceIDs) > 0 && s.syncService != nil {
		if err := s.syncService.DeleteToDevice(ctx, toDeviceIDs); err != nil {
			s.log.WarnContext(ctx, "api: delete delivered to-device failed", "error", err)
		}
	}
}

// leaveStateDelta - state покинутой комнаты без того, что клиент уже знает или
// получает в timeline: событий самого timeline и, в incremental, состояния не
// новее since. Как в Synapse: state - это состояние до начала timeline
// (Complement TestLeaveEventVisibility, TestArchivedRoomsHistory).
func leaveStateDelta(state, timeline []core.Event, since int64) []core.Event {
	inTimeline := make(map[string]bool, len(timeline))
	for i := range timeline {
		inTimeline[timeline[i].EventID] = true
	}

	kept := make([]core.Event, 0, len(state))

	for i := range state {
		if inTimeline[state[i].EventID] || (since > 0 && state[i].StreamOrdering <= since) {
			continue
		}

		kept = append(kept, state[i])
	}

	return kept
}

// buildSyncResponse собирает ответ из загруженных данных.
func (s *Server) buildSyncResponse(
	userID string,
	since int64,
	sinceEphemeral int64,
	data core.SyncData,
	filter syncFilter,
	toDevice []eventView,
	changedUsers []string,
	otk map[string]int,
	perRoom int,
) syncResponse {
	resp := syncResponse{
		Rooms:       emptySyncRooms(),
		Presence:    ephemeralEvents{Events: []ephemeralEvent{}},
		AccountData: accountDataEvents{Events: []accountDataEvent{}},
		ToDevice:    syncEvents{Events: toDevice},
		DeviceLists: syncDeviceLists{Changed: changedUsers, Left: []string{}},
		OTKCount:    otk,

		UnusedFallbackKeyTypes: []string{},
	}

	for _, membership := range data.Memberships {
		switch membership.Membership {
		case "invite":
			// Приглашение, которое клиент уже получил (позиция события не новее
			// since), повторно не отдается.
			if since > 0 && membership.LeaveStream > 0 && membership.LeaveStream <= since {
				continue
			}

			state := filterInviteState(data.State[membership.RoomID], userID)
			resp.Rooms.Invite[membership.RoomID] = syncInvitedRoom{InviteState: syncEvents{Events: state}}
		case "leave", "ban":
			// Комнату отдаём один раз: если клиент уже видел позицию выхода,
			// дальше по ней ничего не приходит. События — только до выхода,
			// включая само событие leave/ban.
			if membership.LeaveStream > 0 && since >= membership.LeaveStream {
				continue
			}

			events := data.Timeline[membership.RoomID]

			if membership.LeaveStream > 0 {
				kept := make([]core.Event, 0, len(events))

				for i := range events {
					if events[i].StreamOrdering <= membership.LeaveStream {
						kept = append(kept, events[i])
					}
				}

				events = kept
			}

			resp.Rooms.Leave[membership.RoomID] = syncLeftRoom{
				Timeline: syncTimeline{Events: toEventViews(events), Limited: false},
				// State на момент выхода: name/avatar/alias/topic и членство самого
				// пользователя — этого достаточно, чтобы отрисовать комнату.
				State: syncEvents{Events: toEventViews(leaveStateDelta(data.State[membership.RoomID], events, since))},
			}
		default:
			events := data.Timeline[membership.RoomID]

			// Обрезка: было запрошено limit+1 событие. Если пришло больше limit,
			// отдаём последние limit и ставим limited=true с prev_batch по
			// первому отданному (историю дальше клиент тянет через /messages).
			limited := false

			if perRoom > 0 && len(events) > perRoom {
				events = events[len(events)-perRoom:]
				limited = true
			}

			state := data.State[membership.RoomID]

			if filter.Room.State.LazyLoadMembers {
				state = lazyLoadMembers(state, events, userID)
			}

			room := syncJoinedRoom{
				Timeline: syncTimeline{
					Events:  toEventViews(events),
					Limited: limited,
				},
				State:       syncEvents{Events: toEventViews(state)},
				Ephemeral:   s.ephemeralEvents(membership.RoomID, userID, sinceEphemeral),
				AccountData: accountDataEvents{Events: []accountDataEvent{}},
			}

			// prev_batch — как в Synapse (sync.py): token по первому отданному
			// событию; /messages назад берет события строго раньше него. Раньше
			// здесь было «-1», и событие с соседней позицией терялось на стыке.
			if len(events) > 0 {
				room.Timeline.PrevBatch = s.syncTokens.Encode(events[0].StreamOrdering)
			}

			resp.Rooms.Join[membership.RoomID] = room
		}
	}

	return resp
}

// synapseSinceGap - ответ на since Synapse: timeline комнат пуст и
// помечен limited, prev_batch - на позицию ответа (upper).
//
// Клиент, переживший переезд, присылает since Synapse, который в нашу позицию не
// переводится. Раньше запрос молча обслуживался как initial sync:
// последние сообщения комнат приходили в ответе, неотличимом от incremental, и
// боты обрабатывали их как новые (Element отбрасывает их по event_id, боты -
// нет). Теперь это разрыв: state - текущее состояние комнат целиком (с
// lazy_load_members - только свое членство, остальных клиент берет из summary
// и /members), приглашения, выходы (state на момент выхода), account data,
// эфемерные, непрочитанное и to-device - как в initial; историю клиент при
// желании подгружает через /messages от prev_batch. next_batch - наш токен,
// дальше incremental обычный.
func (s *Server) synapseSinceGap(resp *syncResponse, upper int64) {
	// /messages назад берет события строго раньше from: upper+1 - история до
	// позиции ответа включительно; более новые придут следующим incremental.
	prevBatch := s.syncTokens.Encode(upper + 1)

	for roomID := range resp.Rooms.Join {
		room := resp.Rooms.Join[roomID]
		room.Timeline = syncTimeline{Events: []eventView{}, Limited: true, PrevBatch: prevBatch}
		resp.Rooms.Join[roomID] = room
	}

	for roomID := range resp.Rooms.Leave {
		room := resp.Rooms.Leave[roomID]
		room.Timeline = syncTimeline{Events: []eventView{}, Limited: true, PrevBatch: prevBatch}
		resp.Rooms.Leave[roomID] = room
	}
}

// ephemeralEvents собирает m.typing и m.receipt для комнаты (без запросов к БД):
// изменившиеся после версии since, а при неизвестной версии (since < 0) - все.
func (s *Server) ephemeralEvents(roomID, viewer string, since int64) ephemeralEvents {
	if s.ephemeral == nil {
		return ephemeralEvents{Events: []ephemeralEvent{}}
	}

	events := make([]ephemeralEvent, 0, 2)

	// Пустой список тоже отдаем, если он изменился: так гаснет индикатор набора
	// после typing=false или таймаута. Без версии - пока он недавно
	// менялся.
	users, changed := s.ephemeral.TypingSince(roomID, since)
	if since < 0 {
		changed = len(users) > 0 || s.ephemeral.TypingChanged(roomID)
	}

	if changed {
		events = append(events, ephemeralEvent{Type: "m.typing", Content: map[string]any{"user_ids": users}})
	}

	if receipts := s.ephemeral.ReceiptsSince(roomID, viewer, since); len(receipts) > 0 {
		events = append(events, receiptEvent(receipts))
	}

	return ephemeralEvents{Events: events}
}

// receiptEvent - m.receipt комнаты: event_id -> тип -> пользователь -> ts
// (и thread_id). Общий для /sync и расширения receipts sliding sync.
func receiptEvent(receipts []core.ReceiptEvent) ephemeralEvent {
	content := map[string]any{}

	for _, receipt := range receipts {
		byType, ok := content[receipt.EventID].(map[string]any)
		if !ok {
			byType = map[string]any{}
			content[receipt.EventID] = byType
		}

		byUser, ok := byType[receipt.Type].(map[string]any)
		if !ok {
			byUser = map[string]any{}
			byType[receipt.Type] = byUser
		}

		info := map[string]any{"ts": receipt.TS}
		if receipt.ThreadID != "" {
			info["thread_id"] = receipt.ThreadID
		}

		byUser[receipt.UserID] = info
	}

	return ephemeralEvent{Type: "m.receipt", Content: content}
}

// filterInviteState оставляет минимальный набор state для приглашённой комнаты.
func filterInviteState(state []core.Event, userID string) []eventView {
	// Stripped state приглашения как в спецификации и Synapse (room_prejoin_state):
	// без join_rules, canonical_alias, encryption и членства пригласившего Element
	// называл приглашение в личный чат «Empty room».
	allowed := map[string]bool{
		"m.room.create": true, "m.room.name": true, "m.room.topic": true, "m.room.avatar": true,
		"m.room.join_rules": true, "m.room.canonical_alias": true, "m.room.encryption": true,
	}

	inviter := ""

	for i := range state {
		event := state[i]
		if event.Type == "m.room.member" && event.StateKey != nil && *event.StateKey == userID {
			inviter = event.Sender
		}
	}

	views := make([]eventView, 0, len(state))

	for i := range state {
		event := state[i]

		isMember := event.Type == "m.room.member" && event.StateKey != nil
		isOwnMembership := isMember && *event.StateKey == userID
		isInviter := isMember && inviter != "" && *event.StateKey == inviter

		if allowed[event.Type] || isOwnMembership || isInviter {
			views = append(views, newEventView(event))
		}
	}

	return views
}

// lazyLoadMembers оставляет m.room.member только для отправителей событий порции и самого
// пользователя (ADR-0005 §6). Дополнительных запросов не делает.
func lazyLoadMembers(state, timeline []core.Event, userID string) []core.Event {
	needed := map[string]bool{userID: true}
	for i := range timeline {
		needed[timeline[i].Sender] = true
	}

	filtered := make([]core.Event, 0, len(state))

	for i := range state {
		event := state[i]

		if event.Type != "m.room.member" {
			filtered = append(filtered, event)

			continue
		}

		if event.StateKey != nil && needed[*event.StateKey] {
			filtered = append(filtered, event)
		}
	}

	return filtered
}

// toEventViews конвертирует доменные события в ответ (nil-срез → пустой массив).
func toEventViews(events []core.Event) []eventView {
	views := make([]eventView, 0, len(events))
	for i := range events {
		views = append(views, newEventView(events[i]))
	}

	return views
}

// emptySyncRooms создаёт пустые карты (в JSON они должны быть объектами, не null).
func emptySyncRooms() syncRooms {
	return syncRooms{
		Join:   map[string]syncJoinedRoom{},
		Invite: map[string]syncInvitedRoom{},
		Leave:  map[string]syncLeftRoom{},
	}
}

// parseSyncTimeout разбирает timeout и ограничивает его конфигом.
func parseSyncTimeout(raw string, maxMilliseconds int) time.Duration {
	if raw == "" || maxMilliseconds <= 0 {
		return 0
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}

	if value > maxMilliseconds {
		value = maxMilliseconds
	}

	return time.Duration(value) * time.Millisecond
}

// syncTokenSecret возвращает секрет HMAC из конфига или случайный (dev-режим).
// Случайный секрет означает, что токены не выживают перезапуск — это лучше, чем
// секрет по умолчанию, известный всем (ADR-0005).
func syncTokenSecret(configured string) []byte {
	if configured != "" {
		return []byte(configured)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// crypto/rand не должен падать; если это случилось — лучше явный ноль,
		// чем «секрет» по умолчанию, который известен всем.
		return nil
	}

	return secret
}

// lockDeviceSync берёт устройство-лок на выдачу /sync. Возвращает функцию
// разблокировки. Нужен, чтобы два параллельных /sync одного устройства не
// прочитали одну и ту же пачку to-device дважды.
func (s *Server) lockDeviceSync(userID, deviceID string) func() {
	key := userID + "\x00" + deviceID

	value, _ := s.syncLocks.LoadOrStore(key, &sync.Mutex{})
	mu, _ := value.(*sync.Mutex)
	mu.Lock()

	return func() {
		mu.Unlock()
		s.syncLocks.Delete(key)
	}
}

// nextBatchPosition — позиция событий для next_batch: всегда глобальная.
//
// Раньше её опускали до минимальной недоставленной to-device записи, чтобы
// остаток пачки не потерялся, — из-за этого next_batch оказывался ниже позиции
// клиента и таймлайн повторялся. Теперь закладка to-device хранится отдельным
// компонентом токена.
func (s *Server) nextBatchPosition(_ context.Context, _ core.Session, global int64) int64 {
	return global
}

// toDeviceBookmark — позиция закладки to-device для токена.
//
// Если в этой пачке что-то отдано, закладка = максимальная отданная позиция; если
// очередь пуста — закладка остаётся на `since`. Поднимать её до свежей глобальной
// позиции нельзя: `PUT /sendToDevice`, пришедший между чтением очереди и чтением
// позиции, окажется «позади» закладки, и эта сессия не получит его никогда.
// Строки, которые не отдали, не удаляются, поэтому повторный
// скан по индексу (user_id, device_id) безопасен и дешёвый.
func (s *Server) toDeviceBookmark(_ context.Context, since, deliveredMax int64) int64 {
	if deliveredMax > 0 {
		return deliveredMax
	}

	return since
}

// syncPosition — позиция синхронизации: максимум по событиям, to-device и изменениям ключей.
func (s *Server) syncPosition(ctx context.Context) int64 {
	if s.syncService != nil {
		if position, err := s.syncService.Position(ctx); err == nil {
			return position
		}
	}

	position, err := s.syncStore.StreamPosition(ctx)
	if err != nil {
		return 0
	}

	return position
}

// collectToDevice отдает to-device сообщения устройства; удаляются доставленные
// после записи ответа (доставка одноразовая).
func (s *Server) collectToDevice(
	ctx context.Context, session core.Session, since int64,
) (delivered []eventView, ids []int64, maxPos int64) {
	if s.syncService == nil {
		return []eventView{}, nil, 0
	}

	pending, ids, maxPos, err := s.syncService.PendingToDevice(ctx, session.UserID, session.DeviceID, since)
	if err != nil {
		s.log.WarnContext(ctx, "api: pending to-device failed", "error", err)

		return []eventView{}, nil, 0
	}

	delivered = make([]eventView, 0, len(pending))

	for i := range pending {
		ev := pending[i]
		delivered = append(delivered, eventView{Type: ev.Type, Sender: ev.Sender, Content: ev.Content})
	}

	// Удаляем НЕ здесь: очередь чистится только после того, как ответ реально
	// записан клиенту. Иначе упавший ответ теряет m.room_key навсегда.
	return delivered, ids, maxPos
}

// oneTimeKeyCounts — счётчики one-time keys текущего устройства для /sync.
func (s *Server) oneTimeKeyCounts(ctx context.Context, session core.Session) map[string]int {
	if s.syncService == nil {
		return map[string]int{}
	}

	counts, err := s.syncService.OneTimeKeyCounts(ctx, session.UserID, session.DeviceID)
	if err != nil {
		s.log.WarnContext(ctx, "api: otk counts failed", "error", err)

		return map[string]int{}
	}

	return counts
}

// unusedFallbackKeyTypes - device_unused_fallback_key_types текущего устройства.
// Пустой список отдается всегда: по его наличию клиент понимает, что
// сервер поддерживает fallback keys.
func (s *Server) unusedFallbackKeyTypes(ctx context.Context, session core.Session) []string {
	if s.syncService == nil {
		return []string{}
	}

	types, err := s.syncService.UnusedFallbackKeyTypes(ctx, session.UserID, session.DeviceID)
	if err != nil {
		s.log.WarnContext(ctx, "api: unused fallback keys failed", "error", err)

		return []string{}
	}

	return types
}

// ephemeralVersion - текущая версия typing и receipts (0 без хранилища).
// ephemeralActivity - версия последнего изменения typing/receipts (без presence).
func (s *Server) ephemeralActivity() int64 {
	if s.ephemeral == nil {
		return 0
	}

	return s.ephemeral.ActivityVersion()
}

func (s *Server) ephemeralVersion() int64 {
	if s.ephemeral == nil {
		return 0
	}

	return s.ephemeral.Version()
}

// pruneQuietRooms убирает из incremental-ответа join-комнаты, где после since
// ничего не изменилось: нет событий timeline и state, эфемерных событий
// (typing, receipts - по версии из токена), room account data, и комната не
// новая для пользователя. Счетчики непрочитанного меняются только
// вместе с чем-то из этого: событием (новое, свое, redaction), receipt или
// выходом, - поэтому отдельного признака не нужно. Так же Synapse пропускает
// комнаты без изменений.
func pruneQuietRooms(resp *syncResponse, newlyJoined map[string]bool) {
	for roomID := range resp.Rooms.Join {
		room := resp.Rooms.Join[roomID]
		if newlyJoined[roomID] || len(room.Timeline.Events) > 0 || len(room.State.Events) > 0 ||
			len(room.Ephemeral.Events) > 0 || len(room.AccountData.Events) > 0 {
			continue
		}

		delete(resp.Rooms.Join, roomID)
	}
}
