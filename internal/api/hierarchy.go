package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	// hierarchyDefaultLimit, hierarchyMaxLimit - число комнат на странице ответа
	// (по умолчанию 20, не больше 100).
	hierarchyDefaultLimit = 20
	hierarchyMaxLimit     = 100
	// hierarchySessionTTL - срок next_batch (как сессии пагинации Synapse).
	hierarchySessionTTL = 5 * time.Minute
	// hierarchySessionsMax - предел сессий; при переполнении очищаются истекшие,
	// затем все.
	hierarchySessionsMax = 10000
	// hierarchyRequestBudget - сколько запрос ждет чужие серверы: по истечении
	// отдается собранное и next_batch с остатком очереди.
	hierarchyRequestBudget = 30 * time.Second
)

// hierarchyResponse — тело GET /rooms/{roomId}/hierarchy.
type hierarchyResponse struct {
	Rooms     []spaceRoom `json:"rooms"`
	NextBatch string      `json:"next_batch,omitempty"`
}

// hierarchyQueueItem - комната в очереди обхода.
type hierarchyQueueItem struct {
	roomID string
	// via - серверы из m.space.child родителя: у них спрашиваем чужую комнату.
	via   []string
	depth int
	// remote - сводка из children ответа чужого сервера о родителе.
	remote *spaceSummary
}

// hierarchySession - состояние обхода для next_batch: продолжение допустимо
// только с теми же пользователем, комнатой, suggested_only и max_depth.
type hierarchySession struct {
	userID        string
	roomID        string
	suggestedOnly bool
	maxDepth      int
	queue         []hierarchyQueueItem
	processed     map[string]struct{}
	expires       time.Time
}

// hierarchySessions - сессии пагинации в памяти процесса. Нулевое значение готово.
type hierarchySessions struct {
	mu       sync.Mutex
	sessions map[string]*hierarchySession
}

// put сохраняет сессию и возвращает ее токен.
func (c *hierarchySessions) put(session *hierarchySession) string {
	var raw [16]byte

	_, _ = rand.Read(raw[:])
	token := base64.RawURLEncoding.EncodeToString(raw[:])

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()

	if len(c.sessions) >= hierarchySessionsMax {
		maps.DeleteFunc(c.sessions, func(_ string, s *hierarchySession) bool { return !now.Before(s.expires) })
	}

	if c.sessions == nil || len(c.sessions) >= hierarchySessionsMax {
		c.sessions = map[string]*hierarchySession{}
	}

	session.expires = now.Add(hierarchySessionTTL)
	c.sessions[token] = session

	return token
}

// get - неистекшая сессия по токену.
func (c *hierarchySessions) get(token string) (*hierarchySession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	session, ok := c.sessions[token]
	if !ok || !time.Now().Before(session.expires) {
		return nil, false
	}

	return session, true
}

// hierarchyParams - разобранные параметры запроса.
type hierarchyParams struct {
	limit         int
	maxDepth      int // -1 - без ограничения
	suggestedOnly bool
	from          string
}

// parseHierarchyParams разбирает limit, max_depth, suggested_only и from; при
// ошибке пишет 400.
func parseHierarchyParams(w http.ResponseWriter, r *http.Request) (hierarchyParams, bool) {
	query := r.URL.Query()
	params := hierarchyParams{limit: hierarchyDefaultLimit, maxDepth: -1, from: query.Get("from")}

	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return params, false
		}

		params.limit = min(parsed, hierarchyMaxLimit)
	}

	if raw := query.Get("max_depth"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid max_depth")

			return params, false
		}

		params.maxDepth = parsed
	}

	suggestedOnly, ok := parseHierarchyBool(w, r, "suggested_only")
	params.suggestedOnly = suggestedOnly

	return params, ok
}

// handleHierarchy обрабатывает GET /rooms/{roomId}/hierarchy — обход пространства
// по m.space.child в глубину в порядке спеки (ориентир -
// get_room_hierarchy в synapse/handlers/room_summary.py).
//
// Комнату, в которой есть наш сервер, описываем по своему состоянию; остальные
// - федеративным /hierarchy у серверов из via (сам узел, если нам неизвестен, -
// у сервера из его room_id). Недоступные пользователю комнаты пропускаются.
// max_depth по умолчанию не ограничен; страница - limit комнат, продолжение -
// next_batch (from).
func (s *Server) handleHierarchy(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID := roomIDParam(r)
	if roomID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	params, ok := parseHierarchyParams(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	walker := s.newHierarchyWalker()
	walk := &hierarchySession{
		userID: session.UserID, roomID: roomID, suggestedOnly: params.suggestedOnly,
		maxDepth: params.maxDepth, processed: map[string]struct{}{},
	}

	if params.from != "" {
		saved, ok := s.hierarchySessions.get(params.from)
		if !ok || saved.userID != walk.userID || saved.roomID != roomID ||
			saved.suggestedOnly != walk.suggestedOnly || saved.maxDepth != walk.maxDepth {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Unknown pagination token")

			return
		}

		// Сессия не меняется: повтор запроса с тем же from дает ту же страницу.
		walk.queue = slices.Clone(saved.queue)
		walk.processed = maps.Clone(saved.processed)
	} else {
		root, accessible, err := s.hierarchyRoot(ctx, walker, session.UserID, roomID, params.suggestedOnly)
		if err != nil {
			s.writeRoomError(w, err)

			return
		}

		if !accessible {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden,
				"User "+session.UserID+" not in room "+roomID+", and room previews are disabled")

			return
		}

		walk.queue = []hierarchyQueueItem{root}
	}

	rooms, err := s.walkHierarchy(ctx, walker, walk, params.limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	resp := hierarchyResponse{Rooms: rooms}
	if len(walk.queue) > 0 {
		resp.NextBatch = s.hierarchySessions.put(walk)
	}

	writeJSON(w, http.StatusOK, resp)
}

// walkHierarchy снимает комнаты с очереди walk, пока не наберет limit; дети
// кладутся в обратном порядке, чтобы следующим шел первый по порядку спеки.
func (s *Server) walkHierarchy(ctx context.Context, walker *hierarchyWalker, walk *hierarchySession, limit int) ([]spaceRoom, error) {
	rooms := []spaceRoom{}
	deadline := time.Now().Add(hierarchyRequestBudget)

	for len(walk.queue) > 0 && len(rooms) < limit {
		if len(rooms) > 0 && time.Now().After(deadline) {
			break
		}

		item := walk.queue[len(walk.queue)-1]
		walk.queue = walk.queue[:len(walk.queue)-1]

		if _, done := walk.processed[item.roomID]; done {
			continue
		}

		entry, err := s.hierarchyEntry(ctx, walker, walk.userID, item, walk.suggestedOnly, walk.maxDepth)
		if err != nil {
			return nil, err
		}

		walk.processed[item.roomID] = struct{}{}

		if entry.room == nil {
			continue
		}

		room := *entry.room
		room.AllowedRoomIDs = nil // только для федерации
		rooms = append(rooms, room)

		if walk.maxDepth >= 0 && item.depth >= walk.maxDepth {
			continue
		}

		for i := len(room.ChildrenState) - 1; i >= 0; i-- {
			ev := room.ChildrenState[i]
			if ev.Type != "m.space.child" || entry.inaccessible[ev.StateKey] {
				continue
			}

			child := hierarchyQueueItem{roomID: ev.StateKey, via: childVia(ev.Content), depth: item.depth + 1}
			if summary, ok := entry.children[ev.StateKey]; ok {
				child.remote = &summary
			}

			walk.queue = append(walk.queue, child)
		}
	}

	// Уже обойденные комнаты в хвосте очереди не повод для next_batch.
	for len(walk.queue) > 0 {
		if _, done := walk.processed[walk.queue[len(walk.queue)-1].roomID]; !done {
			break
		}

		walk.queue = walk.queue[:len(walk.queue)-1]
	}

	return rooms, nil
}

// childVia - серверы из content.via m.space.child.
func childVia(content json.RawMessage) []string {
	var parsed struct {
		Via []string `json:"via"`
	}

	_ = json.Unmarshal(content, &parsed)

	return parsed.Via
}

// hierarchyEntryResult - комната для ответа (nil - недоступна) и данные чужого
// сервера о ее детях.
type hierarchyEntryResult struct {
	room         *spaceRoom
	children     map[string]spaceSummary
	inaccessible map[string]bool
}

// hierarchyEntry - сводка комнаты очереди для пользователя: своя - по нашему
// состоянию, чужая - из сводки родителя (не пространство или предел глубины)
// или федеративным /hierarchy; чужая проверяется на доступ пользователю.
func (s *Server) hierarchyEntry(
	ctx context.Context, walker *hierarchyWalker, userID string, item hierarchyQueueItem, suggestedOnly bool, maxDepth int,
) (hierarchyEntryResult, error) {
	st, err := walker.state(ctx, item.roomID)
	if err != nil {
		return hierarchyEntryResult{}, err
	}

	if walker.hostJoined(st) {
		accessible, err := walker.localAccessible(ctx, st, userID, "")
		if err != nil || !accessible {
			return hierarchyEntryResult{}, err
		}

		room := walker.localEntry(st, suggestedOnly, true, false)

		return hierarchyEntryResult{room: &room}, nil
	}

	var result hierarchyEntryResult

	switch {
	case item.remote != nil && (item.remote.RoomType != roomTypeSpace || (maxDepth >= 0 && item.depth >= maxDepth)):
		result.room = &spaceRoom{spaceSummary: *item.remote, ChildrenState: []spaceChildEvent{}}
	default:
		space, ok := s.remoteHierarchy(ctx, item.roomID, item.via, suggestedOnly)
		if !ok {
			return hierarchyEntryResult{}, nil
		}

		result = hierarchyEntryResult{room: &space.room, children: space.children, inaccessible: space.inaccessible}
	}

	accessible, err := walker.remoteAccessible(ctx, st, userID, result.room.spaceSummary)
	if err != nil || !accessible {
		return hierarchyEntryResult{}, err
	}

	return result, nil
}

// hierarchyRoot - проверка доступа к узлу обхода. Узел, в котором есть наш
// сервер, или с room_id без чужого сервера проверяется по нашему состоянию;
// иначе - по сводке сервера из room_id (предпросмотр чужого пространства),
// а при отказе - по нашему состоянию (например, приглашение).
func (s *Server) hierarchyRoot(
	ctx context.Context, walker *hierarchyWalker, userID, roomID string, suggestedOnly bool,
) (hierarchyQueueItem, bool, error) {
	root := hierarchyQueueItem{roomID: roomID}

	st, err := walker.state(ctx, roomID)
	if err != nil {
		return root, false, err
	}

	if !walker.hostJoined(st) && s.isRemote(roomID) {
		root.via = []string{domainOf(roomID)}

		if space, ok := s.remoteHierarchy(ctx, roomID, root.via, suggestedOnly); ok {
			accessible, err := walker.remoteAccessible(ctx, st, userID, space.room.spaceSummary)
			if err != nil || accessible {
				return root, accessible, err
			}
		}
	}

	accessible, err := walker.localAccessible(ctx, st, userID, "")

	return root, accessible, err
}

// remoteAccessible - чужую комнату можно показать пользователю
// (_is_remote_room_accessible в Synapse): public/knock/knock_restricted,
// world_readable, участник комнаты из allowed_room_ids или доступ по нашему
// состоянию комнаты.
func (w *hierarchyWalker) remoteAccessible(ctx context.Context, st hierarchyRoomState, userID string, summary spaceSummary) (bool, error) {
	switch summary.JoinRule {
	case "public", "knock", "knock_restricted":
		return true, nil
	}

	if summary.WorldReadable {
		return true, nil
	}

	if in, err := w.userInAny(ctx, summary.AllowedRoomIDs, userID); err != nil || in {
		return in, err
	}

	return w.localAccessible(ctx, st, userID, "")
}
