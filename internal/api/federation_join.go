package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// roomVersionOf отдаёт версию комнаты из БД (DefaultRoomVersion, если не нашли).
func (s *Server) roomVersionOf(ctx context.Context, roomID string) int {
	if s.db == nil {
		return 0
	}

	var raw string

	err := s.db.Pool().QueryRow(ctx, `SELECT room_version FROM rooms WHERE room_id = $1`, roomID).Scan(&raw)
	if err != nil {
		return 0 // комнаты ещё нет в БД: версию смотрит вызывающий (content create)
	}

	version, err := strconv.Atoi(raw)
	if err != nil || version < core.DefaultRoomVersion {
		return 0
	}

	return version
}

// handleMakeJoin — GET /_matrix/federation/v1/make_join/{roomId}/{userId}?ver=...
//
// Отдаёт шаблон m.room.member(join) по текущему состоянию комнаты, который
// чужой сервер подпишет и пришлёт в send_join. Запрос подписан X-Matrix (спека);
// пользователь - с сервера-отправителя; версия комнаты - из поддерживаемых им
// (ver); право войти (join_rules, ban) проверяется сразу.
func (s *Server) handleMakeJoin(w http.ResponseWriter, r *http.Request) {
	s.writeMembershipTemplate(w, r, "join", "make_join reply")
}

// handleSendJoin — PUT /_matrix/federation/v1|v2/send_join/{roomId}/{eventId}.
//
// Принимает подписанное событие member(join) за себя от сервера вступающего,
// проверяет подпись и auth rules (по auth_events и по текущему состоянию),
// пишет в DAG и отдаёт состояние комнаты до join.
func (s *Server) handleSendJoin(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Storage is not configured")

		return
	}

	roomID, ok := pathParam(r, "roomId")
	if !ok {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid room id")

		return
	}

	var object map[string]any
	if !decodeFederationBody(w, r, &object) {
		return
	}

	incoming, ok := s.acceptIncomingJoinLeave(w, r, roomID, object, "join")
	if !ok {
		return
	}

	parsed := incoming.pdu

	// Restricted join, авторизованный нашим участником, подписываем своим ключом.
	signed, rejected := s.signRestrictedJoin(r.Context(), incoming)
	if rejected != nil {
		rejected.write(w)

		return
	}

	parsed.PDU = rawPDU(signed)

	if raw, err := json.Marshal(signed["signatures"]); err == nil {
		parsed.Signatures = raw
	}

	// MSC3706: пир может попросить partial state join (omit_members=true) — тогда
	// state отдаётся без membership-событий, а флаг members_omitted=true. Иначе
	// Synapse-получатель режет ответ сам и остаётся без нужного state.
	omit := s.omitMembersRequested(r, object)

	// Состояние снимаем ДО применения join-события: по спеке state в send_join —
	// это состояние на момент события, а само событие передаётся отдельно в поле
	// event. Если в state уже лежит membership=join вступающего, Synapse видит
	// событие, которое ещё не авторизовано, и отвечает «Join event was rejected».
	// При omit_members членство вступающего остается: оно auth-событие его join
	// (Synapse: _get_event_ids_for_partial_state_join).
	stateIDs, err := s.sendJoinStateIDs(r.Context(), roomID, *parsed.StateKey, omit)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read state")

		return
	}

	// servers_in_room - серверы с участниками до join (спека).
	serversInRoom, err := s.db.JoinedHosts(r.Context(), roomID)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read state")

		return
	}

	state, err := s.federationPDUs(r.Context(), stateIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read state")

		return
	}

	// auth_chain - рекурсивная цепочка всего отдаваемого состояния и самого join:
	// без нее пир не авторизует state и join (например, invite, которое уже не
	// входит в текущее состояние).
	authIDs, err := s.db.AuthChainIDs(r.Context(), append(append([]string{}, stateIDs...), parsed.Auth...))
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	authChain, err := s.federationPDUs(r.Context(), authIDs)
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read auth chain")

		return
	}

	// Запись события + membership + materialized state.
	if err := s.applyMembershipEvent(r.Context(), parsed, incoming.origin, incoming.version); err != nil {
		s.log.ErrorContext(r.Context(), "api: send_join apply failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply join")

		return
	}

	// Состояние на join, forward extremities и room_state.
	if err := s.materializeRoomsState(r.Context(), []store.FedPDU{parsed}); err != nil {
		s.log.ErrorContext(r.Context(), "api: send_join state failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to apply join")

		return
	}

	// Порядок полей важен: Synapse парсит ответ потоково (ijson) корутинами в
	// порядке state → auth_chain → event → members_omitted → servers_in_room.
	// Если state идёт не первым (Go сортирует ключи map по алфавиту), парсер
	// не находит create-событие и отвечает «No create event in state».
	// event_id в event нет: для room v3+ Synapse отвергает явный event_id.
	event := withoutEventID(signed)

	// Пир (Synapse) отвергает событие без поля `hashes` («missing field hashes»).
	// state и auth_chain - исходные PDU с hashes (или собранные с ними, federationPDUs);
	// join-событие - как прислал пир, без hashes достраиваем.
	if _, has := event["hashes"]; !has {
		if hash, hashErr := signing.ContentHash(event); hashErr == nil {
			event["hashes"] = map[string]string{"sha256": hash}
		}
	}

	// Инвариант перед отправкой: каждая ссылка prev_events/auth_events в отдаваемых
	// событиях обязана существовать в БД. Иначе пир получает висячую ссылку и либо
	// не может авторизовать join («Join event was rejected»), либо идёт за событием
	// к нам и получает 404.
	if dangling, dErr := s.danglingRefs(r.Context(), append(append([]json.RawMessage{}, state...), authChain...)); dErr == nil && len(dangling) > 0 {
		s.log.ErrorContext(r.Context(), "api: send_join dangling event refs", "room_id", roomID, "count", len(dangling), "ids", dangling)

		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Room state has dangling references")

		return
	}

	body := sendJoinResponse{
		State:          state,
		AuthChain:      authChain,
		Event:          event,
		MembersOmitted: omit,
		ServersInRoom:  serversInRoom,
	}

	// Федеративный join меняет комнату — будим long-poll.
	s.eventsWritten(r.Context())

	// v2 (room v3+): Synapse сначала зовёт /v2/send_join и на 404 не откатывается на v1
	// для новых версий комнат — без v2 join через федерацию не работает.
	if strings.Contains(r.URL.Path, "/v2/send_join/") {
		s.writeJoinResponse(r.Context(), "send_join reply", w, body)

		return
	}

	// v1 формат: [200, {...}]
	s.writeJoinResponse(r.Context(), "send_join reply (v1)", w, []any{200, body})
}

// acceptIncomingJoinLeave - общие проверки send_join/send_leave:
// событие member(join|leave) за себя от сервера отправителя, сервер не запрещен
// ACL, комната наша, подпись отправителя, auth rules по auth_events и по
// текущему состоянию. При отказе ошибка уже записана.
func (s *Server) acceptIncomingJoinLeave(
	w http.ResponseWriter, r *http.Request, roomID string, object map[string]any, membership string,
) (incomingMembership, bool) {
	ctx := r.Context()

	version := s.roomVersionOf(ctx, roomID)
	if version == 0 {
		version = core.DefaultRoomVersion
	}

	incoming, rejected := s.parseIncomingMembership(r, object, roomID, membership, version)
	if rejected == nil {
		rejected = requireSelfMembership(incoming)
	}

	if rejected != nil {
		s.log.WarnContext(ctx, "api: send_"+membership+" rejected", "room_id", roomID, "reason", rejected.message)
		rejected.write(w)

		return incomingMembership{}, false
	}

	if !s.requireServerACL(w, r, roomID, incoming.origin) {
		return incomingMembership{}, false
	}

	if rejected := s.requireResidentRoom(ctx, roomID); rejected != nil {
		rejected.write(w)

		return incomingMembership{}, false
	}

	// Диагностика расхождения id: если auth_events ссылаются на события, которых
	// у нас нет, значит id при приёме и при отправке посчитаны по-разному.
	// Пишем список — по нему видно, какие ссылки битые.
	s.warnMissingAuthRefs(ctx, roomID, incoming.pdu.EventID, incoming.pdu.Auth)

	// Подпись события ключом сервера отправителя.
	if canonical, cErr := signing.CanonicalJSON(object); cErr == nil {
		sigs, _ := json.Marshal(object["signatures"])
		s.log.DebugContext(ctx, "api: send_"+membership+" canonical",
			"b64", base64.RawStdEncoding.EncodeToString(canonical),
			"sigs", base64.RawStdEncoding.EncodeToString(sigs))
	}

	if err := s.verifyFederationJSON(ctx, object, incoming.origin); err != nil {
		s.log.WarnContext(ctx, "api: send_"+membership+" signature rejected", "origin", incoming.origin, "error", err)
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Invalid event signature")

		return incomingMembership{}, false
	}

	if err := s.checkIncomingMembershipAuth(ctx, incoming.pdu); err != nil {
		s.log.WarnContext(ctx, "api: send_"+membership+" auth rejected", "event_id", incoming.pdu.EventID, "error", err)
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Event is not authorized")

		return incomingMembership{}, false
	}

	return incoming, true
}

// sendJoinStateIDs - текущее состояние комнаты для ответа send_join (по типу и
// ключу); при omit_members - без членства, кроме членства вступающего.
func (s *Server) sendJoinStateIDs(ctx context.Context, roomID, joiner string, omitMembers bool) ([]string, error) {
	rows, err := s.db.StateEventIDs(ctx, roomID)
	if err != nil {
		return nil, err
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Type != rows[j].Type {
			return rows[i].Type < rows[j].Type
		}

		return rows[i].StateKey < rows[j].StateKey
	})

	ids := make([]string, 0, len(rows))

	for i := range rows {
		if omitMembers && rows[i].Type == "m.room.member" && rows[i].StateKey != joiner {
			continue
		}

		ids = append(ids, rows[i].EventID)
	}

	return ids, nil
}

// missingEventIDs возвращает id, которых нет в events.
func (s *Server) missingEventIDs(ctx context.Context, ids []string) []string {
	if s.db == nil || len(ids) == 0 {
		return nil
	}

	rows, err := s.db.Pool().Query(ctx,
		`SELECT id FROM unnest($1::text[]) AS id
		 WHERE NOT EXISTS (SELECT 1 FROM events WHERE event_id = id)`, ids)
	if err != nil {
		return nil
	}

	defer rows.Close()

	var missing []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			missing = append(missing, id)
		}
	}

	return missing
}

// writeJoinResponse пишет ответ join-флоу и дампит его в лог (b64): по нему
// сверяют подпись/hashes с эталоном Synapse.
func (s *Server) writeJoinResponse(ctx context.Context, label string, w http.ResponseWriter, payload any) {
	if raw, err := json.Marshal(payload); err == nil {
		s.log.DebugContext(ctx, "api: "+label, "b64", base64.RawStdEncoding.EncodeToString(raw))
	}

	writeFederationJSON(w, payload)
}

// omitMembersRequested сообщает, просит ли пир partial state join (MSC3706):
// query-параметр omit_members=true или поле omit_members в теле запроса.
func (s *Server) omitMembersRequested(r *http.Request, body map[string]any) bool {
	if s.cfg.Federation.SendFullMemberState {
		return false
	}

	return wantsOmitMembers(r, body)
}

// wantsOmitMembers — сам разбор запроса пира (MSC3706).
func wantsOmitMembers(r *http.Request, body map[string]any) bool {
	if v := r.URL.Query().Get("omit_members"); v == "true" || v == "1" {
		return true
	}

	if v, ok := body["omit_members"].(bool); ok {
		return v
	}

	return false
}

// sendJoinResponse — тело ответа send_join с фиксированным порядком полей
// (потоковый парсер Synapse ожидает именно такой порядок).
type sendJoinResponse struct {
	State          []json.RawMessage `json:"state"`
	AuthChain      []json.RawMessage `json:"auth_chain"`
	Event          map[string]any    `json:"event"`
	MembersOmitted bool              `json:"members_omitted"`
	ServersInRoom  []string          `json:"servers_in_room"`
}

// applyJoinEvent пишет membership-событие, состояние и destination_rooms.
func (s *Server) applyMembershipEvent(ctx context.Context, pdu store.FedPDU, origin string, version int) error {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback(ctx) }()

	prev := pdu.Prev
	if prev == nil {
		prev = []string{}
	}

	authEvents := pdu.Auth
	if authEvents == nil {
		authEvents = []string{}
	}

	// Комнаты может ещё не быть локально (входящий invite в чужую комнату):
	// events.room_id ссылается на rooms, без строки insert падал 500.
	// Версия - из запроса (room_version /v2/invite): без нее комната v12 до join
	// числилась v10, и события проверялись по чужим правилам.
	if version <= 0 {
		version = core.DefaultRoomVersion
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO rooms (room_id, creator, room_version) VALUES ($1,$2,$3)
		ON CONFLICT (room_id) DO NOTHING`, pdu.RoomID, pdu.Sender, strconv.Itoa(version)); err != nil {
		return err
	}

	inserted, err := tx.Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events,
		                    auth_events, depth, origin_server_ts, origin, signatures, hashes, pdu_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (event_id) DO NOTHING`,
		pdu.EventID, pdu.RoomID, pdu.Type, pdu.Sender, pdu.StateKey, pdu.Content,
		prev, authEvents, pdu.Depth, pdu.OriginTS, origin, pdu.Signatures, pdu.Hashes, nullPDU(pdu.PDU))
	if err != nil {
		return err
	}

	var membership string

	var content struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(pdu.Content, &content); err == nil {
		membership = content.Membership
	}

	if pdu.StateKey != nil && membership != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (room_id, user_id) DO UPDATE
			SET event_id = EXCLUDED.event_id, membership = EXCLUDED.membership`,
			pdu.EventID, pdu.RoomID, *pdu.StateKey, pdu.Sender, membership); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO room_state (room_id, type, state_key, event_id)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (room_id, type, state_key) DO UPDATE SET event_id = EXCLUDED.event_id`,
			pdu.RoomID, pdu.Type, *pdu.StateKey, pdu.EventID); err != nil {
			return err
		}
	}

	// join/leave/knock из send_join/send_leave/send_knock ретранслируются
	// остальным серверам комнаты в этой же транзакции; invite из
	// /invite - нет: его рассылает сервер приглашающего. destination_rooms
	// ведет RelayMembershipTx по joined-серверам.
	switch membership {
	case "join", "leave", "knock":
		if inserted.RowsAffected() > 0 {
			if err := s.db.RelayMembershipTx(ctx, tx, pdu.RoomID, pdu.EventID, s.cfg.Matrix.ServerName, origin); err != nil {
				return err
			}
		}
	}

	return tx.Commit(ctx)
}

// checkAuthRules проверяет авторизацию события по его auth_events из БД.
func (s *Server) checkAuthRules(ctx context.Context, pdu store.FedPDU) error {
	// В комнате v12 create в auth_events не входит, и у первого события создателя
	// (его join) auth_events пуст.
	hashRoom := createIDOfRoom(pdu.RoomID) != ""
	if len(pdu.Auth) == 0 && !hashRoom {
		return fmt.Errorf("auth: event without auth_events")
	}

	// create комнаты v12 - неявное auth-событие: его id выводится из room_id.
	authPDUs, err := s.db.AuthEventsByID(ctx, withImplicitCreate(pdu.RoomID, pdu.Type, pdu.Auth))
	if err != nil {
		return err
	}

	authEvents := make([]auth.Event, 0, len(authPDUs))

	var implicitCreate *auth.Event

	for i := range authPDUs {
		ev := auth.Event{
			EventID: authPDUs[i].EventID, RoomID: authPDUs[i].RoomID, Type: authPDUs[i].Type,
			Sender: authPDUs[i].Sender, StateKey: authPDUs[i].StateKey,
			Content: authPDUs[i].Content, Prev: authPDUs[i].Prev, Depth: authPDUs[i].Depth,
		}

		if hashRoom && ev.EventID == createIDOfRoom(pdu.RoomID) && !slices.Contains(pdu.Auth, ev.EventID) {
			implicitCreate = &ev

			continue
		}

		authEvents = append(authEvents, ev)
	}

	// Набор auth_events проверяем до применения правил: дубли, чужая комната,
	// лишние типы и отсутствие create/membership отправителя — отказ.
	event := auth.Event{
		EventID: pdu.EventID, RoomID: pdu.RoomID, Type: pdu.Type, Sender: pdu.Sender,
		StateKey: pdu.StateKey, Content: pdu.Content, Prev: pdu.Prev, Depth: pdu.Depth,
	}
	if err := auth.ValidateAuthEvents(event, authEvents); err != nil {
		return err
	}

	if implicitCreate != nil {
		authEvents = append(authEvents, *implicitCreate)
	} else if hashRoom && pdu.Type != "m.room.create" {
		return fmt.Errorf("%w: m.room.create %s of v12 room is unknown", auth.ErrUnauthorized, createIDOfRoom(pdu.RoomID))
	}

	state, err := auth.BuildState(authEvents)
	if err != nil {
		return err
	}

	// restricted join: членство вступающего в allow-комнатах знает только БД,
	// поэтому подтягиваем его здесь одним запросом.
	if len(state.JoinRulesAllow) > 0 && state.Members[pdu.Sender] != "join" {
		memberships, mErr := store.NewSyncStore(s.db).RoomMembershipsFor(ctx, pdu.Sender, state.JoinRulesAllow)
		if mErr != nil {
			return mErr
		}

		state.AllowMemberships = memberships
	}

	return auth.Check(event, state)
}

// pduFromObject собирает PDU из произвольного JSON-объекта события.
// warnMissingAuthRefs пишет в лог auth_events входящего события, которых нет в БД.
func (s *Server) warnMissingAuthRefs(ctx context.Context, roomID, eventID string, authIDs []string) {
	if s.db == nil || len(authIDs) == 0 {
		return
	}

	rows, err := s.db.Pool().Query(ctx,
		`SELECT id FROM unnest($1::text[]) AS id
		 WHERE NOT EXISTS (SELECT 1 FROM events WHERE event_id = id)`, authIDs)
	if err != nil {
		return
	}

	defer rows.Close()

	var missing []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			missing = append(missing, id)
		}
	}

	if len(missing) > 0 {
		s.log.WarnContext(ctx, "api: auth_events ссылаются на отсутствующие события",
			"room_id", roomID, "event_id", eventID, "missing", missing)
	}
}

// cloneObject — поверхностная копия: нужна, чтобы проверять подпись по исходному
// набору полей, не добавляя в него event_id из пути.
func cloneObject(object map[string]any) map[string]any {
	clone := make(map[string]any, len(object))
	for k, v := range object {
		clone[k] = v
	}

	return clone
}

// withoutEventID копирует событие без поля event_id (v2 send_join).
func withoutEventID(event map[string]any) map[string]any {
	out := make(map[string]any, len(event))

	for key, value := range event {
		if key == "event_id" {
			continue
		}

		out[key] = value
	}

	return out
}

func pduFromObject(object map[string]any) (store.FedPDU, error) {
	getString := func(key string) string {
		value, _ := object[key].(string)

		return value
	}

	pdu := store.FedPDU{
		EventID: getString("event_id"),
		RoomID:  getString("room_id"),
		Type:    getString("type"),
		Sender:  getString("sender"),
	}

	// m.room.create комнаты v12 без room_id: room_id - его event_id с «!».
	if stateKey, ok := object["state_key"].(string); ok && stateKey == "" && pdu.Type == "m.room.create" &&
		object["room_id"] == nil {
		pdu.RoomID = roomIDOfCreate(pdu.EventID)
	}

	if pdu.EventID == "" || pdu.RoomID == "" || pdu.Sender == "" {
		return store.FedPDU{}, errInvalidPDU
	}

	if sigs, ok := object["signatures"]; ok && sigs != nil {
		if raw, err := json.Marshal(sigs); err == nil {
			pdu.Signatures = raw
		}
	}

	if hashes, ok := object["hashes"]; ok && hashes != nil {
		if raw, err := json.Marshal(hashes); err == nil {
			pdu.Hashes = raw
		}
	}

	if content, ok := object["content"]; ok {
		raw, err := json.Marshal(content)
		if err != nil {
			return store.FedPDU{}, err
		}

		pdu.Content = raw
	}

	if stateKey, ok := object["state_key"].(string); ok {
		pdu.StateKey = &stateKey
	}

	pdu.Prev = stringSlice(object["prev_events"])
	pdu.Auth = stringSlice(object["auth_events"])

	// Тело федерации разбирается с UseNumber (decodeFederationBody): числа -
	// json.Number. Раньше принимался только float64, и invite получал depth=0 и
	// origin_server_ts=0.
	pdu.Depth = int64Value(object["depth"])
	pdu.OriginTS = int64Value(object["origin_server_ts"])

	return pdu, nil
}

// int64Value - целое из числа JSON: json.Number (UseNumber) или float64.
func int64Value(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return n
		}
	case float64:
		return int64(typed)
	}

	return 0
}

// nullPDU - пустой исходный PDU как NULL в events.pdu_json.
func nullPDU(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	return string(raw)
}

// stringSlice приводит []any к []string.
// stringSlice разбирает prev_events/auth_events из федеративного PDU.
//
// В room v3+ это пары [event_id, {"sha256": hash}], в v1/v2 — просто строки.
// Раньше пары игнорировались (item.(string) не срабатывал), из-за чего входящий
// invite от Synapse падал 400 «Invalid event JSON» без auth_events/prev_events.
func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))

	for _, item := range raw {
		switch typed := item.(type) {
		case string:
			if typed != "" {
				out = append(out, typed)
			}
		case []any:
			// Пара [event_id, hashes]: берём только идентификатор.
			if len(typed) > 0 {
				if id, ok := typed[0].(string); ok && id != "" {
					out = append(out, id)
				}
			}
		}
	}

	return out
}

// domainOf возвращает server_name идентификатора (@user:server, !room:server,
// $event:server): по грамматике спеки это все после первого двоеточия, и
// server_name сам может содержать порт - @a:example.com:8448 -> example.com:8448.
// Раньше бралось после последнего двоеточия и выходило "8448".
func domainOf(id string) string {
	if _, server, found := strings.Cut(id, ":"); found {
		return server
	}

	return ""
}

// federationKeyLookup возвращает функцию поиска ключей (кэш БД/сеть) в контексте запроса.
func (s *Server) federationKeyLookup(ctx context.Context) func(origin, keyID string) (string, int64, error) {
	return func(origin, keyID string) (string, int64, error) {
		// Своё имя: подпись проверяем своим же ключом, без сетевого запроса
		// (пир может быть не в allow_hosts, да и ходить в себя по HTTP незачем).
		if origin == s.cfg.Matrix.ServerName && s.federationKey != nil {
			// Прежний свой ключ (old_signing_keys) действует до expired_ts.
			if old, ok := s.cfg.Federation.OldSigningKeys[keyID]; ok && keyID != s.federationKey.KeyID {
				return old.Key, old.ExpiredTS, nil
			}

			if keyID != "" && keyID != s.federationKey.KeyID {
				return "", 0, signing.ErrNoSignature
			}

			return s.federationKey.Public(), 0, nil
		}

		if s.keyLookup != nil {
			return s.keyLookup(origin, keyID)
		}

		return s.lookupServerKeyCtx(ctx, origin, keyID)
	}
}

// registerFederationJoinRoutes подключает join-флоу федерации.
func (s *Server) registerFederationJoinRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/make_join/{roomId}/{userId}", s.handleMakeJoin)
	r.Put("/_matrix/federation/v1/send_join/{roomId}/{eventId}", s.handleSendJoin)
	r.Put("/_matrix/federation/v2/send_join/{roomId}/{eventId}", s.handleSendJoin)
}

// danglingRefs возвращает prev_events/auth_events из переданных событий, которых нет
// в БД. Пустой список — состояние консистентно и его можно отдавать пиру.
func (s *Server) danglingRefs(ctx context.Context, events []json.RawMessage) ([]string, error) {
	refs := make(map[string]struct{})

	for _, item := range events {
		header := parsePDUHeader(item)

		for _, list := range []json.RawMessage{header.Prev, header.Auth} {
			for _, id := range refIDs(list) {
				refs[id] = struct{}{}
			}
		}
	}

	if len(refs) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}

	rows, err := s.db.Pool().Query(ctx,
		`SELECT id FROM unnest($1::text[]) AS id
		 WHERE NOT EXISTS (SELECT 1 FROM events WHERE event_id = id) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var missing []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			missing = append(missing, id)
		}
	}

	return missing, rows.Err()
}
