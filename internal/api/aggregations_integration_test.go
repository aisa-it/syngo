//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// aggView — событие выдачи с unsigned.m.relations.
type aggView struct {
	EventID  string `json:"event_id"`
	Unsigned struct {
		Relations *struct {
			Thread *struct {
				LatestEvent  *aggView `json:"latest_event"`
				Count        int      `json:"count"`
				Participated bool     `json:"current_user_participated"`
			} `json:"m.thread"`
			Replace *struct {
				EventID string `json:"event_id"`
				Sender  string `json:"sender"`
			} `json:"m.replace"`
			Reference *struct {
				Chunk []struct {
					EventID string `json:"event_id"`
				} `json:"chunk"`
			} `json:"m.reference"`
		} `json:"m.relations"`
	} `json:"unsigned"`
}

func (v *aggView) threadCount() int {
	if v.Unsigned.Relations == nil || v.Unsigned.Relations.Thread == nil {
		return 0
	}

	return v.Unsigned.Relations.Thread.Count
}

func (v *aggView) replaceID() string {
	if v.Unsigned.Relations == nil || v.Unsigned.Relations.Replace == nil {
		return ""
	}

	return v.Unsigned.Relations.Replace.EventID
}

func (v *aggView) latestID() string {
	if v.Unsigned.Relations == nil || v.Unsigned.Relations.Thread == nil ||
		v.Unsigned.Relations.Thread.LatestEvent == nil {
		return ""
	}

	return v.Unsigned.Relations.Thread.LatestEvent.EventID
}

func findAggView(t *testing.T, where string, events []aggView, id string) *aggView {
	t.Helper()

	for i := range events {
		if events[i].EventID == id {
			return &events[i]
		}
	}

	t.Fatalf("%s: нет события %s", where, id)

	return nil
}

func (f *hvFixture) threadReply(tok, roomID, txn, root, body string) string {
	return f.send(tok, roomID, txn,
		`{"msgtype":"m.text","body":"`+body+`","m.relates_to":{"rel_type":"m.thread","event_id":"`+root+`"}}`)
}

func (f *hvFixture) edit(tok, roomID, txn, target, body string) string {
	return f.send(tok, roomID, txn,
		`{"msgtype":"m.text","body":"* `+body+`","m.new_content":{"msgtype":"m.text","body":"`+body+`"},`+
			`"m.relates_to":{"rel_type":"m.replace","event_id":"`+target+`"}}`)
}

func (f *hvFixture) redact(tok, roomID, target, txn string) {
	f.t.Helper()

	rec := do(f.t, f.server, http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/redact/"+
		url.PathEscape(target)+"/"+txn, `{}`, tok)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}
}

// views собирает событие root из всех путей выдачи: /sync, /messages, /context,
// /event, /search, /threads - и проверяет, что агрегации везде одинаковые.
func (f *hvFixture) rootViews(tok, roomID, root, searchTerm string) map[string]*aggView {
	f.t.Helper()

	t := f.t
	roomPath := url.PathEscape(roomID)
	out := map[string]*aggView{}

	getJSON := func(path string, into any) {
		t.Helper()

		code, body := f.get(tok, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}

		if err := json.Unmarshal([]byte(body), into); err != nil {
			t.Fatalf("%s: %v %s", path, err, body)
		}
	}

	var sync struct {
		Rooms struct {
			Join map[string]struct {
				Timeline struct {
					Events []aggView `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}

	getJSON("/_matrix/client/v3/sync?timeout=0&filter="+url.QueryEscape(`{"room":{"timeline":{"limit":50}}}`), &sync)
	out["sync"] = findAggView(t, "sync", sync.Rooms.Join[roomID].Timeline.Events, root)

	var messages struct {
		Chunk []aggView `json:"chunk"`
	}

	getJSON("/_matrix/client/v3/rooms/"+roomPath+"/messages?dir=b&limit=50", &messages)
	out["messages"] = findAggView(t, "messages", messages.Chunk, root)

	var ctx struct {
		Event aggView `json:"event"`
	}

	getJSON("/_matrix/client/v3/rooms/"+roomPath+"/context/"+url.PathEscape(root)+"?limit=1", &ctx)
	out["context"] = &ctx.Event

	var event aggView

	getJSON("/_matrix/client/v3/rooms/"+roomPath+"/event/"+url.PathEscape(root), &event)
	out["event"] = &event

	var search struct {
		SearchCategories struct {
			RoomEvents struct {
				Results []struct {
					Result aggView `json:"result"`
				} `json:"results"`
			} `json:"room_events"`
		} `json:"search_categories"`
	}

	rec := do(t, f.server, http.MethodPost, "/_matrix/client/v3/search",
		`{"search_categories":{"room_events":{"search_term":"`+searchTerm+`"}}}`, tok)
	if err := json.Unmarshal(rec.Body.Bytes(), &search); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("search: %d %v %s", rec.Code, err, rec.Body.String())
	}

	results := make([]aggView, 0)
	for _, r := range search.SearchCategories.RoomEvents.Results {
		results = append(results, r.Result)
	}

	out["search"] = findAggView(t, "search", results, root)

	// Список тредов - последним и без остановки теста: остальные пути проверяются
	// и без него.
	var threads struct {
		Chunk []aggView `json:"chunk"`
	}

	if code, body := f.get(tok, "/_matrix/client/v1/rooms/"+roomPath+"/threads"); code != http.StatusOK ||
		json.Unmarshal([]byte(body), &threads) != nil {
		t.Errorf("threads: %d %s", code, body)

		return out
	}

	out["threads"] = findAggView(t, "threads", threads.Chunk, root)

	return out
}

// TestBundledAggregations — unsigned.m.relations во всех путях выдачи:
// m.thread (число ответов, последний ответ с его правкой, участие), m.replace
// (последняя правка того же отправителя), m.reference; удаленная правка не
// считается, правка удаленного события не применяется, удаленный ответ не
// считается.
func TestBundledAggregations(t *testing.T) {
	f := newHVFixture(t, "agg")
	room := f.createRoom("shared")
	f.join(f.bob, room)

	root := f.text(room, "root", "aggroot")
	f.threadReply(f.bob, room, "t1", root, "первый ответ")
	reply2 := f.threadReply(f.bob, room, "t2", root, "второй ответ")
	replyEdit := f.edit(f.bob, room, "t2e", reply2, "второй ответ исправлен")
	edit1 := f.edit(f.alice, room, "e1", root, "правка 1")
	edit2 := f.edit(f.alice, room, "e2", root, "правка 2")
	// Правка чужого сообщения недействительна (спека, Validity of replacement events).
	f.edit(f.bob, room, "e3", root, "чужая правка")
	ref := f.send(f.bob, room, "ref", `{"msgtype":"m.text","body":"ссылка",`+
		`"m.relates_to":{"rel_type":"m.reference","event_id":"`+root+`"}}`)

	for where, view := range f.rootViews(f.alice, room, root, "aggroot") {
		rels := view.Unsigned.Relations
		if rels == nil || rels.Thread == nil || rels.Replace == nil || rels.Reference == nil {
			t.Fatalf("%s: неполные агрегации: %+v", where, rels)
		}

		if rels.Thread.Count != 2 || view.latestID() != reply2 || !rels.Thread.Participated {
			t.Fatalf("%s: m.thread count=%d latest=%s participated=%v", where,
				rels.Thread.Count, view.latestID(), rels.Thread.Participated)
		}

		if rels.Thread.LatestEvent.replaceID() != replyEdit {
			t.Fatalf("%s: latest_event без своей правки", where)
		}

		// m.replace - событие правки целиком, а не только id.
		if view.replaceID() != edit2 || rels.Replace.Sender == "" {
			t.Fatalf("%s: m.replace = %+v, ожидали %s", where, rels.Replace, edit2)
		}

		if len(rels.Reference.Chunk) != 1 || rels.Reference.Chunk[0].EventID != ref {
			t.Fatalf("%s: m.reference = %+v", where, rels.Reference.Chunk)
		}
	}

	// /relations и /threads/{id}: ответ в треде несет свою правку.
	for _, path := range []string{
		"/_matrix/client/v1/rooms/" + url.PathEscape(room) + "/relations/" + url.PathEscape(root) + "/m.thread",
		"/_matrix/client/v1/rooms/" + url.PathEscape(room) + "/threads/" + url.PathEscape(root),
	} {
		code, body := f.get(f.alice, path)

		var resp struct {
			Chunk []aggView `json:"chunk"`
		}

		if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, code, err)
		}

		if got := findAggView(t, path, resp.Chunk, reply2).replaceID(); got != replyEdit {
			t.Fatalf("%s: m.replace ответа = %q", path, got)
		}
	}

	// Удаленная правка не считается: действует предыдущая; удаленный ответ не
	// считается, последним становится оставшийся.
	f.redact(f.alice, room, edit2, "rd1")
	f.redact(f.bob, room, reply2, "rd2")

	for where, view := range f.rootViews(f.bob, room, root, "aggroot") {
		if view.replaceID() != edit1 || view.threadCount() != 1 || view.latestID() == reply2 {
			t.Fatalf("%s: после удаления правки и ответа: replace=%s count=%d latest=%s",
				where, view.replaceID(), view.threadCount(), view.latestID())
		}
	}

	// Правка удаленного события не применяется; тред остается.
	f.redact(f.alice, room, root, "rd3")

	code, body := f.get(f.alice, "/_matrix/client/v3/rooms/"+url.PathEscape(room)+"/event/"+url.PathEscape(root))

	var redacted aggView
	if err := json.Unmarshal([]byte(body), &redacted); err != nil || code != http.StatusOK {
		t.Fatalf("event: %d %v", code, err)
	}

	if redacted.replaceID() != "" || redacted.threadCount() != 1 {
		t.Fatalf("удаленное событие: replace=%q count=%d, ожидали без правки и с тредом",
			redacted.replaceID(), redacted.threadCount())
	}
}

// TestBundledAggregationsVisibility — связи из скрытого по
// history_visibility промежутка не попадают в агрегации: ни в число ответов, ни в
// последний ответ, ни правкой.
func TestBundledAggregationsVisibility(t *testing.T) {
	f := newHVFixture(t, "aggvis")
	room := f.createRoom("shared")
	root := f.text(room, "root", "visroot")

	f.setVisibility(room, "joined")
	f.threadReply(f.alice, room, "hidden", root, "скрытый ответ")
	hiddenEdit := f.edit(f.alice, room, "hiddenedit", root, "скрытая правка")

	f.join(f.bob, room)

	visible := f.threadReply(f.alice, room, "visible", root, "видимый ответ")

	for where, view := range f.rootViews(f.bob, room, root, "visroot") {
		if view.threadCount() != 1 || view.latestID() != visible || view.replaceID() != "" {
			t.Fatalf("%s: bob видит count=%d latest=%s replace=%s", where,
				view.threadCount(), view.latestID(), view.replaceID())
		}

		if view.Unsigned.Relations.Thread.Participated {
			t.Fatalf("%s: bob не участвовал в треде", where)
		}
	}

	for where, view := range f.rootViews(f.alice, room, root, "visroot") {
		if view.threadCount() != 2 || view.replaceID() != hiddenEdit {
			t.Fatalf("%s: alice видит count=%d replace=%s", where, view.threadCount(), view.replaceID())
		}
	}
}

// TestThreadsListIntegration — /threads через SQL - порядок по
// последнему ответу, include=participated, пагинация без повторов, корни из
// скрытого промежутка и треды из одних удаленных ответов не отдаются.
func TestThreadsListIntegration(t *testing.T) {
	f := newHVFixture(t, "thrl")
	room := f.createRoom("shared")
	f.join(f.bob, room)

	first := f.text(room, "r1", "первый")
	second := f.text(room, "r2", "второй")
	gone := f.text(room, "r3", "только удаленный ответ")

	f.threadReply(f.bob, room, "a1", first, "ответ")
	f.threadReply(f.alice, room, "b1", second, "ответ")
	goneReply := f.threadReply(f.bob, room, "c1", gone, "ответ")
	f.redact(f.bob, room, goneReply, "rd1")
	f.threadReply(f.bob, room, "a2", first, "ответ")

	f.setVisibility(room, "joined")

	hidden := f.text(room, "r4", "скрытый корень")
	f.threadReply(f.alice, room, "d1", hidden, "ответ")

	list := func(tok, query string) ([]string, string) {
		t.Helper()

		code, body := f.get(tok, "/_matrix/client/v1/rooms/"+url.PathEscape(room)+"/threads"+query)

		var resp struct {
			Chunk []aggView `json:"chunk"`
			Next  string    `json:"next_batch"`
		}

		if err := json.Unmarshal([]byte(body), &resp); err != nil || code != http.StatusOK {
			t.Fatalf("threads%s: %d %s", query, code, body)
		}

		ids := make([]string, 0, len(resp.Chunk))
		for i := range resp.Chunk {
			ids = append(ids, resp.Chunk[i].EventID)

			if resp.Chunk[i].threadCount() == 0 {
				t.Fatalf("threads%s: корень %s без m.thread", query, resp.Chunk[i].EventID)
			}
		}

		return ids, resp.Next
	}

	if ids, _ := list(f.alice, ""); fmt.Sprint(ids) != fmt.Sprint([]string{hidden, first, second}) {
		t.Fatalf("alice: %v", ids)
	}

	// bob перевходит после смены видимости на joined: корень, отправленный между
	// сменой и перевходом, ему не виден.
	f.leaveAndRejoin(f.bob, room)

	if ids, _ := list(f.bob, ""); fmt.Sprint(ids) != fmt.Sprint([]string{first, second}) {
		t.Fatalf("bob после перевхода: %v (скрытый корень не отдается)", ids)
	}

	ids, next := list(f.alice, "?limit=2")
	if fmt.Sprint(ids) != fmt.Sprint([]string{hidden, first}) || next == "" {
		t.Fatalf("страница 1: %v next=%q", ids, next)
	}

	if ids, next = list(f.alice, "?limit=2&from="+url.QueryEscape(next)); fmt.Sprint(ids) != fmt.Sprint([]string{second}) || next != "" {
		t.Fatalf("страница 2: %v next=%q", ids, next)
	}

	if ids, _ := list(f.bob, "?include=participated"); fmt.Sprint(ids) != fmt.Sprint([]string{first}) {
		t.Fatalf("participated bob: %v", ids)
	}
}

func (f *hvFixture) leaveAndRejoin(tok, roomID string) {
	f.t.Helper()

	if rec := do(f.t, f.server, http.MethodPost,
		"/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/leave", `{}`, tok); rec.Code != http.StatusOK {
		f.t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	f.join(tok, roomID)
}

// TestSyncAggregationsQueryCountIsConstant — агрегации в /sync читаются
// пачкой, число SQL-запросов не растет с числом тредов и правок в timeline.
func TestSyncAggregationsQueryCountIsConstant(t *testing.T) {
	server := newIntegrationServerWithTracer(t, syncQueryTracer{})
	f := &hvFixture{t: t, server: server, alice: registerHTTP(t, server, "aggqalice", "ADEV")}
	room := f.createRoom("shared")

	seed := func(prefix string, n int) {
		for i := range n {
			root := f.text(room, fmt.Sprintf("%s-root-%d", prefix, i), "x")
			reply := f.threadReply(f.alice, room, fmt.Sprintf("%s-reply-%d", prefix, i), root, "y")
			f.edit(f.alice, room, fmt.Sprintf("%s-edit-%d", prefix, i), reply, "z")
			f.edit(f.alice, room, fmt.Sprintf("%s-redit-%d", prefix, i), root, "w")
		}
	}

	// Timeline обрезан (limited) в обоих замерах: иначе разница в запросах -
	// от состояния комнаты, а не от агрегаций.
	for i := range 25 {
		f.text(room, fmt.Sprintf("pad-%d", i), "pad")
	}

	seed("small", 1)
	small, next := countedSync(t, server, f.alice, "")

	seed("large", 6)
	large, _ := countedSync(t, server, f.alice, "")

	seed("delta", 6)
	delta, _ := countedSync(t, server, f.alice, next)

	t.Logf("SQL на /sync с тредами и правками: 1 тред %d, 7 тредов %d, incremental %d", small, large, delta)

	if small != large || large > syncHTTPQueryBudget || delta > syncHTTPQueryBudget {
		t.Fatalf("SQL-запросов: 1 тред %d, 7 тредов %d, incremental %d (бюджет %d)",
			small, large, delta, syncHTTPQueryBudget)
	}
}
