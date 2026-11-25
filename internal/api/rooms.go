package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/join"
)

// maxRoomsBodySize ограничивает тело запросов комнат.
const maxRoomsBodySize = 256 << 10

// eventView — представление события в ответах CS-API.
type eventView struct {
	EventID        string          `json:"event_id"`
	Type           string          `json:"type"`
	Sender         string          `json:"sender"`
	RoomID         string          `json:"room_id"`
	StateKey       *string         `json:"state_key,omitempty"`
	Content        json.RawMessage `json:"content"`
	OriginServerTS int64           `json:"origin_server_ts"`
	Unsigned       map[string]any  `json:"unsigned"`
	// Redacts — цель m.room.redaction на верхнем уровне: для комнат v1-v10 клиенты
	// читают ее отсюда (в v11 она в content, отдаем в обоих местах, как Synapse).
	Redacts string `json:"redacts,omitempty"`

	// redactedBy — id события удаления; по нему attachRedactions добавляет
	// unsigned.redacted_because (не сериализуется).
	redactedBy string
}

// newEventView конвертирует доменное событие в ответ API.
func newEventView(event core.Event) eventView {
	content := event.Content
	if len(content) == 0 {
		content = json.RawMessage(`{}`)
	}

	return eventView{
		EventID:        event.EventID,
		Type:           event.Type,
		Sender:         event.Sender,
		RoomID:         event.RoomID,
		StateKey:       event.StateKey,
		Content:        content,
		OriginServerTS: event.OriginServerTS,
		Unsigned:       map[string]any{},
		Redacts:        clientRedactionTarget(event),
		redactedBy:     event.RedactedBy,
	}
}

// clientRedactionTarget — цель m.room.redaction из content.redacts (пусто для других типов).
func clientRedactionTarget(event core.Event) string {
	if event.Type != "m.room.redaction" {
		return ""
	}

	var body struct {
		Redacts string `json:"redacts"`
	}

	if json.Unmarshal(event.Content, &body) != nil {
		return ""
	}

	return body.Redacts
}

// attachRedactions добавляет удаленным событиям unsigned.redacted_because - само
// событие удаления, как Synapse. Без него Element не понимает, что сообщение удалено,
// и вместо «Message deleted» показывает «Unable to decrypt». События
// удаления читает core.RedactionLookup.
//
// Принимает несколько срезов: /sync передает таймлайны всех комнат сразу, и
// события удаления читаются одним запросом, а не запросом на комнату.
func (s *Server) attachRedactions(ctx context.Context, groups ...[]eventView) {
	if s.redactions == nil {
		return
	}

	ids := make([]string, 0)

	for _, views := range groups {
		for i := range views {
			if views[i].redactedBy != "" {
				ids = append(ids, views[i].redactedBy)
			}
		}
	}

	if len(ids) == 0 {
		return
	}

	byID, err := s.redactions.ByID(ctx, ids)
	if err != nil {
		s.log.WarnContext(ctx, "api: load redaction events failed", "error", err)
	}

	for _, views := range groups {
		for i := range views {
			id := views[i].redactedBy
			if id == "" {
				continue
			}

			if ev, ok := byID[id]; ok {
				views[i].Unsigned["redacted_because"] = newEventView(ev)

				continue
			}

			views[i].Unsigned["redacted_because"] = map[string]any{
				"event_id": id, "type": "m.room.redaction", "room_id": views[i].RoomID,
				"redacts": views[i].EventID, "content": map[string]any{"redacts": views[i].EventID},
			}
		}
	}
}

// normalizeJSON переводит JSON в компактный вид с UTF-8 (без \uXXXX escape-последовательностей).
// Это ближе к canonical JSON Synapse и избавляет от проблем с серверами БД в SQL_ASCII.
func normalizeJSON(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return normalized, nil
}

// createRoomRequest — тело POST /createRoom (поддерживаемое подмножество).
type createRoomRequest struct {
	Name         string                   `json:"name"`
	Topic        string                   `json:"topic"`
	Preset       string                   `json:"preset"`
	RoomVersion  string                   `json:"room_version"`
	Invite       []string                 `json:"invite"`
	InitialState []core.StateEventRequest `json:"initial_state"`
	IsDirect     bool                     `json:"is_direct"`
	// RoomAliasName — localpart алиаса, который создаётся вместе с комнатой
	// (спека createRoom, Complement TestRoomCanonicalAlias).
	RoomAliasName string `json:"room_alias_name"`
	// CreationContent — поля m.room.create (например, {"type":"m.space"}).
	CreationContent json.RawMessage `json:"creation_content"`
	// Visibility — public публикует комнату в каталоге и (без preset) выбирает public_chat.
	Visibility string `json:"visibility"`
	// PowerLevelContentOverride — правки уровней по умолчанию (Element шлет всегда).
	PowerLevelContentOverride map[string]any `json:"power_level_content_override"`
}

// writeRoomError переводит доменные ошибки комнат в коды Matrix.
func (s *Server) writeRoomError(w http.ResponseWriter, err error) {
	// Отказ сервиса с текстом для клиента (например, неверный токен пагинации -
	// M_INVALID_PARAM).
	if writeRequestError(w, err) {
		return
	}

	switch {
	case errors.Is(err, core.ErrStillJoined):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "User is in room")
	case errors.Is(err, core.ErrRoomBlocked):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Room is blocked")
	case errors.Is(err, core.ErrForbidden):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "You are not allowed to do that")
	case errors.Is(err, core.ErrNotAllowed):
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Insufficient power level")
	case errors.Is(err, core.ErrInvalidAliasContent):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid canonical alias")
	case errors.Is(err, core.ErrBadAlias):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadAlias, "Alias does not point to this room")
	case errors.Is(err, core.ErrNotFound):
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Not found")
	case errors.Is(err, core.ErrInvalidRoomVersion):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnsupportedRoomVersion, "Unsupported room version")
	case errors.Is(err, core.ErrBadJSON):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Invalid parameter")
	case errors.Is(err, core.ErrTooLarge):
		writeMatrixError(w, http.StatusRequestEntityTooLarge, "M_TOO_LARGE", "Event is too large")
	default:
		s.log.Error("api: room failure", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
	}
}

// decodeRoomBody читает тело запроса комнат (пустое тело допустимо).
// decodeFederationBody — как decodeRoomBody, но сохраняет числа как json.Number.
// Иначе при пересборке подписанного JSON большие целые (origin_server_ts, depth)
// уходят в экспоненту (`1.79e+12`) и подпись пира не сходится.
func decodeFederationBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize))
	decoder.UseNumber()

	if err := decoder.Decode(target); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return false
	}

	return true
}

// decodeOptionalRoomBody - как decodeRoomBody, но пустое тело допустимо (POST /join
// без тела).
func decodeOptionalRoomBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRoomsBodySize))
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not read request body")

		return false
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}

	if err := json.Unmarshal(body, target); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON")

		return false
	}

	return true
}

func decodeRoomBody(w http.ResponseWriter, r *http.Request, target any) bool {
	var payload json.RawMessage

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&payload); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return false
	}

	if len(payload) == 0 {
		return true
	}

	if err := json.Unmarshal(payload, target); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON")

		return false
	}

	return true
}

// handleCreateRoom обрабатывает POST /createRoom.
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var req createRoomRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	req.Invite = core.UniqueInvitees(req.Invite)

	for i := range req.InitialState {
		normalized, err := normalizeJSON(req.InitialState[i].Content)
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed initial_state content")

			return
		}

		req.InitialState[i].Content = normalized
	}

	version := 0
	if req.RoomVersion != "" {
		if err := parseRoomVersion(req.RoomVersion, &version); err != nil {
			s.writeRoomError(w, err)

			return
		}
	}

	roomID, err := s.rooms.CreateRoom(r.Context(), core.CreateRoomRequest{
		Creator:            session.UserID,
		Name:               req.Name,
		Topic:              req.Topic,
		RoomVersion:        version,
		Preset:             req.Preset,
		IsPublic:           req.Preset == "public_chat" || (req.Preset == "" && req.Visibility == "public"),
		Invite:             req.Invite,
		InitialState:       req.InitialState,
		CreationExtras:     parseCreationExtras(req.CreationContent),
		PowerLevelOverride: req.PowerLevelContentOverride,
		IsDirect:           req.IsDirect,
	})
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// room_alias_name: алиас создаётся сразу с комнатой, иначе клиенту нужно два
	// запроса, а canonical_alias остаётся пустым.
	var createdAlias string

	if aliasName := strings.TrimSpace(req.RoomAliasName); aliasName != "" {
		alias := "#" + aliasName + ":" + s.cfg.Matrix.ServerName
		createdAlias = alias

		if err := s.rooms.SetAlias(r.Context(), session.UserID, roomID, alias); err != nil {
			// Комната уже создана — удалить её нельзя без частичного состояния,
			// поэтому отдаём ошибку алиаса как есть (M_ROOM_IN_USE/M_INVALID_PARAM).
			// room_id в логе обязателен: иначе по жалобе «комната без алиаса» не
			// понять, о какой комнате речь.
			s.log.WarnContext(r.Context(), "api: createRoom alias failed",
				"room_id", roomID, "alias", alias, "user_id", session.UserID, "error", err)
			s.writeRoomError(w, err)

			return
		}

		if _, _, err := s.rooms.SendState(r.Context(), session.UserID, session.DeviceID, roomID,
			"m.room.canonical_alias", "", "", []byte(`{"alias":"`+alias+`"}`)); err != nil {
			s.log.WarnContext(r.Context(), "api: createRoom canonical_alias failed",
				"room_id", roomID, "alias", alias, "error", err)
		}
	}

	// Приглашённые на чужих серверах: createRoom создаёт только локальное
	// membership-событие, федеративный invite нужно отправить отдельно.
	for _, userID := range req.Invite {
		dest := inviteDestination(userID)
		if dest == "" || dest == s.cfg.Matrix.ServerName {
			continue
		}

		// Событие пишется только после доставки: недоставленное
		// приглашение не остается в комнате.
		if _, inviteErr := s.inviteRemoteUser(r.Context(), session.UserID, roomID, userID); inviteErr != nil {
			s.log.WarnContext(r.Context(), "api: createRoom remote invite failed", "room_id", roomID, "user_id", userID, "error", inviteErr)
		}
	}

	// Push локальным приглашенным (.m.rule.invite_for_me): приглашения
	// createRoom создает внутри комнаты, событие берется из состояния.
	for _, userID := range req.Invite {
		if dest := inviteDestination(userID); dest != s.cfg.Matrix.ServerName {
			continue
		}

		if ev, err := s.rooms.StateEvent(r.Context(), session.UserID, roomID, "m.room.member", userID); err == nil {
			s.dispatchPushes(r.Context(), ev)
		}
	}

	s.eventsWritten(r.Context())

	resp := map[string]string{"room_id": roomID}
	if createdAlias != "" {
		resp["room_alias"] = createdAlias
	}

	writeJSON(w, http.StatusOK, resp)
}

// eventsWritten - событие записано: сигнал фоновому разбору push-действий
// (счетчики непрочитанного) и пробуждение long-poll. Запрос разбора
// не ждет: раньше синхронный догон под общим мьютексом
// сериализовал все отправки и /sync. /sync, проснувшийся по событию, ждет
// разбора до своей позиции ограниченное время.
func (s *Server) eventsWritten(ctx context.Context) {
	if s.pushActions != nil {
		s.pushActions.EventsWritten(ctx)
	}

	s.notifySync()
}

// notifySync будит ожидающие long-poll /sync (ADR-0005 §7).
func (s *Server) notifySync() {
	if s.syncNotifier != nil {
		s.syncNotifier.Notify()
	}
}

// notifySyncUser — адресное пробуждение: только ожидания этого пользователя
// (account data, личные изменения), остальных не трогаем.
func (s *Server) notifySyncUser(userID string) {
	if s.syncNotifier != nil {
		s.syncNotifier.NotifyUser(userID)
	}
}

// parseRoomVersion переводит строку версии комнаты в число (поддерживаем v10 и v11).
func parseRoomVersion(raw string, out *int) error {
	value, err := strconv.Atoi(raw)
	if err != nil || value < core.DefaultRoomVersion || value > core.MaxRoomVersion {
		return core.ErrInvalidRoomVersion
	}

	*out = value

	return nil
}

// handleSend обрабатывает PUT /rooms/{roomId}/send/{eventType}/{txnId}.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID := roomIDParam(r)
	eventType := pathParamValue(r, "eventType")
	txnID := pathParamValue(r, "txnId")

	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRoomsBodySize)).Decode(&raw); err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeNotJSON, "Could not parse request body")

		return
	}

	content, err := normalizeJSON(raw)
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed JSON")

		return
	}

	eventID, _, err := s.rooms.SendMessage(
		r.Context(), session.UserID, session.DeviceID, roomID, eventType, txnID, content,
	)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// Fan-out в федерацию - outbox в транзакции записи события.

	// Push-уведомления участникам с http-pushers, тоже асинхронно.
	s.dispatchPushes(r.Context(), core.Event{
		EventID: eventID, RoomID: roomID, Type: eventType, Sender: session.UserID, Content: content,
	})

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{"event_id": eventID})
}

// handleRoomEvent обрабатывает GET /rooms/{roomId}/event/{eventId}.
func (s *Server) handleRoomEvent(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	event, err := s.rooms.VisibleEvent(r.Context(), session.UserID,
		roomIDParam(r), eventIDParam(r))
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	roomVis := s.visibility(r.Context(), session.UserID, []string{event.RoomID})
	if vis, ok := roomVis[event.RoomID]; ok && !visibleTo(vis, session.UserID, &event) {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Event not found")

		return
	}

	view := []eventView{newEventView(event)}
	s.attachRedactions(r.Context(), view)
	s.attachTransactionIDs(r.Context(), session, view)
	s.attachAggregations(r.Context(), session.UserID, roomVis, view)
	writeJSON(w, http.StatusOK, view[0])
}

// messagesResponse — тело GET /rooms/{roomId}/messages.
type messagesResponse struct {
	Start string      `json:"start"`
	End   string      `json:"end,omitempty"`
	Chunk []eventView `json:"chunk"`
	// State - m.room.member отправителей chunk при lazy_load_members.
	State []eventView `json:"state,omitempty"`
}

// handleMessages обрабатывает GET /rooms/{roomId}/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	query := r.URL.Query()

	limit := 10
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Invalid limit")

			return
		}

		limit = parsed
	}

	var filter messagesFilter

	if raw := query.Get("filter"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &filter); err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Malformed filter")

			return
		}
	}

	result, err := s.rooms.Messages(r.Context(), session.UserID, roomIDParam(r),
		s.messagesCursor(query.Get("from")), query.Get("dir"), limit)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	// history_visibility: скрытые события не отдаются; дошли до границы при
	// пагинации назад - end не отдаем, и клиент прекращает подгрузку.
	roomVis := s.visibility(r.Context(), session.UserID, []string{roomIDParam(r)})
	if vis, ok := roomVis[roomIDParam(r)]; ok {
		var hidden bool

		result.Events, hidden = filterVisible(vis, session.UserID, result.Events)
		if hidden && query.Get("dir") != "f" {
			result.End = ""
		}
	}

	// RoomEventFilter: события не того типа или без url отсеиваются после
	// выборки; курсоры end/start остаются по выбранной странице, клиент листает
	// дальше (Complement TestRoomImageRoundtrip).
	result.Events = filter.apply(result.Events)

	resp := messagesResponse{Start: result.Start, End: result.End, Chunk: make([]eventView, 0, len(result.Events))}
	for i := range result.Events {
		resp.Chunk = append(resp.Chunk, newEventView(result.Events[i]))
	}

	if filter.LazyLoadMembers {
		resp.State = s.lazyMembers(r, session.UserID, roomIDParam(r), result.Events)
	}

	s.attachRedactions(r.Context(), resp.Chunk)
	s.attachTransactionIDs(r.Context(), session, resp.Chunk)
	s.attachAggregations(r.Context(), session.UserID, roomVis, resp.Chunk)
	writeJSON(w, http.StatusOK, resp)
}

// messagesFilter - поддерживаемая часть RoomEventFilter для /messages.
type messagesFilter struct {
	LazyLoadMembers bool     `json:"lazy_load_members"`
	ContainsURL     *bool    `json:"contains_url"`
	Types           []string `json:"types"`
	NotTypes        []string `json:"not_types"`
	// RelTypes и NotRelTypes - фильтр по m.relates_to.rel_type (MSC3874).
	RelTypes    []string `json:"org.matrix.msc3874.rel_types"`
	NotRelTypes []string `json:"org.matrix.msc3874.not_rel_types"`
}

// apply оставляет события, подходящие под types, not_types и contains_url.
func (f messagesFilter) apply(events []core.Event) []core.Event {
	if f.ContainsURL == nil && f.Types == nil && len(f.NotTypes) == 0 && f.RelTypes == nil && len(f.NotRelTypes) == 0 {
		return events
	}

	kept := events[:0:0]

	for i := range events {
		ev := events[i]

		if f.Types != nil && !eventTypeMatches(ev.Type, f.Types) {
			continue
		}

		if eventTypeMatches(ev.Type, f.NotTypes) {
			continue
		}

		if f.ContainsURL != nil && hasContentURL(ev.Content) != *f.ContainsURL {
			continue
		}

		if f.RelTypes != nil || len(f.NotRelTypes) > 0 {
			relType := relationType(ev.Content)
			if (f.RelTypes != nil && !slices.Contains(f.RelTypes, relType)) ||
				(relType != "" && slices.Contains(f.NotRelTypes, relType)) {
				continue
			}
		}

		kept = append(kept, ev)
	}

	return kept
}

// eventTypeMatches - тип подходит под один из шаблонов (звездочка в конце -
// префикс, как в фильтрах Synapse).
func eventTypeMatches(eventType string, patterns []string) bool {
	for _, pattern := range patterns {
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		} else if pattern == eventType {
			return true
		}
	}

	return false
}

// relationType - m.relates_to.rel_type события ("" - без связи).
func relationType(content json.RawMessage) string {
	var body struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
		} `json:"m.relates_to"`
	}

	if json.Unmarshal(content, &body) != nil {
		return ""
	}

	return body.RelatesTo.RelType
}

// hasContentURL - в content есть строковый url (вложение).
func hasContentURL(content json.RawMessage) bool {
	var body struct {
		URL json.RawMessage `json:"url"`
	}

	if json.Unmarshal(content, &body) != nil || len(body.URL) == 0 {
		return false
	}

	var url string

	return json.Unmarshal(body.URL, &url) == nil
}

// lazyMembers - текущие m.room.member отправителей событий (lazy_load_members в
// /messages, как в Synapse, Complement TestRoomMessagesLazyLoading).
func (s *Server) lazyMembers(r *http.Request, userID, roomID string, events []core.Event) []eventView {
	senders := make(map[string]bool, len(events))
	for i := range events {
		senders[events[i].Sender] = true
	}

	if len(senders) == 0 {
		return nil
	}

	state, err := s.rooms.StateEvents(r.Context(), userID, roomID)
	if err != nil {
		s.log.WarnContext(r.Context(), "api: lazy members failed", "room_id", roomID, "error", err)

		return nil
	}

	members := make([]core.Event, 0, len(senders))

	for i := range state {
		ev := state[i]
		if ev.Type == "m.room.member" && ev.StateKey != nil && senders[*ev.StateKey] {
			members = append(members, ev)
		}
	}

	return eventViews(members)
}

// messagesCursor приводит from к формату /messages: клиент приносит prev_batch
// из /sync (подписанный токен вида "<ver>.e<pos>.<mac>"), а пагинация /messages
// внутри ходит по "s<pos>". Иначе from из /sync отвергался.
func (s *Server) messagesCursor(from string) string {
	if from == "" || strings.HasPrefix(from, "s") {
		return from
	}

	if position, err := s.syncTokens.Decode(from); err == nil {
		return fmt.Sprintf("s%d", position)
	}

	return from
}

// handleJoin обрабатывает POST /rooms/{roomId}/join.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomParam := roomIDParam(r)
	if roomParam == "" {
		if raw, ok := pathParam(r, "roomIdOrAlias"); ok {
			roomParam = raw
		}
	}

	// servers - через кого входить в чужую комнату (ответ /query/directory).
	var servers []string

	// Алиас резолвим в room_id: join по алиасу — основной путь Element.
	switch {
	case strings.HasPrefix(roomParam, "#") && s.isRemote(roomParam):
		// Алиас чужого сервера - через его /query/directory.
		resolved, via, resolveErr := s.remoteAlias(r.Context(), roomParam)
		if resolveErr != nil {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Room alias not found")

			return
		}

		roomParam, servers = resolved, via
	case strings.HasPrefix(roomParam, "#"):
		// Без федерации алиас чужого сервера не резолвится: явный 404, а не
		// падение.
		if server := domainOfIdentifier(roomParam); server != "" && server != s.cfg.Matrix.ServerName {
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Room alias not found")

			return
		}

		resolved, resolveErr := s.rooms.ResolveAlias(r.Context(), roomParam)
		if resolveErr != nil {
			s.writeRoomError(w, resolveErr)

			return
		}

		roomParam = resolved
	}

	// Заблокированная комната (admin API): join запрещён.
	if s.rooms != nil {
		if blocked, err := s.rooms.RoomBlocked(r.Context(), roomParam); err == nil && blocked {
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Room is blocked")

			return
		}
	}

	// Комната на чужом сервере: исходящий join-флоу, локальная комната — как раньше.
	// У комнаты v12 домена в room_id нет: сервер комнаты - создатель, пригласивший
	// и via из запроса.
	remote, servers := s.remoteRoomTarget(r.Context(), roomParam, session.UserID, append(servers, joinVia(r)...))
	if remote {
		if s.remoteJoiner == nil {
			writeMatrixError(w, http.StatusNotImplemented, ErrCodeUnknown, "Federation join is not configured")

			return
		}

		if err := s.remoteJoiner.JoinVia(r.Context(), roomParam, session.UserID, servers); err != nil {
			s.log.WarnContext(r.Context(), "api: remote join failed", "room_id", roomParam, "error", err)

			if errors.Is(err, join.ErrUnsupportedRoomVersion) {
				writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Unsupported room version")

				return
			}

			writeMatrixError(w, http.StatusBadGateway, ErrCodeUnknown, "Remote server rejected join")

			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"room_id": roomParam})

		return
	}

	// Поля тела (reason и произвольные ключи) попадают в content m.room.member,
	// как в Synapse (Complement TestRoomMembers); third_party_signed -
	// параметр запроса, а не content.
	var extra map[string]json.RawMessage
	if !decodeOptionalRoomBody(w, r, &extra) {
		return
	}

	delete(extra, "third_party_signed")

	roomID, err := s.rooms.JoinWithContent(r.Context(), session.UserID, roomParam, extra)
	if err != nil {
		s.writeRoomError(w, err)

		return
	}

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{"room_id": roomID})
}

// inviteRequest — тело POST /rooms/{roomId}/invite.
type inviteRequest struct {
	UserID string `json:"user_id"`
}

// handleInvite обрабатывает POST /rooms/{roomId}/invite.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	var req inviteRequest
	if !decodeRoomBody(w, r, &req) {
		return
	}

	if !isValidUserID(req.UserID) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid user_id")

		return
	}

	roomID := roomIDParam(r)

	var (
		eventID string
		err     error
	)

	// Приглашённый на другом сервере: событие пишется только после доставки
	// отдельным федеративным флоу (PUT /invite), ошибка доставки - ошибка клиенту.
	// Раньше событие писалось до доставки, ошибка уходила в лог, клиент получал
	// 200.
	if dest := inviteDestination(req.UserID); dest != "" && dest != s.cfg.Matrix.ServerName {
		eventID, err = s.inviteRemoteUser(r.Context(), session.UserID, roomID, req.UserID)
	} else {
		eventID, err = s.rooms.Invite(r.Context(), session.UserID, roomID, req.UserID)
	}

	if err != nil {
		var remoteErr *remoteInviteError
		if errors.As(err, &remoteErr) {
			s.log.WarnContext(r.Context(), "api: remote invite failed",
				"room_id", roomID, "user_id", req.UserID, "error", err)
			writeRemoteInviteError(w, remoteErr)

			return
		}

		s.writeRoomError(w, err)

		return
	}

	// Push приглашенному: раньше приглашение push не давало.
	s.dispatchStoredPush(r.Context(), roomID, eventID)

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleLeave обрабатывает POST /rooms/{roomId}/leave.
func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomParam := roomIDParam(r)

	// Чужая комната: S2S leave (make_leave → подпись → send_leave → локальное membership leave).
	if remote, servers := s.remoteRoomTarget(r.Context(), roomParam, session.UserID, nil); remote {
		if s.remoteJoiner == nil {
			writeMatrixError(w, http.StatusNotImplemented, ErrCodeUnknown, "Federation leave is not configured")

			return
		}

		if err := s.leaveRemoteRoom(r.Context(), roomParam, session.UserID, servers); err != nil {
			s.log.WarnContext(r.Context(), "api: remote leave failed", "room_id", roomParam, "error", err)
			s.writeRemoteLeaveError(w, err)

			return
		}

		s.eventsWritten(r.Context())

		writeJSON(w, http.StatusOK, map[string]any{})

		return
	}

	roomID := roomParam

	if _, err := s.rooms.Leave(r.Context(), session.UserID, roomID); err != nil {
		s.writeRoomError(w, err)

		return
	}

	s.eventsWritten(r.Context())

	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleForget — POST /rooms/{roomId}/forget: комната, из которой пользователь
// вышел, перестаёт приходить в /sync. Маршрута не было вовсе.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	roomID := roomIDParam(r)

	if err := s.rooms.ForgetRoom(r.Context(), session.UserID, roomID); err != nil {
		s.writeRoomError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// isValidUserID проверяет формат Matrix user_id (@localpart:server).
func isValidUserID(userID string) bool {
	localpart, serverName, found := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	if !found || !strings.HasPrefix(userID, "@") || serverName == "" {
		return false
	}

	trimmed := strings.TrimSpace(localpart)

	return trimmed != "" && trimmed == localpart
}

// registerRoomRoutes подключает эндпоинты комнат (все требуют токен).
func (s *Server) registerRoomRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/createRoom", s.handleCreateRoom)
		r.Get("/joined_rooms", s.handleJoinedRooms)

		// POST /join/{roomIdOrAlias} — путь из спеки, по которому Element входит
		// по алиасу и принимает приглашение. Раньше маршрута не было вовсе
		// (404), работал только /rooms/{roomId}/join.
		r.Post("/join/{roomIdOrAlias}", s.handleJoin)

		r.Route("/rooms/{roomId}", func(r chi.Router) {
			r.Post("/join", s.handleJoin)
			r.Post("/invite", s.handleInvite)
			r.Post("/leave", s.handleLeave)
			r.Post("/forget", s.handleForget)
			r.Get("/event/{eventId}", s.handleRoomEvent)
			r.Get("/messages", s.handleMessages)
			r.Get("/context/{eventId}", s.handleEventContext)
			// Спека держит hierarchy и relations под /v1; /v3 оставляем для
			// совместимости с уже написанными клиентами.
			r.Get("/hierarchy", s.handleHierarchy)
			r.Post("/upgrade", s.handleUpgrade)
			r.Get("/relations/{eventId}", s.handleRelations)
			r.Get("/relations/{eventId}/{relType}", s.handleRelations)
			r.Get("/relations/{eventId}/{relType}/{eventType}", s.handleRelations)
			r.Get("/members", s.handleRoomMembers)
			r.Get("/joined_members", s.handleJoinedMembers)
			r.Get("/aliases", s.handleRoomAliases)
			// rc_message в Synapse: лимит по пользователю (ключ — user из сессии).
			r.With(s.rateLimit(limitSend, userOrIPKey)).Put("/send/{eventType}/{txnId}", s.handleSend)

			// Управление состоянием и модерация.
			r.Get("/state", s.handleRoomState)
			r.Get("/state/{eventType}/{stateKey}", s.handleRoomStateEvent)
			// Element читает состояние с пустым state_key: /state/m.room.encryption/ —
			// chi матчит только с завершающим слэшем.
			r.Get("/state/{eventType}", s.handleRoomStateEvent)
			r.Get("/state/{eventType}/", s.handleRoomStateEvent)
			r.Put("/state/{eventType}/{stateKey}", s.handleSendState)
			r.Put("/state/{eventType}", s.handleSendStateEmptyKey)
			// Synapse иногда шлёт путь с завершающим слэшем (/state/m.room.power_levels/):
			// chi такой маршрут не матчил, и мы отвечали 404.
			r.Put("/state/{eventType}/", s.handleSendStateEmptyKey)
			r.Post("/kick", s.handleKick)
			r.Post("/ban", s.handleBan)
			r.Post("/unban", s.handleUnban)
			r.Put("/redact/{eventId}/{txnId}", s.handleRedact)
		})
	})
}

// domainOfIdentifier возвращает домен Matrix-идентификатора (@user:domain / !room:domain).
func domainOfIdentifier(id string) string {
	return domainOf(id)
}

// parseCreationExtras разбирает creation_content в набор полей m.room.create.
func parseCreationExtras(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}

	var extras map[string]any
	if err := json.Unmarshal(raw, &extras); err != nil {
		return nil
	}

	return extras
}
