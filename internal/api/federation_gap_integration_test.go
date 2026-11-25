//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// specRemote - мок сервера remote.test с семантикой спецификации S2S:
// события хранятся без event_id (room v3+), get_missing_events отдает события
// между earliest_events (то, что у получателя есть) и latest_events, не включая
// их, не глубже min_depth и не больше limit; /event, /state_ids и /state - по
// состоянию до события, которое задает тест.
type specRemote struct {
	mu     sync.Mutex
	events map[string]map[string]any
	// before - состояние до события (id state-событий).
	before map[string][]string
	// requests - число запросов по виду (get_missing_events, event, state_ids, state).
	requests map[string]int
	// earliest - earliest_events последнего get_missing_events.
	earliest []string
	// fail - отвечать 404 на все.
	fail bool
}

func newSpecRemote() *specRemote {
	return &specRemote{
		events: map[string]map[string]any{}, before: map[string][]string{}, requests: map[string]int{},
	}
}

// add запоминает событие отправителя и состояние до него.
func (m *specRemote) add(id string, event map[string]any, before []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.events[id] = event
	m.before[id] = before
}

func (m *specRemote) count(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.requests[kind]
}

func (m *specRemote) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, c := range m.requests {
		n += c
	}

	return n
}

func (m *specRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := r.URL.Path

	var kind string

	switch {
	case strings.Contains(path, "/get_missing_events/"):
		kind = "get_missing_events"
	case strings.Contains(path, "/state_ids/"):
		kind = "state_ids"
	case strings.Contains(path, "/state/"):
		kind = "state"
	case strings.Contains(path, "/event/"):
		kind = "event"
	}

	m.requests[kind]++

	if m.fail || kind == "" {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	switch kind {
	case "get_missing_events":
		m.getMissingEvents(w, r)
	case "event":
		id, _ := url.PathUnescape(path[strings.LastIndex(path, "/")+1:])

		event, ok := m.events[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"origin": "remote.test", "origin_server_ts": 1, "pdus": []any{event}})
	case "state_ids", "state":
		id := r.URL.Query().Get("event_id")

		before, ok := m.before[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		chain := m.authChain(before)

		if kind == "state_ids" {
			_ = json.NewEncoder(w).Encode(map[string]any{"pdu_ids": before, "auth_chain_ids": chain})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"pdus": m.objects(before), "auth_chain": m.objects(chain)})
	}
}

// getMissingEvents - обход назад от prev_events latest до earliest (спецификация).
func (m *specRemote) getMissingEvents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Earliest []string `json:"earliest_events"`
		Latest   []string `json:"latest_events"`
		Limit    int      `json:"limit"`
		MinDepth int64    `json:"min_depth"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	m.earliest = req.Earliest

	stop := map[string]bool{}
	for _, id := range append(append([]string(nil), req.Earliest...), req.Latest...) {
		stop[id] = true
	}

	var queue []string
	for _, id := range req.Latest {
		queue = append(queue, stringSlice(m.events[id]["prev_events"])...)
	}

	seen := map[string]bool{}

	var out []any

	for len(queue) > 0 && len(out) < req.Limit {
		id := queue[0]
		queue = queue[1:]

		event, ok := m.events[id]
		if !ok || stop[id] || seen[id] || int64Value(event["depth"]) < req.MinDepth {
			continue
		}

		seen[id] = true
		out = append(out, event)
		queue = append(queue, stringSlice(event["prev_events"])...)
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"events": out})
}

// authChain - auth chain событий (события мока).
func (m *specRemote) authChain(ids []string) []string {
	seen := map[string]bool{}
	stack := append([]string(nil), ids...)

	var out []string

	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, authID := range stringSlice(m.events[id]["auth_events"]) {
			if !seen[authID] {
				seen[authID] = true

				if _, ok := m.events[authID]; ok {
					out = append(out, authID)
				}

				stack = append(stack, authID)
			}
		}
	}

	sort.Strings(out)

	return out
}

func (m *specRemote) objects(ids []string) []any {
	out := []any{}

	for _, id := range ids {
		if event, ok := m.events[id]; ok {
			out = append(out, event)
		}
	}

	return out
}

// gapEnv - seedRoom, bob вошел (событие принято), мок отправителя подключен.
func gapEnv(t *testing.T, roomID string) (*fedEnv, *specRemote, string) {
	t.Helper()

	env := newFedEnv(t)
	env.seedRoom(t, roomID)

	remote := newSpecRemote()
	server := httptest.NewServer(remote)
	t.Cleanup(server.Close)
	env.setTransport(server.URL)

	join, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)
	remote.add(joinID, join, []string{"$create:test.local", "$pl:test.local", "$invite:test.local", "$alice-join:test.local"})

	if results := env.sendTxn(t, "gap-join", join); len(results[joinID]) != 0 {
		t.Fatalf("join: %v", results)
	}

	return env, remote, joinID
}

// chain - сообщения bob цепочкой от prev (глубина от depth), записанные в мок.
func (e *fedEnv) chain(t *testing.T, remote *specRemote, roomID, prev string, n int, depth int64, auth, before []string) []string {
	t.Helper()

	ids := make([]string, 0, n)

	for i := 0; i < n; i++ {
		msg, id := e.event(t, "@bob:remote.test", roomID, "m.room.message", nil,
			map[string]any{"body": fmt.Sprintf("m%d-%d", depth, i)}, auth, []string{prev}, depth+int64(i))
		remote.add(id, msg, before)
		ids = append(ids, id)
		prev = id
	}

	return ids
}

// timelineEvents - события из ids в таймлайне (не outlier) у получателя.
func (e *fedEnv) timelineEvents(t *testing.T, ids []string) []string {
	t.Helper()

	return e.queryStrings(t, `SELECT event_id FROM events WHERE event_id = ANY($1) AND NOT outlier`, ids)
}

// TestGapFillSpecMock - дыра в DAG догоняется у отправителя с
// семантикой спецификации: earliest_events - наши forward extremities (то, что
// у нас есть), latest - событие с дырой; PDU ответа без event_id (v3+) - id
// считается. Раньше в earliest шли недостающие id, а PDU без event_id
// отвергались, и дыра давала 400/500 на транзакцию; старый TestGapFill проходил
// только потому, что мок игнорировал параметры и отдавал event_id.
func TestGapFillSpecMock(t *testing.T) {
	const roomID = "!gapspec:test.local"

	env, remote, joinID := gapEnv(t, roomID)
	auth := []string{"$create:test.local", "$pl:test.local", joinID}
	before := []string{"$create:test.local", "$pl:test.local", joinID, "$alice-join:test.local"}

	ids := env.chain(t, remote, roomID, joinID, 4, 5, auth, before)
	latest := ids[len(ids)-1]

	results := env.sendTxn(t, "gap-latest", remote.events[latest])
	if len(results[latest]) != 0 {
		t.Fatalf("событие с дырой: %v", results)
	}

	if remote.count("get_missing_events") != 1 {
		t.Fatalf("get_missing_events запрошен %d раз", remote.count("get_missing_events"))
	}

	if !slices.Equal(remote.earliest, []string{joinID}) {
		t.Fatalf("earliest_events %v, ожидали наши extremities [%s]", remote.earliest, joinID)
	}

	if got := env.timelineEvents(t, ids); len(got) != len(ids) {
		t.Fatalf("в таймлайне %d событий из %d", len(got), len(ids))
	}

	extremities := env.queryStrings(t, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID)
	if !slices.Equal(extremities, []string{latest}) {
		t.Fatalf("extremities %v, ожидали [%s]", extremities, latest)
	}

	if remote.count("state_ids") != 0 {
		t.Fatal("дыра закрыта get_missing_events, /state_ids не нужен")
	}
}

// TestGapFillStateIDs - дыра длиннее limit get_missing_events. Самое
// раннее из полученных событий ссылается на prev, которого у нас нет: состояние
// после него берется у отправителя (/state_ids), недостающие события (смена
// displayname bob) догружаются, и событие принимается по этому состоянию (Synapse
// _get_state_ids_after_missing_prev_event). Текущее состояние комнаты - разрешение
// с нашей веткой.
func TestGapFillStateIDs(t *testing.T) {
	const roomID = "!gapstate:test.local"

	env, remote, joinID := gapEnv(t, roomID)
	base := []string{"$create:test.local", "$pl:test.local", "$alice-join:test.local"}

	rename, renameID := env.event(t, "@bob:remote.test", roomID, "m.room.member", strPtr("@bob:remote.test"),
		map[string]any{"membership": "join", "displayname": "Bob"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{joinID}, 5)
	remote.add(renameID, rename, append(append([]string(nil), base...), joinID))

	after := append(append([]string(nil), base...), renameID)
	ids := env.chain(t, remote, roomID, renameID, 14, 6, []string{"$create:test.local", "$pl:test.local", renameID}, after)
	latest := ids[len(ids)-1]

	if results := env.sendTxn(t, "gap-state", remote.events[latest]); len(results[latest]) != 0 {
		t.Fatalf("событие с длинной дырой: %v", results)
	}

	if remote.count("state_ids") == 0 {
		t.Fatal("/state_ids не запрошен")
	}

	// limit 10: из 14 сообщений получено 10 ближайших, они и latest - в таймлайне.
	if got := env.timelineEvents(t, ids); len(got) != 11 {
		t.Fatalf("в таймлайне %d событий, ожидали 11", len(got))
	}

	// Смена displayname догружена outlier-событием, и события ветки приняты по
	// состоянию отправителя (с ней).
	if got := env.queryStrings(t, `SELECT event_id FROM events WHERE event_id = $1 AND outlier`, renameID); len(got) != 1 {
		t.Fatal("prev-событие из /state_ids не записано outlier-событием")
	}

	before, found, err := env.server.db.StateIDsBefore(context.Background(), roomID, latest)
	if err != nil || !found || !slices.Contains(before, renameID) || slices.Contains(before, joinID) {
		t.Fatalf("состояние до %s: %v (found=%v, %v), ожидали смену displayname", latest, before, found, err)
	}

	// Текущее состояние - разрешение нашей ветки (join) с веткой отправителя.
	state := env.queryStrings(t, `
		SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID)
	if len(state) != 1 || (state[0] != renameID && state[0] != joinID) {
		t.Fatalf("member bob в room_state %v", state)
	}

	if remote.total() > maxGapRequests {
		t.Fatalf("запросов к отправителю %d", remote.total())
	}
}

// TestGapFillAuthEventWithoutEventID - недостающее auth-событие
// догружается по /event; PDU в ответе без event_id (v3+), id считается и
// сверяется с запрошенным (раньше сверялось отсутствующее поле event_id, и
// дозагрузка всегда была неуспешной). Догруженный outlier, пришедший затем в
// get_missing_events, становится событием таймлайна.
func TestGapFillAuthEventWithoutEventID(t *testing.T) {
	const roomID = "!gapauth:test.local"

	env := newFedEnv(t)
	env.seedRoom(t, roomID)

	remote := newSpecRemote()
	server := httptest.NewServer(remote)
	t.Cleanup(server.Close)
	env.setTransport(server.URL)

	join, joinID := env.member(t, roomID, "join", []string{"$create:test.local", "$pl:test.local", "$invite:test.local"}, 4)
	remote.add(joinID, join, []string{"$create:test.local", "$pl:test.local", "$invite:test.local", "$alice-join:test.local"})

	msg, msgID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "hi"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{joinID}, 5)
	remote.add(msgID, msg, []string{"$create:test.local", "$pl:test.local", joinID, "$alice-join:test.local"})

	if results := env.sendTxn(t, "gap-auth", msg); len(results[msgID]) != 0 {
		t.Fatalf("сообщение с неизвестным auth-событием: %v", results)
	}

	if remote.count("event") == 0 {
		t.Fatal("/event не запрошен")
	}

	if got := env.timelineEvents(t, []string{joinID, msgID}); len(got) != 2 {
		t.Fatalf("в таймлайне %v, ожидали join и сообщение", got)
	}

	var membership string
	if err := env.server.db.Pool().QueryRow(context.Background(),
		`SELECT membership FROM room_memberships WHERE room_id = $1 AND user_id = '@bob:remote.test'`, roomID).Scan(&membership); err != nil || membership != "join" {
		t.Fatalf("membership bob = %q (%v)", membership, err)
	}
}

// TestGapFillUnresolvable - дыру закрыть нельзя (отправитель не отдает ни
// событий, ни состояния): ошибка по этому PDU, транзакция 200, событие не
// записано. Раньше - 500 на всю транзакцию и бесконечные повторы.
func TestGapFillUnresolvable(t *testing.T) {
	const roomID = "!gapbad:test.local"

	env, remote, joinID := gapEnv(t, roomID)
	remote.fail = true

	orphan, orphanID := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": "x"},
		[]string{"$create:test.local", "$pl:test.local", joinID}, []string{"$nope:remote.test"}, 12)

	results := env.sendTxn(t, "gap-orphan", orphan)
	if msg, _ := results[orphanID]["error"].(string); !strings.Contains(msg, "state history") {
		t.Fatalf("pdus = %v, ожидали ошибку состояния по %s", results, orphanID)
	}

	if env.eventCount(t, orphanID) != 0 {
		t.Fatal("событие-сирота записано")
	}
}

// TestGapFillBudget - запросы к отправителю за транзакцию ограничены общим
// бюджетом: 50 PDU по 20 неизвестных prev каждый без бюджета дали бы больше
// тысячи запросов внутри обработчика /send.
func TestGapFillBudget(t *testing.T) {
	const roomID = "!gapbudget:test.local"

	env, remote, joinID := gapEnv(t, roomID)
	remote.fail = true

	pdus := make([]map[string]any, 0, maxPDUsPerTxn)

	for i := 0; i < maxPDUsPerTxn; i++ {
		prev := make([]string, 0, maxPDUPrevEvents)
		for j := 0; j < maxPDUPrevEvents; j++ {
			prev = append(prev, fmt.Sprintf("$missing-%d-%d:remote.test", i, j))
		}

		pdu, _ := env.event(t, "@bob:remote.test", roomID, "m.room.message", nil, map[string]any{"body": i},
			[]string{"$create:test.local", "$pl:test.local", joinID}, prev, 20)
		pdus = append(pdus, pdu)
	}

	results := env.sendTxn(t, "gap-budget", pdus...)

	if len(results) != maxPDUsPerTxn {
		t.Fatalf("результатов %d", len(results))
	}

	if remote.total() > maxGapRequests {
		t.Fatalf("запросов к отправителю %d, бюджет %d", remote.total(), maxGapRequests)
	}

}
