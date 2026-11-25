package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
)

// relationsResponse — тело GET /rooms/{roomId}/relations/{eventId}.
type relationsResponse struct {
	Chunk     []eventView `json:"chunk"`
	NextBatch string      `json:"next_batch,omitempty"`
}

// handleRelations обрабатывает GET /rooms/{roomId}/relations/{eventId}[/{relType}[/{eventType}]].
// Отдаёт события, ссылающиеся на eventID (реакции, правки, треды).
func (s *Server) handleRelations(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = min(parsed, 100)
	}

	relType := pathParamValue(r, "relType")
	eventType := pathParamValue(r, "eventType")

	// dir по умолчанию — b (самые новые отношения первыми), как в спеке.
	dir := r.URL.Query().Get("dir")
	if dir != "f" {
		dir = "b"
	}

	// Пагинация: from — курсор "s<stream_ordering>" из предыдущего next_batch
	// (SPEC: from/next_batch) или токен Synapse. Пустой from — с
	// начала.
	var from int64

	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, parseErr := s.relationsFrom(r, roomIDParam(r), raw, dir == "b")
		if parseErr != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid from")

			return
		}

		from = parsed
	}

	events, nextBatch, err := s.rooms.Relations(
		r.Context(), session.UserID, roomIDParam(r), eventIDParam(r), relType, eventType, from, dir, limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	events, ok = s.visibleRelations(w, r, session.UserID, roomIDParam(r), eventIDParam(r), events)
	if !ok {
		return
	}

	chunk := toEventViews(events)
	s.attachRedactions(r.Context(), chunk)
	s.attachAggregations(r.Context(), session.UserID, nil, chunk)
	writeJSON(w, http.StatusOK, relationsResponse{Chunk: chunk, NextBatch: nextBatch})
}

// handleThread — GET /rooms/{roomId}/threads/{threadId}: события треда
// (relation rel_type=m.thread). Спека держит маршрут под /v1; раньше его не
// было вовсе, и клиент получал 404.
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = min(parsed, 100)
	}

	threadID, ok := pathParam(r, "threadId")
	if !ok || threadID == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid thread id")

		return
	}

	// Тред тоже листается назад по умолчанию: клиент ждёт конец обсуждения.
	events, nextBatch, err := s.rooms.Relations(
		r.Context(), session.UserID, roomIDParam(r), threadID, "m.thread", "", 0, "b", limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	events, ok = s.visibleRelations(w, r, session.UserID, roomIDParam(r), threadID, events)
	if !ok {
		return
	}

	chunk := toEventViews(events)
	s.attachRedactions(r.Context(), chunk)
	s.attachAggregations(r.Context(), session.UserID, nil, chunk)
	writeJSON(w, http.StatusOK, relationsResponse{Chunk: chunk, NextBatch: nextBatch})
}

// handleThreads — GET /_matrix/client/v1/rooms/{roomId}/threads: корни тредов
// комнаты, самые недавно обновленные первыми. include=participated -
// только треды с участием пользователя (автор корня или ответа). Корни отдаются
// с агрегациями: по m.thread панель тредов Element показывает последний ответ и
// число ответов без запроса на каждый тред.
func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.rooms == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	query := r.URL.Query()

	limit := 10
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = min(parsed, 100)
	}

	var participated bool

	switch query.Get("include") {
	case "", "all":
	case "participated":
		participated = true
	default:
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid include")

		return
	}

	var from int64

	if raw := query.Get("from"); raw != "" {
		parsed, err := s.relationsFrom(r, roomIDParam(r), synapseThreadsToken(raw), true)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid from")

			return
		}

		from = parsed
	}

	roomID := roomIDParam(r)
	q := core.ThreadsQuery{
		RoomID: roomID, UserID: session.UserID, Participated: participated, From: from, Limit: limit,
	}

	var roomVis map[string]core.Visibility

	if vis, filter := s.roomVisibility(r.Context(), session.UserID, roomID); filter {
		q.VisibleBefore, q.VisibleFrom = vis.Before, vis.From
		roomVis = map[string]core.Visibility{roomID: vis}
	}

	events, nextBatch, err := s.rooms.Threads(r.Context(), q)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	chunk := toEventViews(events)
	s.attachRedactions(r.Context(), chunk)
	s.attachAggregations(r.Context(), session.UserID, roomVis, chunk)
	writeJSON(w, http.StatusOK, relationsResponse{Chunk: chunk, NextBatch: nextBatch})
}

// visibleRelations применяет history_visibility к связям события:
// родитель скрыт - 404 (как в Synapse), связи из скрытого промежутка отбрасываются.
// Неизвестный родитель не ошибка: связи могли прийти раньше него. false - ответ
// уже записан.
func (s *Server) visibleRelations(
	w http.ResponseWriter, r *http.Request, userID, roomID, parentID string, events []core.Event,
) ([]core.Event, bool) {
	vis, filter := s.roomVisibility(r.Context(), userID, roomID)
	if !filter {
		return events, true
	}

	if parent, err := s.rooms.Event(r.Context(), userID, roomID, parentID); err == nil &&
		!visibleTo(vis, userID, &parent) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found")

		return nil, false
	}

	events, _ = filterVisible(vis, userID, events)

	return events, true
}

// relationsCursor разбирает наш курсор пагинации relations ("s<pos>" или число).
func relationsCursor(raw string) (int64, error) {
	position, err := strconv.ParseInt(strings.TrimPrefix(raw, "s"), 10, 64)
	if err != nil || position < 0 {
		return 0, fmt.Errorf("api: invalid relations cursor %q", raw)
	}

	return position, nil
}

// relationsFrom - from для /relations и /threads: наш курсор как есть, токен
// Synapse, оставшийся у клиента после переезда, - через перевод позиции комнаты,
// как в /messages.
func (s *Server) relationsFrom(r *http.Request, roomID, raw string, backward bool) (int64, error) {
	if position, err := relationsCursor(raw); err == nil {
		return position, nil
	}

	if !core.IsSynapsePageToken(raw) {
		return 0, fmt.Errorf("api: invalid relations cursor %q", raw)
	}

	return s.rooms.PageCursor(r.Context(), roomID, raw, backward)
}

// synapseThreads - next_batch /threads Synapse (ThreadsNextBatch): позиция
// последнего ответа треда "<topological>_<stream>".
var synapseThreads = regexp.MustCompile(`^(\d+)_(-?\d+)$`)

// synapseThreadsToken переводит next_batch /threads Synapse в topological-токен
// комнаты. Synapse берет следующие треды с последним ответом topological <= T и
// stream < S, то есть до позиции (T, S-1) включительно. Остальное - как есть.
func synapseThreadsToken(raw string) string {
	match := synapseThreads.FindStringSubmatch(raw)
	if match == nil {
		return raw
	}

	stream, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		return raw
	}

	return fmt.Sprintf("t%s-%d", match[1], stream-1)
}
