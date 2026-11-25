package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/store"
)

// Проверка входящих membership-событий федерации: invite, send_join,
// send_leave.
//
// Раньше /invite писал в events, room_state и room_memberships любое событие
// (m.room.power_levels с content.membership подменял PL, membership=join/ban
// ставился любому), а send_join/send_leave авторизовались по auth_events,
// выбранным отправителем. Теперь, как в Synapse:
//   - тип m.room.member и membership, соответствующий ручке;
//   - домен sender совпадает с origin запроса (X-Matrix);
//   - invite - только нашему пользователю; join/leave - только за себя
//     (state_key == sender);
//   - event_id - reference hash содержимого; event_id пути и тела обязаны с ним
//     совпадать;
//   - авторизация по текущему состоянию комнаты (а не только по auth_events
//     отправителя); для invite - если мы в комнате.

// membershipError - отказ во входящем membership-событии.
type membershipError struct {
	status  int
	errcode string
	message string
}

func (e *membershipError) Error() string { return e.message }

func (e *membershipError) write(w http.ResponseWriter) {
	writeMatrixError(w, e.status, e.errcode, e.message)
}

func badMembership(message string) *membershipError {
	return &membershipError{status: http.StatusBadRequest, errcode: ErrCodeInvalidParam, message: message}
}

func forbiddenMembership(message string) *membershipError {
	return &membershipError{status: http.StatusForbidden, errcode: ErrCodeForbidden, message: message}
}

// incomingMembership - разобранное входящее membership-событие.
type incomingMembership struct {
	// object - событие, как его подписал отправитель (без event_id из пути).
	object map[string]any
	// pdu - поля события; EventID посчитан по содержимому, PDU - исходный JSON.
	pdu     store.FedPDU
	version int
	origin  string
}

// parseIncomingMembership разбирает и проверяет событие ручки invite/send_join/
// send_leave: room_id пути, тип, membership, домен sender == origin, event_id.
// Подпись и авторизацию проверяет вызывающий.
func (s *Server) parseIncomingMembership(
	r *http.Request, object map[string]any, roomID, membership string, version int,
) (incomingMembership, *membershipError) {
	origin := federationOrigin(r)
	if origin == "" {
		return incomingMembership{}, &membershipError{
			status: http.StatusUnauthorized, errcode: ErrCodeUnauthorized, message: "Unauthorized federation request",
		}
	}

	if version < core.DefaultRoomVersion || version > core.FederationMaxRoomVersion {
		return incomingMembership{}, &membershipError{
			status: http.StatusBadRequest, errcode: ErrCodeUnsupportedRoomVersion, message: "Unsupported room version",
		}
	}

	eventID, err := core.EventID(object, version)
	if err != nil {
		return incomingMembership{}, &membershipError{
			status: http.StatusBadRequest, errcode: ErrCodeBadJSON, message: "Malformed membership event",
		}
	}

	// v12: room_id - хеш create, create не в auth_events.
	if _, err := checkV12PDU(object, eventID, version); err != nil {
		return incomingMembership{}, &membershipError{
			status: http.StatusBadRequest, errcode: ErrCodeBadJSON, message: "Malformed membership event",
		}
	}

	parsed, err := pduFromObject(withEventID(object, eventID))
	if err != nil {
		return incomingMembership{}, &membershipError{
			status: http.StatusBadRequest, errcode: ErrCodeBadJSON, message: "Malformed membership event",
		}
	}

	parsed.PDU = rawPDU(object)

	if parsed.RoomID != roomID {
		return incomingMembership{}, badMembership("Room ID in body does not match that in request path")
	}

	if parsed.Type != "m.room.member" || parsed.StateKey == nil {
		return incomingMembership{}, badMembership("The event was not an m.room.member " + membership + " event")
	}

	if got := membershipOf(parsed.Content); got != membership {
		return incomingMembership{}, badMembership("The event was not an m.room.member " + membership + " event")
	}

	// event_id для v3+ - хеш содержимого: из пути и тела его не берем, но
	// расхождение - признак подмены или ошибки отправителя.
	if fromPath := pathParamValue(r, "eventId"); fromPath != "" && fromPath != eventID {
		return incomingMembership{}, badMembership("Event ID in path does not match event content")
	}

	if fromBody, ok := object["event_id"].(string); ok && fromBody != eventID {
		return incomingMembership{}, badMembership("Event ID in body does not match event content")
	}

	if domainOf(parsed.Sender) != origin {
		return incomingMembership{}, forbiddenMembership("The event was not from the server sending it")
	}

	return incomingMembership{object: object, pdu: parsed, version: version, origin: origin}, nil
}

// withEventID - копия объекта с event_id (для разбора pduFromObject).
func withEventID(object map[string]any, eventID string) map[string]any {
	clone := cloneObject(object)
	clone["event_id"] = eventID

	return clone
}

// membershipOf - content.membership события.
func membershipOf(content json.RawMessage) string {
	var parsed struct {
		Membership string `json:"membership"`
	}

	_ = json.Unmarshal(content, &parsed)

	return parsed.Membership
}

// requireSelfMembership - join/leave только за себя (state_key == sender).
func requireSelfMembership(in incomingMembership) *membershipError {
	if *in.pdu.StateKey != in.pdu.Sender {
		return badMembership("state_key does not match sender")
	}

	return nil
}

// requireResidentRoom - комната нам известна и в ней есть наш участник: иначе
// принимать join/leave и строить шаблоны не по чему (404, как Synapse).
func (s *Server) requireResidentRoom(ctx context.Context, roomID string) *membershipError {
	in, err := s.db.HostInRoom(ctx, roomID, s.cfg.Matrix.ServerName)
	if err != nil {
		return &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to read room"}
	}

	if !in {
		return &membershipError{status: http.StatusNotFound, errcode: ErrCodeNotFound, message: "Not an active room on this server"}
	}

	return nil
}

// currentAuthState - авторизационное состояние комнаты по текущему room_state
// для события sender -> target: create, power_levels, join_rules, членство
// sender, target и авторизующего restricted join, third_party_invite по токену.
func (s *Server) currentAuthState(ctx context.Context, pdu store.FedPDU) (auth.State, error) {
	keys := []stateKeyRef{
		{"m.room.create", ""},
		{"m.room.power_levels", ""},
		{"m.room.join_rules", ""},
		{"m.room.member", pdu.Sender},
	}

	if pdu.StateKey != nil && *pdu.StateKey != pdu.Sender {
		keys = append(keys, stateKeyRef{"m.room.member", *pdu.StateKey})
	}

	var content struct {
		Authorised       string `json:"join_authorised_via_users_server"`
		ThirdPartyInvite struct {
			Signed struct {
				Token string `json:"token"`
			} `json:"signed"`
		} `json:"third_party_invite"`
	}

	_ = json.Unmarshal(pdu.Content, &content)

	if content.Authorised != "" {
		keys = append(keys, stateKeyRef{"m.room.member", content.Authorised})
	}

	if token := content.ThirdPartyInvite.Signed.Token; token != "" {
		keys = append(keys, stateKeyRef{"m.room.third_party_invite", token})
	}

	events, err := s.currentStateEvents(ctx, pdu.RoomID, keys)
	if err != nil {
		return auth.State{}, err
	}

	authEvents := make([]auth.Event, 0, len(events))

	for i := range events {
		authEvents = append(authEvents, auth.Event{
			EventID: events[i].EventID, RoomID: events[i].RoomID, Type: events[i].Type,
			Sender: events[i].Sender, StateKey: events[i].StateKey, Content: events[i].Content,
			Prev: events[i].Prev, Depth: events[i].Depth,
		})
	}

	return auth.BuildState(authEvents)
}

// authorizeByCurrentState проверяет событие auth rules по текущему состоянию
// комнаты (Synapse: soft fail по current state). Отправитель выбирает
// auth_events сам и мог сослаться на устаревшие join_rules или invite до бана.
func (s *Server) authorizeByCurrentState(ctx context.Context, pdu store.FedPDU) error {
	state, err := s.currentAuthState(ctx, pdu)
	if err != nil {
		return err
	}

	return auth.Check(auth.Event{
		EventID: pdu.EventID, RoomID: pdu.RoomID, Type: pdu.Type, Sender: pdu.Sender,
		StateKey: pdu.StateKey, Content: pdu.Content, Prev: pdu.Prev, Depth: pdu.Depth,
	}, state)
}

// errUnauthorizedMembership - событие не прошло auth rules.
var errUnauthorizedMembership = errors.New("api: membership event is not authorized")

// checkIncomingMembershipAuth - auth rules по auth_events события и по
// текущему состоянию комнаты.
func (s *Server) checkIncomingMembershipAuth(ctx context.Context, pdu store.FedPDU) error {
	if err := s.checkAuthRules(ctx, pdu); err != nil {
		return fmt.Errorf("%w: auth_events: %w", errUnauthorizedMembership, err)
	}

	if err := s.authorizeByCurrentState(ctx, pdu); err != nil {
		return fmt.Errorf("%w: current state: %w", errUnauthorizedMembership, err)
	}

	return nil
}

// addOurSignature добавляет к событию подпись нашего сервера, сохраняя подписи
// отправителя (invite, restricted join: спека требует подписи резидента).
func (s *Server) addOurSignature(object map[string]any, version int) (map[string]any, error) {
	if s.federationKey == nil {
		return nil, fmt.Errorf("api: no federation key")
	}

	ours, err := eventjson.SignVersioned(*s.federationKey, s.cfg.Matrix.ServerName, cloneObject(object), version)
	if err != nil {
		return nil, err
	}

	merged := map[string]any{}

	if existing, ok := object["signatures"].(map[string]any); ok {
		for server, sigs := range existing {
			merged[server] = sigs
		}
	}

	if added, ok := ours["signatures"].(map[string]any); ok {
		for server, sigs := range added {
			merged[server] = sigs
		}
	}

	signed := cloneObject(object)
	signed["signatures"] = merged

	return signed, nil
}

// joinAuthoriser - join_authorised_via_users_server из content события.
func joinAuthoriser(content json.RawMessage) string {
	var parsed struct {
		Authorised string `json:"join_authorised_via_users_server"`
	}

	_ = json.Unmarshal(content, &parsed)

	return parsed.Authorised
}

// restrictedAllowRooms - комнаты allow-списка restricted join_rules.
func restrictedAllowRooms(content json.RawMessage) []string {
	var rules struct {
		Allow []struct {
			Type   string `json:"type"`
			RoomID string `json:"room_id"`
		} `json:"allow"`
	}

	if json.Unmarshal(content, &rules) != nil {
		return nil
	}

	var rooms []string

	for _, item := range rules.Allow {
		if item.Type == "m.room_membership" && item.RoomID != "" {
			rooms = append(rooms, item.RoomID)
		}
	}

	return rooms
}

// isRestrictedJoinRule - join_rule restricted или knock_restricted.
func isRestrictedJoinRule(rule string) bool {
	return rule == "restricted" || rule == "knock_restricted"
}

// errNoAllowedMembership / errNoAuthoriser - отказ restricted join: пользователь
// не состоит ни в одной комнате allow-списка или у нас нет участника, способного
// пригласить.
var (
	errNoAllowedMembership = errors.New("api: user is not in any allowed room")
	errNoAuthoriser        = errors.New("api: no local user can authorise the join")
)

// restrictedJoinAuthoriser выбирает нашего участника, который авторизует вход
// userID в restricted-комнату (make_join в Synapse): пользователь обязан быть
// joined в одной из комнат allow-списка (это знаем только мы), авторизующий -
// joined наш участник с уровнем не ниже invite (старший по уровню, затем по id).
func (s *Server) restrictedJoinAuthoriser(ctx context.Context, roomID, userID string, state auth.State) (string, error) {
	if err := s.requireAllowedMembership(ctx, roomID, userID); err != nil {
		return "", err
	}

	rows, err := s.db.StateEventIDs(ctx, roomID)
	if err != nil {
		return "", err
	}

	var memberIDs []string

	for i := range rows {
		if rows[i].Type == "m.room.member" && domainOf(rows[i].StateKey) == s.cfg.Matrix.ServerName {
			memberIDs = append(memberIDs, rows[i].EventID)
		}
	}

	members, err := s.db.AuthEventsByID(ctx, memberIDs)
	if err != nil {
		return "", err
	}

	required, _ := auth.PowerLevelFor(state, "invite")

	type candidate struct {
		userID string
		level  int
	}

	var candidates []candidate

	for i := range members {
		if members[i].StateKey == nil || membershipOf(members[i].Content) != "join" {
			continue
		}

		if level := auth.Level(state, *members[i].StateKey); level >= required {
			candidates = append(candidates, candidate{userID: *members[i].StateKey, level: level})
		}
	}

	if len(candidates) == 0 {
		return "", errNoAuthoriser
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].level != candidates[j].level {
			return candidates[i].level > candidates[j].level
		}

		return candidates[i].userID < candidates[j].userID
	})

	return candidates[0].userID, nil
}

// requireAllowedMembership - пользователь joined хотя бы в одной комнате
// allow-списка restricted join_rules комнаты.
func (s *Server) requireAllowedMembership(ctx context.Context, roomID, userID string) error {
	rules, err := s.currentStateEvents(ctx, roomID, []stateKeyRef{{"m.room.join_rules", ""}})
	if err != nil {
		return err
	}

	if len(rules) == 0 {
		return errNoAllowedMembership
	}

	allow := restrictedAllowRooms(rules[0].Content)

	memberships, err := store.NewSyncStore(s.db).RoomMembershipsFor(ctx, userID, allow)
	if err != nil {
		return err
	}

	for _, room := range allow {
		if memberships[room] == "join" {
			return nil
		}
	}

	return errNoAllowedMembership
}

// signRestrictedJoin - restricted join, авторизованный нашим участником,
// подписывается нашим ключом (спека: резидент подписывает такой join), но
// только если вступающий все еще в комнате allow-списка. Авторизующий с другого
// сервера - его подпись обязана быть на событии. Возвращает событие для записи
// и ответа (без изменений, если join не restricted).
func (s *Server) signRestrictedJoin(ctx context.Context, in incomingMembership) (map[string]any, *membershipError) {
	authoriser := joinAuthoriser(in.pdu.Content)
	if authoriser == "" {
		return in.object, nil
	}

	server := domainOf(authoriser)

	if server != s.cfg.Matrix.ServerName {
		if err := s.verifyFederationJSON(ctx, in.object, server); err != nil {
			return nil, forbiddenMembership("Join event is not signed by the authorising server")
		}

		return in.object, nil
	}

	if err := s.requireAllowedMembership(ctx, in.pdu.RoomID, in.pdu.Sender); err != nil {
		if errors.Is(err, errNoAllowedMembership) {
			return nil, &membershipError{
				status: http.StatusForbidden, errcode: "M_UNABLE_TO_AUTHORISE_JOIN",
				message: "You do not belong to any of the required rooms to join this room",
			}
		}

		return nil, &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to check join rules"}
	}

	signed, err := s.addOurSignature(in.object, in.version)
	if err != nil {
		return nil, &membershipError{status: http.StatusInternalServerError, errcode: ErrCodeUnknown, message: "Failed to sign join event"}
	}

	return signed, nil
}
