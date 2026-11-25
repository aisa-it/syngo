package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// Simplified sliding sync (MSC4186):
// POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync. Element X
// работает только через него. Сборка комнат - core.SlidingSyncService, здесь -
// разбор запроса, long-poll, расширения и формат ответа (как
// SlidingSyncRestServlet Synapse 1.165).

// maxSlidingBodySize - тело запроса: room_subscriptions могут перечислять сотни
// комнат, лимит тел аутентификации (64 КБ) для них мал.
const maxSlidingBodySize = 1 << 20

// slidingSyncBody - тело запроса.
type slidingSyncBody struct {
	ConnID            string                     `json:"conn_id"`
	Lists             map[string]slidingListBody `json:"lists"`
	RoomSubscriptions map[string]slidingRoomBody `json:"room_subscriptions"`
	// UnsubscribeRooms снимает «липкие» подписки (так делает matrix-js-sdk).
	UnsubscribeRooms []string              `json:"unsubscribe_rooms"`
	Extensions       slidingExtensionsBody `json:"extensions"`
}

// slidingRoomBody - общие параметры списка и подписки.
type slidingRoomBody struct {
	RequiredState [][]string `json:"required_state"`
	TimelineLimit int        `json:"timeline_limit"`
}

type slidingListBody struct {
	slidingRoomBody
	Ranges  [][]int             `json:"ranges"`
	Filters *slidingFiltersBody `json:"filters"`
}

// slidingFiltersBody - filters списка; nil-список - условия нет, пустой - есть.
type slidingFiltersBody struct {
	IsDM         *bool     `json:"is_dm"`
	Spaces       []string  `json:"spaces"`
	IsEncrypted  *bool     `json:"is_encrypted"`
	IsInvite     *bool     `json:"is_invite"`
	RoomTypes    []*string `json:"room_types"`
	NotRoomTypes []*string `json:"not_room_types"`
	Tags         []string  `json:"tags"`
	NotTags      []string  `json:"not_tags"`
}

// slidingExtensionsBody - расширения; неизвестные игнорируются (MSC4186).
type slidingExtensionsBody struct {
	ToDevice    *slidingToDeviceBody `json:"to_device"`
	E2EE        *slidingToggleBody   `json:"e2ee"`
	AccountData *slidingRoomsExtBody `json:"account_data"`
	Receipts    *slidingRoomsExtBody `json:"receipts"`
	Typing      *slidingRoomsExtBody `json:"typing"`
}

type slidingToggleBody struct {
	Enabled bool `json:"enabled"`
}

type slidingToDeviceBody struct {
	Enabled bool    `json:"enabled"`
	Limit   *int    `json:"limit"`
	Since   *string `json:"since"`
}

// slidingRoomsExtBody - расширение по комнатам: lists и rooms по умолчанию ["*"].
type slidingRoomsExtBody struct {
	Enabled bool     `json:"enabled"`
	Lists   []string `json:"lists"`
	Rooms   []string `json:"rooms"`
}

// slidingExtensions - разобранные расширения.
type slidingExtensions struct {
	toDevice      bool
	toDeviceSince int64
	toDeviceLimit int
	e2ee          bool
	accountData   *slidingRoomsExtBody
	receipts      *slidingRoomsExtBody
	typing        *slidingRoomsExtBody
}

// parseSlidingBody переводит тело в запрос сервиса; ошибка - текст для 400.
func parseSlidingBody(body *slidingSyncBody) (*core.SlidingRequest, *slidingExtensions, string) {
	req := &core.SlidingRequest{
		ConnID: body.ConnID, Lists: map[string]core.SlidingList{}, Subscriptions: map[string]core.SlidingRoomConfig{},
		Unsubscribe: body.UnsubscribeRooms,
	}

	for name, list := range body.Lists {
		cfg, msg := parseSlidingRoom(&list.slidingRoomBody)
		if msg != "" {
			return nil, nil, msg
		}

		ranges := make([][2]int, 0, len(list.Ranges))

		for _, rng := range list.Ranges {
			if len(rng) != 2 || rng[0] < 0 || rng[1] < 0 {
				return nil, nil, "Invalid range in list " + name
			}

			ranges = append(ranges, [2]int{rng[0], rng[1]})
		}

		req.Lists[name] = core.SlidingList{Ranges: ranges, Config: cfg, Filters: parseSlidingFilters(list.Filters)}
	}

	for roomID, sub := range body.RoomSubscriptions {
		cfg, msg := parseSlidingRoom(&sub)
		if msg != "" {
			return nil, nil, msg
		}

		req.Subscriptions[roomID] = cfg
	}

	ext := &slidingExtensions{toDeviceLimit: 100}
	b := body.Extensions

	if b.ToDevice != nil && b.ToDevice.Enabled {
		ext.toDevice = true

		if b.ToDevice.Limit != nil && *b.ToDevice.Limit > 0 && *b.ToDevice.Limit < ext.toDeviceLimit {
			ext.toDeviceLimit = *b.ToDevice.Limit
		}

		if b.ToDevice.Since != nil && *b.ToDevice.Since != "" {
			since, err := strconv.ParseInt(*b.ToDevice.Since, 10, 64)
			if err != nil || since < 0 {
				return nil, nil, "'extensions.to_device.since' is invalid (should look like an int)"
			}

			ext.toDeviceSince = since
		}
	}

	ext.e2ee = b.E2EE != nil && b.E2EE.Enabled
	ext.accountData = enabledRoomsExt(b.AccountData)
	ext.receipts = enabledRoomsExt(b.Receipts)
	ext.typing = enabledRoomsExt(b.Typing)

	return req, ext, ""
}

func parseSlidingRoom(body *slidingRoomBody) (cfg core.SlidingRoomConfig, msg string) {
	pairs := make([][2]string, 0, len(body.RequiredState))

	for _, pair := range body.RequiredState {
		if len(pair) != 2 {
			return core.SlidingRoomConfig{}, "required_state entries must be [event_type, state_key]"
		}

		pairs = append(pairs, [2]string{pair[0], pair[1]})
	}

	return core.SlidingRoomConfig{
		TimelineLimit: min(max(body.TimelineLimit, 0), core.SlidingMaxTimelineLimit),
		RequiredState: core.NewRequiredState(pairs),
	}, ""
}

func parseSlidingFilters(body *slidingFiltersBody) *core.SlidingFilters {
	if body == nil {
		return nil
	}

	return &core.SlidingFilters{
		IsDM: body.IsDM, IsEncrypted: body.IsEncrypted, IsInvite: body.IsInvite, Spaces: body.Spaces,
		RoomTypes: body.RoomTypes, HasRoomTypes: body.RoomTypes != nil,
		NotRoomTypes: body.NotRoomTypes, HasNotRoomTypes: body.NotRoomTypes != nil,
		Tags: body.Tags, HasTags: body.Tags != nil, NotTags: body.NotTags, HasNotTags: body.NotTags != nil,
	}
}

func enabledRoomsExt(body *slidingRoomsExtBody) *slidingRoomsExtBody {
	if body == nil || !body.Enabled {
		return nil
	}

	out := *body
	if out.Lists == nil {
		out.Lists = []string{"*"}
	}

	if out.Rooms == nil {
		out.Rooms = []string{"*"}
	}

	return &out
}

// slidingSyncResponse - ответ (encode_response Synapse).
type slidingSyncResponse struct {
	Pos        string                      `json:"pos"`
	Lists      map[string]slidingListView  `json:"lists,omitempty"`
	Rooms      map[string]*slidingRoomView `json:"rooms"`
	Extensions slidingExtensionsView       `json:"extensions"`
}

type slidingListView struct {
	Count int             `json:"count"`
	Ops   []slidingOpView `json:"ops"`
}

type slidingOpView struct {
	Op      string   `json:"op"`
	Range   [2]int   `json:"range"`
	RoomIDs []string `json:"room_ids"`
}

// slidingRoomView - комната ответа; пустые поля не пишутся, как в Synapse.
type slidingRoomView struct {
	Name              string            `json:"name,omitempty"`
	Avatar            string            `json:"avatar,omitempty"`
	Heroes            []slidingHeroView `json:"heroes,omitempty"`
	Initial           bool              `json:"initial,omitempty"`
	ExpandedTimeline  bool              `json:"unstable_expanded_timeline,omitempty"`
	RequiredState     []eventView       `json:"required_state,omitempty"`
	Timeline          []eventView       `json:"timeline,omitempty"`
	PrevBatch         string            `json:"prev_batch,omitempty"`
	Limited           *bool             `json:"limited,omitempty"`
	NumLive           *int              `json:"num_live,omitempty"`
	BumpStamp         *int64            `json:"bump_stamp,omitempty"`
	JoinedCount       *int              `json:"joined_count,omitempty"`
	InvitedCount      *int              `json:"invited_count,omitempty"`
	NotificationCount int               `json:"notification_count"`
	HighlightCount    int               `json:"highlight_count"`
	IsDM              bool              `json:"is_dm,omitempty"`
	InviteState       []eventView       `json:"invite_state,omitempty"`
}

type slidingHeroView struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// handleSlidingSync - POST .../org.matrix.simplified_msc3575/sync.
func (s *Server) handleSlidingSync(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.slidingSync == nil || s.syncService == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Sync storage is not configured")

		return
	}

	var body slidingSyncBody
	if !decodeBodyLimit(w, r, &body, maxSlidingBodySize) {
		return
	}

	req, ext, msg := parseSlidingBody(&body)
	if msg != "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, msg)

		return
	}

	query := r.URL.Query()

	var from *core.SlidingPos

	if raw := query.Get("pos"); raw != "" {
		pos, err := s.syncTokens.DecodeSliding(raw)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknownPos, "Unknown position")

			return
		}

		from = &pos
	}

	timeout := parseSyncTimeout(query.Get("timeout"), s.cfg.Sync.MaxTimeoutMilliseconds)
	if s.draining.Load() {
		timeout = 0
	}

	ctx := r.Context()

	prev, err := s.loadSlidingConnection(ctx, session, req.ConnID, from)
	if err != nil {
		s.writeSlidingError(w, r, err)

		return
	}

	// Клиент подтвердил закладкой since получение to-device до нее (MSC3885).
	if ext.toDevice && ext.toDeviceSince > 0 && s.toDeviceAck != nil {
		if err := s.toDeviceAck.DeleteToDeviceUpTo(ctx, session.UserID, session.DeviceID, ext.toDeviceSince); err != nil {
			s.log.WarnContext(ctx, "api: delete delivered to-device failed", "error", err)
		}
	}

	result, err := s.slidingLongPoll(ctx, session, req, ext, from, prev, timeout)
	if err != nil {
		s.writeSlidingError(w, r, err)

		return
	}

	if ctx.Err() != nil {
		return
	}

	var prevConn int64
	if from != nil {
		prevConn = from.Conn
	}

	conn, err := s.slidingSync.SaveConnection(ctx, session.UserID, session.DeviceID, req.ConnID, prevConn, result.out.State)
	if err != nil {
		s.writeSlidingError(w, r, err)

		return
	}

	result.resp.Pos = s.syncTokens.EncodeSliding(core.SlidingPos{Conn: conn, Stream: result.upper, Ephemeral: result.ephemeral})
	writeJSON(w, http.StatusOK, result.resp)
}

// loadSlidingConnection - состояние соединения на pos (без pos - пустое).
func (s *Server) loadSlidingConnection(
	ctx context.Context, session core.Session, connID string, from *core.SlidingPos,
) (*core.SlidingConnState, error) {
	if from == nil {
		return core.NewSlidingConnState(), nil
	}

	return s.slidingSync.LoadConnection(ctx, session.UserID, session.DeviceID, connID, *from)
}

// writeSlidingError: неизвестная позиция - 400 M_UNKNOWN_POS (клиент начинает
// соединение заново), остальное - как ошибки комнат.
func (s *Server) writeSlidingError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, core.ErrUnknownPos) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknownPos, "Unknown position")

		return
	}

	if r.Context().Err() != nil {
		return
	}

	s.writeRoomError(w, err)
}

// slidingResult - собранный ответ без pos.
type slidingResult struct {
	resp      *slidingSyncResponse
	out       *core.SlidingSyncOutput
	upper     int64
	ephemeral int64
	nonEmpty  bool
}

// slidingLongPoll собирает ответ и, если в incremental нечего отдать, ждет
// изменений до timeout (как wait_for_sync_for_user Synapse): после каждого
// пробуждения ответ пересобирается. Первый запрос соединения и timeout=0
// отвечают сразу. Ожидание - тот же механизм пробуждений, что у /sync.
func (s *Server) slidingLongPoll(
	ctx context.Context, session core.Session, req *core.SlidingRequest, ext *slidingExtensions,
	from *core.SlidingPos, prev *core.SlidingConnState, timeout time.Duration,
) (*slidingResult, error) {
	deadline := time.Now().Add(timeout)
	rechecked := false

	for {
		upper := s.syncPosition(ctx)
		ephemeral := s.ephemeralVersion()

		result, err := s.slidingResponse(ctx, session, req, ext, from, prev, upper, ephemeral)
		if err != nil {
			return nil, err
		}

		remaining := time.Until(deadline)
		if result.nonEmpty || from == nil || timeout <= 0 || remaining <= 0 {
			return result, nil
		}

		// Изменение пришло во время сборки (до подписки на пробуждение):
		// пересобираем сразу, но не чаще раза на пробуждение - на занятом сервере
		// позиция растет непрерывно.
		if !rechecked && (s.syncPosition(ctx) > upper || s.ephemeralActivity() > ephemeral) {
			rechecked = true

			continue
		}

		if !s.syncNotifier.WaitFor(ctx, remaining, session.UserID) {
			// Таймаут: изменения без пробуждения (позиция выросла) отдаем
			// последней сборкой, как Synapse.
			if position := s.syncPosition(ctx); ctx.Err() == nil && position > upper {
				return s.slidingResponse(ctx, session, req, ext, from, prev, position, s.ephemeralVersion())
			}

			return result, nil
		}

		rechecked = false
	}
}

// slidingResponse - один проход сборки: комнаты (core) и расширения.
func (s *Server) slidingResponse(
	ctx context.Context, session core.Session, req *core.SlidingRequest, ext *slidingExtensions,
	from *core.SlidingPos, prev *core.SlidingConnState, upper, ephemeral int64,
) (*slidingResult, error) {
	out, err := s.slidingSync.Compute(ctx, core.SlidingSyncInput{
		UserID: session.UserID, Request: req, From: from, Upper: upper, Prev: prev, NeedLeavers: ext.e2ee,
	})
	if err != nil {
		return nil, err
	}

	resp := &slidingSyncResponse{Lists: map[string]slidingListView{}, Rooms: s.slidingRoomViews(ctx, session, out, upper)}

	for name, list := range out.Lists {
		view := slidingListView{Count: list.Count, Ops: make([]slidingOpView, 0, len(list.Ops))}
		for _, op := range list.Ops {
			view.Ops = append(view.Ops, slidingOpView{Op: "SYNC", Range: op.Range, RoomIDs: op.RoomIDs})
		}

		resp.Lists[name] = view
	}

	extensions, extNonEmpty := s.slidingExtensions(ctx, session, ext, out, from, prev, upper, ephemeral, resp.Rooms)
	resp.Extensions = extensions

	return &slidingResult{
		resp: resp, out: out, upper: upper, ephemeral: ephemeral, nonEmpty: len(resp.Rooms) > 0 || extNonEmpty,
	}, nil
}

// slidingRoomViews - комнаты в формате ответа: события с unsigned (redaction,
// transaction_id, membership, агрегации) и счетчики непрочитанного, как в /sync.
func (s *Server) slidingRoomViews(
	ctx context.Context, session core.Session, out *core.SlidingSyncOutput, upper int64,
) map[string]*slidingRoomView {
	views := make(map[string]*slidingRoomView, len(out.Rooms))
	timelines := make([][]eventView, 0, len(out.Rooms))
	joined := make([]string, 0, len(out.Rooms))

	for roomID, room := range out.Rooms {
		view := &slidingRoomView{
			Name: room.Name, Avatar: room.Avatar, Initial: room.Initial, ExpandedTimeline: room.ExpandedTimeline,
			RequiredState: toEventViews(room.RequiredState), IsDM: room.IsDM,
		}

		stamp := room.BumpStamp
		view.BumpStamp = &stamp

		for _, hero := range room.Heroes {
			view.Heroes = append(view.Heroes, slidingHeroView(hero))
		}

		if room.Counts != nil {
			joinedCount, invitedCount := room.Counts.Joined, room.Counts.Invited
			view.JoinedCount, view.InvitedCount = &joinedCount, &invitedCount
		}

		if room.HasTimeline {
			view.Timeline = toEventViews(room.Timeline)
			limited := room.Limited
			view.Limited = &limited
			view.PrevBatch = s.syncTokens.Encode(room.PrevBatch)
			fillMembership(session.UserID, room.Membership, view.Timeline)
			timelines = append(timelines, view.Timeline)
		}

		if room.HasNumLive {
			numLive := room.NumLive
			view.NumLive = &numLive
		}

		// Приглашение и просмотр комнаты до входа - stripped state.
		if len(room.InviteState) > 0 {
			view.InviteState = filterInviteState(room.InviteState, session.UserID)
		}

		if room.Membership == "join" {
			joined = append(joined, roomID)
		}

		views[roomID] = view
	}

	s.attachRedactions(ctx, timelines...)
	s.attachTransactionIDs(ctx, session, timelines...)
	s.attachAggregations(ctx, session.UserID, out.Visibility, timelines...)
	s.fillSlidingUnread(ctx, session.UserID, upper, joined, views)

	return views
}

// fillSlidingUnread - notification_count и highlight_count из хранимых
// push-действий (как unread_notifications в /sync, все треды вместе). Synapse
// здесь отдает нули; Element X для незашифрованных комнат берет счетчики сервера.
func (s *Server) fillSlidingUnread(
	ctx context.Context, userID string, upper int64, roomIDs []string, views map[string]*slidingRoomView,
) {
	if s.pushActions == nil || len(roomIDs) == 0 {
		return
	}

	sort.Strings(roomIDs)

	stored, ok := s.pushActions.UnreadCounts(ctx, userID, upper, roomIDs)
	if !ok {
		return
	}

	for _, roomID := range roomIDs {
		for _, c := range stored[roomID] {
			views[roomID].NotificationCount += c.Notification
			views[roomID].HighlightCount += c.Highlight
		}
	}
}
