package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/state"
	"github.com/aisa-it/syngo/internal/store"
)

// Прием события таймлайна и догон дыр в DAG, как Synapse
// FederationEventHandler.on_receive_pdu:
//   - недостающие auth_events догружаются по /event и пишутся outlier-событиями
//     (проверка по своим auth_events); не догрузились - PDU не принят;
//   - событие, не прошедшее auth по своим auth_events, сохраняется отклоненным;
//   - дыра в prev_events: POST /get_missing_events (earliest - наши forward
//     extremities и известные prev, latest - само событие), полученные события
//     принимаются по порядку depth; осталась дыра - состояние после каждого
//     недостающего prev берется у отправителя (/state_ids, недостающие события -
//     /event или /state) и разрешается с состояниями известных prev
//     (_get_state_ids_after_missing_prev_event);
//   - состояние, extremities и soft-fail - core.ApplyRoomEvents в транзакции
//     записи события.
//
// Все запросы к отправителю за одну транзакцию /send - из общего бюджета
// maxGapRequests: раньше рекурсивная дозагрузка auth могла сделать до 32^8
// запросов /event внутри обработчика.

const (
	// maxGapRequests - запросов к отправителю на одну транзакцию /send.
	maxGapRequests = 100
	// missingEventsLimit - limit запроса /get_missing_events (как Synapse).
	missingEventsLimit = 10
)

// errNoGapBudget - бюджет запросов транзакции исчерпан.
var errNoGapBudget = errors.New("api: federation request budget exhausted")

// gapFetcher - прием событий одной транзакции /send от origin с общим бюджетом
// запросов к нему.
type gapFetcher struct {
	s      *Server
	origin string
	budget int
}

func (s *Server) newGapFetcher(origin string) *gapFetcher {
	return &gapFetcher{s: s, origin: origin, budget: maxGapRequests}
}

// request - запрос к отправителю из бюджета транзакции; ответ не 2xx - ошибка.
func (g *gapFetcher) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if g.s.federationTransport == nil {
		return nil, errors.New("api: federation transport is not configured")
	}

	if g.budget <= 0 {
		return nil, errNoGapBudget
	}

	g.budget--

	response, status, err := g.s.federationTransport.Send(ctx, g.origin, method, path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}

	if status/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d", method, path, status)
	}

	return response, nil
}

// receiveEvent принимает событие таймлайна. pulled - событие из ответа
// get_missing_events: дыру в его prev_events закрывает только /state_ids (без
// повторного get_missing_events, как Synapse _process_pulled_event). Ошибка
// pduError - событие не принято; иная - сбой хранилища.
func (g *gapFetcher) receiveEvent(ctx context.Context, pdu store.FedPDU, pulled bool) error {
	known, err := g.s.eventInTimeline(ctx, pdu.EventID)
	if err != nil || known {
		return err
	}

	if err := g.ensureAuthEvents(ctx, pdu); err != nil {
		return err
	}

	opts := core.ApplyOptions{SoftFail: true}

	rejected, err := g.s.rejectedByAuthEvents(ctx, pdu)
	if err != nil {
		return err
	}

	if rejected {
		opts.Rejected = map[string]struct{}{pdu.EventID: {}}
	} else {
		opts.PrevStates, err = g.resolvePrevs(ctx, pdu, pulled)
		if err != nil {
			return err
		}
	}

	events := []state.Event{stateEventFromPDU(pdu)}

	result, err := g.s.db.ApplyFederationEvents(ctx, g.origin, pdu.RoomID, []store.FedPDU{pdu},
		func(tx core.RoomStateTx) (core.RoomStateResult, error) {
			return g.s.stateResolution.Apply(ctx, tx, pdu.RoomID, events, opts)
		})
	if err != nil {
		return err
	}

	if len(result.Rejected) > 0 {
		g.s.log.InfoContext(ctx, "api: federation event rejected", "event_id", pdu.EventID, "origin", g.origin)
	}

	if len(result.SoftFailed) > 0 {
		g.s.log.InfoContext(ctx, "api: federation event soft-failed", "event_id", pdu.EventID, "origin", g.origin)
	}

	return nil
}

// eventInTimeline - событие уже принято: событие таймлайна, отклоненное или с
// записанным состоянием (outlier без состояния принимается заново).
func (s *Server) eventInTimeline(ctx context.Context, eventID string) (bool, error) {
	known, err := s.knownTimelineEvents(ctx, []string{eventID})

	return len(known) > 0, err
}

// knownTimelineEvents - события из ids, известные как события DAG комнаты (не
// outlier, отклоненные или с записанным состоянием): Synapse have_events_in_timeline.
func (s *Server) knownTimelineEvents(ctx context.Context, ids []string) (map[string]struct{}, error) {
	known := map[string]struct{}{}
	if len(ids) == 0 {
		return known, nil
	}

	rows, err := s.db.Pool().Query(ctx, `
		SELECT e.event_id FROM events e
		WHERE e.event_id = ANY($1)
		  AND (NOT e.outlier OR e.rejected_reason IS NOT NULL
		       OR EXISTS (SELECT 1 FROM event_state_groups g WHERE g.event_id = e.event_id))`, ids)
	if err != nil {
		return nil, fmt.Errorf("api: known events: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		known[id] = struct{}{}
	}

	return known, rows.Err()
}

// existingEvents - события из ids, которые есть в БД (в том числе outlier).
func (s *Server) existingEvents(ctx context.Context, ids []string) (map[string]struct{}, error) {
	present := map[string]struct{}{}
	if len(ids) == 0 {
		return present, nil
	}

	rows, err := s.db.Pool().Query(ctx, `SELECT event_id FROM events WHERE event_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("api: existing events: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		present[id] = struct{}{}
	}

	return present, rows.Err()
}

// rejectedByAuthEvents - событие не проходит auth rules по своим auth_events
// или ссылается на отклоненное auth-событие.
func (s *Server) rejectedByAuthEvents(ctx context.Context, pdu store.FedPDU) (bool, error) {
	var rejectedAuth bool

	if err := s.db.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM events e
			WHERE e.event_id = ANY($1)
			  AND (e.rejected_reason IS NOT NULL
			       OR EXISTS (SELECT 1 FROM event_state_groups g WHERE g.event_id = e.event_id AND g.rejected)))`,
		pdu.Auth).Scan(&rejectedAuth); err != nil {
		return false, fmt.Errorf("api: rejected auth events: %w", err)
	}

	if rejectedAuth {
		return true, nil
	}

	if err := s.checkAuthRules(ctx, pdu); err != nil {
		s.log.InfoContext(ctx, "api: pdu not authorized by its auth events", "event_id", pdu.EventID, "error", err)

		return true, nil
	}

	return false, nil
}

// missing - ids, которых нет в have.
func missing(ids []string, have map[string]struct{}) []string {
	var out []string

	for _, id := range ids {
		if _, ok := have[id]; !ok {
			out = append(out, id)
		}
	}

	return out
}

// ensureAuthEvents догружает недостающие auth_events события.
func (g *gapFetcher) ensureAuthEvents(ctx context.Context, pdu store.FedPDU) error {
	present, err := g.s.existingEvents(ctx, pdu.Auth)
	if err != nil {
		return err
	}

	absent := missing(pdu.Auth, present)
	if len(absent) == 0 {
		return nil
	}

	fetched := g.fetchEvents(ctx, pdu.RoomID, absent)
	if err := g.persistOutliers(ctx, pdu.RoomID, fetched); err != nil {
		return err
	}

	if present, err = g.s.existingEvents(ctx, pdu.Auth); err != nil {
		return err
	}

	if absent = missing(pdu.Auth, present); len(absent) > 0 {
		return pduError{fmt.Sprintf("Cannot fetch auth events (%d missing, first %s)", len(absent), absent[0])}
	}

	return nil
}

// fetchEvents загружает события комнаты по /event; не загрузившиеся и не
// прошедшие проверку пропускаются.
func (g *gapFetcher) fetchEvents(ctx context.Context, roomID string, ids []string) []store.FedPDU {
	out := make([]store.FedPDU, 0, len(ids))

	for _, id := range ids {
		body, err := g.request(ctx, http.MethodGet, "/_matrix/federation/v1/event/"+url.PathEscape(id), nil)
		if err != nil {
			g.s.log.WarnContext(ctx, "api: fetch event failed", "origin", g.origin, "event_id", id, "error", err)

			continue
		}

		// Ответ /event: {origin, origin_server_ts, pdus: [PDU]}; в PDU v3+ поля
		// event_id нет - id считается по содержимому и обязан совпасть с
		// запрошенным (раньше сверялось отсутствующее поле event_id).
		var reply struct {
			PDUs []json.RawMessage `json:"pdus"`
		}

		if err := json.Unmarshal(body, &reply); err != nil || len(reply.PDUs) != 1 {
			continue
		}

		pdu, err := g.prepareFetched(ctx, roomID, reply.PDUs[0])
		if err != nil || pdu.EventID != id {
			g.s.log.WarnContext(ctx, "api: fetched event rejected", "origin", g.origin, "event_id", id, "error", err)

			continue
		}

		out = append(out, pdu)
	}

	return out
}

// prepareFetched проверяет событие, полученное от отправителя (подпись, hash,
// event_id), и что оно из комнаты roomID.
func (g *gapFetcher) prepareFetched(ctx context.Context, roomID string, raw json.RawMessage) (store.FedPDU, error) {
	object, err := decodePDUObject(raw)
	if err != nil {
		return store.FedPDU{}, err
	}

	if got, _ := object["room_id"].(string); got != roomID {
		return store.FedPDU{}, fmt.Errorf("api: event of room %q, expected %s", got, roomID)
	}

	version := g.s.roomVersionOf(ctx, roomID)
	if version == 0 {
		version = core.DefaultRoomVersion
	}

	return g.s.prepareRemotePDU(ctx, raw, object, version)
}

// persistOutliers пишет события вне таймлайна (Synapse _auth_and_persist_outliers):
// недостающие auth-события догружаются (в пределах бюджета), события пишутся по
// порядку auth-цепочки, каждое проверяется по своим auth_events (не прошло -
// пишется отклоненным). Событие, чьи auth-события так и не нашлись, не пишется.
func (g *gapFetcher) persistOutliers(ctx context.Context, roomID string, pdus []store.FedPDU) error {
	batch := map[string]store.FedPDU{}
	for i := range pdus {
		batch[pdus[i].EventID] = pdus[i]
	}

	// Недостающие auth-события пачки - по /event, пока они находятся и есть бюджет.
	for round := 0; round < maxAuthFetchRounds; round++ {
		var refs []string

		for id := range batch {
			refs = append(refs, batch[id].Auth...)
		}

		present, err := g.s.existingEvents(ctx, refs)
		if err != nil {
			return err
		}

		var absent []string

		seen := map[string]struct{}{}

		for _, id := range refs {
			if _, ok := batch[id]; ok {
				continue
			}

			if _, ok := present[id]; ok {
				continue
			}

			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				absent = append(absent, id)
			}
		}

		if len(absent) == 0 || g.budget <= 0 {
			break
		}

		sort.Strings(absent)

		fetched := g.fetchEvents(ctx, roomID, absent)
		for i := range fetched {
			batch[fetched[i].EventID] = fetched[i]
		}
	}

	ordered := authOrder(batch)

	for i := range ordered {
		pdu := ordered[i]

		present, err := g.s.existingEvents(ctx, append([]string{pdu.EventID}, pdu.Auth...))
		if err != nil {
			return err
		}

		if _, exists := present[pdu.EventID]; exists {
			continue
		}

		if absent := missing(pdu.Auth, present); len(absent) > 0 {
			g.s.log.WarnContext(ctx, "api: outlier without auth events skipped", "event_id", pdu.EventID, "missing", absent)

			continue
		}

		rejected, err := g.s.rejectedByAuthEvents(ctx, pdu)
		if err != nil {
			return err
		}

		if err := g.s.db.InsertFederationOutliers(ctx, g.origin, []store.FedPDU{pdu},
			map[string]bool{pdu.EventID: rejected}); err != nil {
			return err
		}
	}

	return nil
}

// maxAuthFetchRounds - раундов догрузки auth-цепочки outlier-событий.
const maxAuthFetchRounds = 8

// authOrder - события пачки по порядку auth-цепочки (auth-события раньше), при
// равенстве - по depth и id.
func authOrder(batch map[string]store.FedPDU) []store.FedPDU {
	ids := make([]string, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool {
		if batch[ids[i]].Depth != batch[ids[j]].Depth {
			return batch[ids[i]].Depth < batch[ids[j]].Depth
		}

		return ids[i] < ids[j]
	})

	out := make([]store.FedPDU, 0, len(ids))
	done := map[string]bool{}

	var visit func(id string, path map[string]bool)

	visit = func(id string, path map[string]bool) {
		pdu, ok := batch[id]
		if !ok || done[id] || path[id] {
			return
		}

		path[id] = true

		for _, authID := range pdu.Auth {
			visit(authID, path)
		}

		delete(path, id)

		done[id] = true
		out = append(out, pdu)
	}

	for _, id := range ids {
		visit(id, map[string]bool{})
	}

	return out
}

// resolvePrevs закрывает дыру в prev_events события и возвращает состояние после
// prev, которых у нас так и нет (nil - дыры нет).
func (g *gapFetcher) resolvePrevs(ctx context.Context, pdu store.FedPDU, pulled bool) (map[string]state.StateMap, error) {
	known, err := g.s.knownTimelineEvents(ctx, pdu.Prev)
	if err != nil {
		return nil, err
	}

	if len(missing(pdu.Prev, known)) == 0 {
		return nil, nil
	}

	if g.s.federationTransport == nil {
		return nil, pduError{"prev_events are unknown and federation transport is not configured"}
	}

	if !pulled {
		if err := g.fetchMissingEvents(ctx, pdu, known); err != nil {
			return nil, err
		}

		if known, err = g.s.knownTimelineEvents(ctx, pdu.Prev); err != nil {
			return nil, err
		}
	}

	absent := missing(pdu.Prev, known)
	if len(absent) == 0 {
		return nil, nil
	}

	g.s.log.InfoContext(ctx, "api: event is missing prev_events, requesting state",
		"event_id", pdu.EventID, "missing", absent)

	states := make(map[string]state.StateMap, len(absent))

	for _, prevID := range absent {
		st, err := g.stateAfterRemotePrev(ctx, pdu.RoomID, prevID)

		var pe pduError
		if errors.As(err, &pe) {
			return nil, pduError{"We can't get valid state history: " + pe.msg}
		}

		if err != nil {
			return nil, err
		}

		states[prevID] = st
	}

	return states, nil
}

// fetchMissingEvents - POST /get_missing_events у отправителя: события между
// тем, что у нас есть (forward extremities комнаты и известные prev события), и
// самим событием; полученные принимаются по порядку depth. Сбой запроса не
// ошибка: дальше - /state_ids.
func (g *gapFetcher) fetchMissingEvents(ctx context.Context, pdu store.FedPDU, knownPrevs map[string]struct{}) error {
	earliest, minDepth, err := g.s.gapEarliest(ctx, pdu.RoomID, knownPrevs)
	if err != nil {
		return err
	}

	// Глубже самого раннего нашего события не догоняем (Synapse get_min_depth).
	if pdu.Depth <= minDepth {
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"earliest_events": earliest,
		"latest_events":   []string{pdu.EventID},
		"limit":           missingEventsLimit,
		"min_depth":       minDepth,
	})
	if err != nil {
		return err
	}

	response, err := g.request(ctx, http.MethodPost, "/_matrix/federation/v1/get_missing_events/"+url.PathEscape(pdu.RoomID), body)
	if err != nil {
		g.s.log.WarnContext(ctx, "api: get_missing_events failed", "origin", g.origin, "room_id", pdu.RoomID, "error", err)

		return nil
	}

	var parsed struct {
		Events []json.RawMessage `json:"events"`
	}

	if err := json.Unmarshal(response, &parsed); err != nil {
		g.s.log.WarnContext(ctx, "api: malformed get_missing_events response", "origin", g.origin, "error", err)

		return nil
	}

	g.s.log.InfoContext(ctx, "api: gap fill requested",
		"origin", g.origin, "room_id", pdu.RoomID, "earliest", len(earliest), "fetched", len(parsed.Events))

	pulled := make([]store.FedPDU, 0, len(parsed.Events))

	for _, raw := range parsed.Events {
		event, err := g.prepareFetched(ctx, pdu.RoomID, raw)
		if err != nil {
			g.s.log.WarnContext(ctx, "api: missing event rejected", "origin", g.origin, "error", err)

			continue
		}

		if event.EventID != pdu.EventID {
			pulled = append(pulled, event)
		}
	}

	sort.SliceStable(pulled, func(i, j int) bool { return pulled[i].Depth < pulled[j].Depth })

	for i := range pulled {
		event := &pulled[i]
		err := g.receiveEvent(ctx, *event, true)

		var pe pduError
		if errors.As(err, &pe) {
			g.s.log.WarnContext(ctx, "api: missing event not accepted", "event_id", event.EventID, "error", err)

			continue
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// gapEarliest - earliest_events догона: forward extremities комнаты (без них -
// последние события) и известные prev события; minDepth - наименьшая глубина
// событий комнаты.
func (s *Server) gapEarliest(
	ctx context.Context, roomID string, knownPrevs map[string]struct{},
) (earliest []string, minDepth int64, err error) {
	rows, err := s.db.Pool().Query(ctx, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID)
	if err != nil {
		return nil, 0, fmt.Errorf("api: forward extremities: %w", err)
	}

	set := map[string]struct{}{}

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()

			return nil, 0, err
		}

		set[id] = struct{}{}
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Комната без записанных extremities (до миграции 0044): последние события
	// таймлайна (outlier - не то, что у нас есть в DAG).
	if len(set) == 0 {
		latest, err := s.db.Pool().Query(ctx, `
			SELECT event_id FROM events WHERE room_id = $1 AND NOT outlier
			ORDER BY stream_ordering DESC LIMIT 5`, roomID)
		if err != nil {
			return nil, 0, fmt.Errorf("api: latest events: %w", err)
		}

		for latest.Next() {
			var id string
			if err := latest.Scan(&id); err != nil {
				latest.Close()

				return nil, 0, err
			}

			set[id] = struct{}{}
		}

		latest.Close()

		if err := latest.Err(); err != nil {
			return nil, 0, err
		}
	}

	for id := range knownPrevs {
		set[id] = struct{}{}
	}

	earliest = make([]string, 0, len(set))
	for id := range set {
		earliest = append(earliest, id)
	}

	sort.Strings(earliest)

	if err := s.db.Pool().QueryRow(ctx, `
		SELECT coalesce(min(depth), 0) FROM events WHERE room_id = $1 AND NOT outlier`, roomID).Scan(&minDepth); err != nil {
		return nil, 0, fmt.Errorf("api: room min depth: %w", err)
	}

	return earliest, minDepth, nil
}

// stateAfterRemotePrev - состояние после события prevID, которого у нас нет, по
// данным отправителя (Synapse _get_state_ids_after_missing_prev_event):
// /state_ids дает состояние до prev, недостающие state и auth события
// догружаются (много - одним /state, мало - по /event) и пишутся outlier-событиями,
// сам prev - тоже; к состоянию добавляется prev, если он state-событие и не отклонен.
func (g *gapFetcher) stateAfterRemotePrev(ctx context.Context, roomID, prevID string) (state.StateMap, error) {
	query := "?event_id=" + url.QueryEscape(prevID)

	body, err := g.request(ctx, http.MethodGet, "/_matrix/federation/v1/state_ids/"+url.PathEscape(roomID)+query, nil)
	if err != nil {
		return nil, pduError{err.Error()}
	}

	var ids struct {
		PDUIDs       []string `json:"pdu_ids"`
		AuthChainIDs []string `json:"auth_chain_ids"`
	}

	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, pduError{"malformed state_ids response"}
	}

	wanted := append(append(append([]string(nil), ids.PDUIDs...), ids.AuthChainIDs...), prevID)

	present, err := g.s.existingEvents(ctx, wanted)
	if err != nil {
		return nil, err
	}

	absent := missing(wanted, present)

	if len(absent) > 0 {
		var fetched []store.FedPDU

		// Не хватает многого (или бюджета на /event) - все состояние одним /state.
		if len(absent)*10 >= len(ids.PDUIDs)+len(ids.AuthChainIDs) || len(absent) >= g.budget {
			fetched = g.fetchRoomState(ctx, roomID, query)
		}

		have := map[string]struct{}{}
		for i := range fetched {
			have[fetched[i].EventID] = struct{}{}
		}

		fetched = append(fetched, g.fetchEvents(ctx, roomID, missing(absent, have))...)

		if err := g.persistOutliers(ctx, roomID, fetched); err != nil {
			return nil, err
		}
	}

	return g.s.stateMapOf(ctx, roomID, prevID, ids.PDUIDs)
}

// fetchRoomState - GET /state: состояние и auth chain до события (проверенные).
func (g *gapFetcher) fetchRoomState(ctx context.Context, roomID, query string) []store.FedPDU {
	body, err := g.request(ctx, http.MethodGet, "/_matrix/federation/v1/state/"+url.PathEscape(roomID)+query, nil)
	if err != nil {
		g.s.log.WarnContext(ctx, "api: fetch room state failed", "origin", g.origin, "room_id", roomID, "error", err)

		return nil
	}

	var reply struct {
		PDUs      []json.RawMessage `json:"pdus"`
		AuthChain []json.RawMessage `json:"auth_chain"`
	}

	if err := json.Unmarshal(body, &reply); err != nil {
		return nil
	}

	out := make([]store.FedPDU, 0, len(reply.PDUs)+len(reply.AuthChain))

	for _, raw := range append(reply.PDUs, reply.AuthChain...) {
		if pdu, err := g.prepareFetched(ctx, roomID, raw); err == nil {
			out = append(out, pdu)
		}
	}

	return out
}

// stateMapOf - состояние после prevID: state-события stateIDs (известные, из
// комнаты, не отклоненные) и сам prevID, если он state-событие и не отклонен.
func (s *Server) stateMapOf(ctx context.Context, roomID, prevID string, stateIDs []string) (state.StateMap, error) {
	rows, err := s.db.Pool().Query(ctx, `
		SELECT event_id, type, state_key FROM events
		WHERE event_id = ANY($1) AND room_id = $2 AND state_key IS NOT NULL AND rejected_reason IS NULL`,
		append(append([]string(nil), stateIDs...), prevID), roomID)
	if err != nil {
		return nil, fmt.Errorf("api: remote state events: %w", err)
	}

	defer rows.Close()

	st := state.StateMap{}

	var prevState *state.Key

	for rows.Next() {
		var id, typ, stateKey string
		if err := rows.Scan(&id, &typ, &stateKey); err != nil {
			return nil, err
		}

		key := state.Key{Type: typ, StateKey: stateKey}
		if id == prevID {
			prevState = &key

			continue
		}

		st[key] = id
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	present, err := s.existingEvents(ctx, []string{prevID})
	if err != nil {
		return nil, err
	}

	if len(present) == 0 {
		return nil, pduError{"Unable to get missing prev_event " + prevID}
	}

	if prevState != nil {
		st[*prevState] = prevID
	}

	return st, nil
}
