package api

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// Доступ серверов к комнате по федерации.
//
// Раньше любой сервер с валидной подписью читал состояние и историю любой
// комнаты. Теперь, как в Synapse:
//   - /event, /state, /state_ids, /backfill, /get_missing_events, /event_auth -
//     только серверу с joined-участником в текущем состоянии комнаты, иначе 403;
//   - m.room.server_acl текущего состояния запрещенному серверу - 403 на всех
//     ручках комнаты (включая invite, make_join, send_join, make_leave, send_leave);
//   - события, которые сервер не мог видеть по history_visibility на момент
//     события, отдаются отредактированными (prune_event): DAG остается связным,
//     содержимое не утекает.

// federationOrigin - сервер-отправитель запроса из X-Matrix (подпись уже
// проверил requireFederation).
func federationOrigin(r *http.Request) string {
	header, err := signing.ParseMatrixAuth(r.Header.Get("Authorization"))
	if err != nil {
		return ""
	}

	return header.Origin
}

// requireServerInRoom пропускает запрос сервера с участником комнаты, не
// запрещенного ACL; иначе пишет ошибку и возвращает ok=false.
func (s *Server) requireServerInRoom(w http.ResponseWriter, r *http.Request, roomID string) (origin string, ok bool) {
	origin = federationOrigin(r)
	if origin == "" {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Unauthorized federation request")

		return "", false
	}

	if !s.requireServerACL(w, r, roomID, origin) {
		return "", false
	}

	in, err := s.db.HostInRoom(r.Context(), roomID, origin)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: host in room check failed", "room_id", roomID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to check room membership")

		return "", false
	}

	if !in {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Host not in room.")

		return "", false
	}

	return origin, true
}

// requireServerACL - 403, если m.room.server_acl комнаты запрещает origin.
func (s *Server) requireServerACL(w http.ResponseWriter, r *http.Request, roomID, origin string) bool {
	allowed, err := s.serverACLAllowed(r.Context(), roomID, origin)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: server acl check failed", "room_id", roomID, "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to check server ACL")

		return false
	}

	if !allowed {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Server is banned from room")

		return false
	}

	return true
}

// serverACLAllowed проверяет origin по m.room.server_acl текущего состояния;
// нет ACL (или комнаты) - разрешено.
func (s *Server) serverACLAllowed(ctx context.Context, roomID, origin string) (bool, error) {
	events, err := s.currentStateEvents(ctx, roomID, []stateKeyRef{{"m.room.server_acl", ""}})
	if err != nil {
		return false, err
	}

	if len(events) == 0 {
		return true, nil
	}

	return serverACLAllows(events[0].Content, origin), nil
}

// serverACLAllows - правило m.room.server_acl (спека, «Server Access Control
// Lists»): порт не учитывается; IP-литерал запрещен при allow_ip_literals=false;
// совпадение с deny запрещает, с allow - разрешает, иначе запрещено. Битый
// content ACL не применяется.
func serverACLAllows(content json.RawMessage, origin string) bool {
	var acl struct {
		Allow           []any `json:"allow"`
		Deny            []any `json:"deny"`
		AllowIPLiterals *bool `json:"allow_ip_literals"`
	}

	if err := json.Unmarshal(content, &acl); err != nil {
		return true
	}

	host := strings.ToLower(serverHost(origin))

	if acl.AllowIPLiterals != nil && !*acl.AllowIPLiterals && isIPLiteral(host) {
		return false
	}

	for _, entry := range acl.Deny {
		if glob, ok := entry.(string); ok && aclGlobMatch(glob, host) {
			return false
		}
	}

	for _, entry := range acl.Allow {
		if glob, ok := entry.(string); ok && aclGlobMatch(glob, host) {
			return true
		}
	}

	return false
}

// serverHost - server_name без порта ([v6]:port -> [v6], host:port -> host).
func serverHost(name string) string {
	if strings.HasPrefix(name, "[") {
		if end := strings.Index(name, "]"); end > 0 {
			return name[:end+1]
		}

		return name
	}

	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[:i]
	}

	return name
}

// isIPLiteral - IPv4 или [IPv6].
func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") {
		return true
	}

	return net.ParseIP(host) != nil
}

// aclGlobMatch - glob ACL (* - любая строка, ? - один символ), без учета регистра.
func aclGlobMatch(glob, host string) bool {
	var pattern strings.Builder

	pattern.WriteString("(?i)^")

	for _, r := range glob {
		switch r {
		case '*':
			pattern.WriteString(".*")
		case '?':
			pattern.WriteString(".")
		default:
			pattern.WriteString(regexp.QuoteMeta(string(r)))
		}
	}

	pattern.WriteString("$")

	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return false
	}

	return re.MatchString(host)
}

// stateKeyRef - ключ состояния комнаты.
type stateKeyRef struct{ typ, key string }

// currentStateEvents - события текущего состояния комнаты по ключам (без
// отсутствующих), в порядке ключей.
func (s *Server) currentStateEvents(ctx context.Context, roomID string, keys []stateKeyRef) ([]store.FedPDU, error) {
	rows, err := s.db.StateEventIDs(ctx, roomID)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(keys))

	for _, want := range keys {
		for i := range rows {
			if rows[i].Type == want.typ && rows[i].StateKey == want.key {
				ids = append(ids, rows[i].EventID)
			}
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	pdus, err := s.db.AuthEventsByID(ctx, ids)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]store.FedPDU, len(pdus))
	for i := range pdus {
		byID[pdus[i].EventID] = pdus[i]
	}

	out := make([]store.FedPDU, 0, len(ids))

	for _, id := range ids {
		if pdu, ok := byID[id]; ok {
			out = append(out, pdu)
		}
	}

	return out, nil
}

// visiblePDUs - PDU событий комнаты в порядке ids для сервера origin: событие,
// которое сервер не мог видеть по history_visibility, отдается
// отредактированным (filter_events_for_server, redact=True в Synapse).
func (s *Server) visiblePDUs(ctx context.Context, roomID, origin string, ids []string) ([]json.RawMessage, error) {
	visible, err := s.db.EventsVisibleToServer(ctx, roomID, origin, ids)
	if err != nil {
		return nil, err
	}

	byID, err := s.FederationPDUs(ctx, ids)
	if err != nil {
		return nil, err
	}

	version := s.roomVersionOf(ctx, roomID)
	out := make([]json.RawMessage, 0, len(ids))

	for _, id := range ids {
		raw, ok := byID[id]
		if !ok {
			continue
		}

		delete(byID, id)

		if !visible[id] {
			raw = prunedPDU(raw, version)
		}

		out = append(out, raw)
	}

	return out, nil
}

// prunedTopLevel - top-level поля, которые сохраняет redaction (prune_event);
// legacyPrunedTopLevel - сохраняемые еще и в комнатах до v11.
var (
	prunedTopLevel = map[string]struct{}{
		"event_id": {}, "type": {}, "room_id": {}, "sender": {}, "state_key": {}, "content": {},
		"hashes": {}, "signatures": {}, "depth": {}, "prev_events": {}, "auth_events": {}, "origin_server_ts": {},
	}
	legacyPrunedTopLevel = map[string]struct{}{"origin": {}, "membership": {}, "prev_state": {}}
)

// prunedPDU - отредактированная копия PDU по правилам версии комнаты: подписи
// и hashes остаются, event_id у пира сходится (reference hash считается по той
// же урезанной форме). Битый PDU возвращается как есть.
func prunedPDU(raw json.RawMessage, version int) json.RawMessage {
	if version <= 0 {
		version = core.DefaultRoomVersion
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return raw
	}

	pruned := make(map[string]json.RawMessage, len(object))

	for key, value := range object {
		_, keep := prunedTopLevel[key]
		if !keep && version <= 10 {
			_, keep = legacyPrunedTopLevel[key]
		}

		if keep {
			pruned[key] = value
		}
	}

	var eventType string
	_ = json.Unmarshal(object["type"], &eventType)

	if content, ok := object["content"]; ok {
		pruned["content"] = json.RawMessage(core.RedactContentFor(eventType, content, version))
	}

	out, err := core.MarshalPDU(pruned)
	if err != nil {
		return raw
	}

	return out
}

// dagQueue - очередь обхода DAG: сначала большая глубина, затем больший
// stream_ordering (как backfill в Synapse).
type dagQueue []store.DAGEvent

func (q dagQueue) Len() int { return len(q) }

func (q dagQueue) Less(i, j int) bool {
	if q[i].Depth != q[j].Depth {
		return q[i].Depth > q[j].Depth
	}

	return q[i].StreamOrdering > q[j].StreamOrdering
}

func (q dagQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *dagQueue) Push(x any) {
	if node, ok := x.(store.DAGEvent); ok {
		*q = append(*q, node)
	}
}

func (q *dagQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]

	return item
}

// backfillIDs - события для /backfill: от v по prev_events назад, сами v
// включаются (как в Synapse), не больше limit, по убыванию глубины.
func (s *Server) backfillIDs(ctx context.Context, roomID string, from []string, limit int) ([]string, error) {
	start, err := s.db.DAGEvents(ctx, roomID, from)
	if err != nil {
		return nil, err
	}

	queue := &dagQueue{}
	queued := map[string]struct{}{}

	for _, node := range start {
		heap.Push(queue, node)
		queued[node.EventID] = struct{}{}
	}

	out := make([]string, 0, limit)

	for queue.Len() > 0 && len(out) < limit {
		node, _ := heap.Pop(queue).(store.DAGEvent)
		out = append(out, node.EventID)

		var next []string

		for _, prev := range node.Prev {
			if _, seen := queued[prev]; !seen {
				queued[prev] = struct{}{}
				next = append(next, prev)
			}
		}

		parents, err := s.db.DAGEvents(ctx, roomID, next)
		if err != nil {
			return nil, err
		}

		for _, parent := range parents {
			heap.Push(queue, parent)
		}
	}

	return out, nil
}

// getMissingEventIDs - события для /get_missing_events (спека, Synapse
// _get_missing_events): обход от prev_events событий latest назад по слоям,
// без latest и без earliest (их у пира уже есть) и без их предков через
// earliest; ниже min_depth не спускаемся; не больше limit. Результат - от
// старых к новым.
func (s *Server) getMissingEventIDs(
	ctx context.Context, roomID string, earliest, latest []string, minDepth int64, limit int,
) ([]string, error) {
	seen := make(map[string]struct{}, len(earliest)+len(latest))
	for _, id := range earliest {
		seen[id] = struct{}{}
	}

	front := make([]string, 0, len(latest))

	for _, id := range latest {
		if _, ok := seen[id]; !ok {
			front = append(front, id)
		}
	}

	nodes, err := s.db.DAGEvents(ctx, roomID, front)
	if err != nil {
		return nil, err
	}

	var result []string

	depths := map[string]int64{}

	for len(nodes) > 0 && len(result) < limit {
		var next []string

		for _, id := range front {
			node, ok := nodes[id]
			if !ok {
				continue
			}

			for _, prev := range node.Prev {
				if _, ok := seen[prev]; ok {
					continue
				}

				seen[prev] = struct{}{}
				next = append(next, prev)
			}
		}

		parents, err := s.db.DAGEvents(ctx, roomID, next)
		if err != nil {
			return nil, err
		}

		front = front[:0]

		for _, id := range next {
			parent, ok := parents[id]
			if !ok || parent.Depth < minDepth {
				delete(parents, id)

				continue
			}

			if len(result) < limit {
				result = append(result, id)
				depths[id] = parent.Depth
			}

			front = append(front, id)
		}

		nodes = parents
	}

	// Собирали от новых к старым - разворачиваем в хронологический порядок
	// (по глубине, при равной - в порядке обхода).
	slices.Reverse(result)
	sort.SliceStable(result, func(i, j int) bool { return depths[result[i]] < depths[result[j]] })

	return result, nil
}

// latestEvents - последние события комнаты (prev_events шаблонов make_join/
// make_leave, earliest_events догона дыр) и их наибольшая глубина.
func (s *Server) latestEvents(ctx context.Context, roomID string) (eventIDs []string, maxDepth int64, err error) {
	rows, err := s.db.Pool().Query(ctx, `
		SELECT event_id, depth FROM events WHERE room_id = $1
		ORDER BY stream_ordering DESC LIMIT 5`, roomID)
	if err != nil {
		return nil, 0, err
	}

	defer rows.Close()

	eventIDs = []string{}

	for rows.Next() {
		var (
			id    string
			depth int64
		)

		if err := rows.Scan(&id, &depth); err != nil {
			return nil, 0, err
		}

		eventIDs = append(eventIDs, id)
		maxDepth = max(maxDepth, depth)
	}

	return eventIDs, maxDepth, rows.Err()
}
