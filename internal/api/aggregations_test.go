package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// fakeAggregations — источник агрегаций без БД: отдает заданные агрегации по
// родителю и запоминает запросы.
type fakeAggregations struct {
	aggs     store.Aggregations
	events   map[string]core.Event
	vis      map[string]store.Visibility
	visErr   error
	aggsErr  error
	gotVis   []map[string]store.Visibility
	gotRooms [][]string
	aggCalls int
}

func (f *fakeAggregations) BundledAggregations(
	_ context.Context, _ string, parents []string, vis map[string]store.Visibility,
) (store.Aggregations, error) {
	f.aggCalls++
	f.gotVis = append(f.gotVis, vis)

	if f.aggsErr != nil {
		return store.Aggregations{}, f.aggsErr
	}

	out := store.Aggregations{
		Threads: map[string]store.ThreadSummary{}, Edits: map[string]string{}, References: map[string][]string{},
		Events: map[string]core.Event{},
	}

	load := func(id string) {
		if ev, ok := f.events[id]; ok {
			out.Events[id] = ev
		}
	}

	for _, id := range parents {
		if v, ok := f.aggs.Threads[id]; ok {
			out.Threads[id] = v
			load(v.LatestEventID)
		}

		if v, ok := f.aggs.Edits[id]; ok {
			out.Edits[id] = v
			load(v)
		}

		if v, ok := f.aggs.References[id]; ok {
			out.References[id] = v
		}
	}

	return out, nil
}

func (f *fakeAggregations) VisibilityCutoffs(
	_ context.Context, _ string, roomIDs []string,
) (map[string]store.Visibility, error) {
	f.gotRooms = append(f.gotRooms, roomIDs)

	return f.vis, f.visErr
}

func aggTestServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func aggEvent(id, sender, content string) core.Event {
	return core.Event{
		EventID: id, RoomID: "!r:test", Type: "m.room.message", Sender: sender, Content: json.RawMessage(content),
	}
}

// relationsOf возвращает unsigned.m.relations события в виде JSON-дерева.
func relationsOf(t *testing.T, view eventView) map[string]any {
	t.Helper()

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Unsigned struct {
			Relations map[string]any `json:"m.relations"`
		} `json:"unsigned"`
	}

	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	return decoded.Unsigned.Relations
}

// TestBundleAggregations — m.thread с последним ответом (и его правкой),
// числом ответов и участием, m.replace - последняя правка, m.reference - id ссылок;
// правки и реакции агрегаций не получают; правка удаленного события не применяется;
// у событий состояния агрегаций нет.
func TestBundleAggregations(t *testing.T) {
	t.Parallel()

	const (
		alice = "@alice:test"
		bob   = "@bob:test"
	)

	root := aggEvent("$root", alice, `{"msgtype":"m.text","body":"корень"}`)
	edited := aggEvent("$edited", bob, `{"msgtype":"m.text","body":"старый"}`)
	redacted := aggEvent("$redacted", bob, `{}`)
	redacted.RedactedBy = "$redaction"
	reply := aggEvent("$reply", bob, `{"m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`)
	editEvent := aggEvent("$edit", bob, `{"m.new_content":{"body":"новый"},"m.relates_to":{"rel_type":"m.replace","event_id":"$edited"}}`)
	replyEdit := aggEvent("$replyedit", bob, `{"m.new_content":{"body":"ответ"},"m.relates_to":{"rel_type":"m.replace","event_id":"$reply"}}`)
	reaction := aggEvent("$reaction", bob, `{"m.relates_to":{"rel_type":"m.annotation","event_id":"$root","key":"+"}}`)
	stateKey := ""
	state := core.Event{EventID: "$state", RoomID: "!r:test", Type: "m.room.name", StateKey: &stateKey, Content: json.RawMessage(`{}`)}

	src := &fakeAggregations{
		aggs: store.Aggregations{
			Threads: map[string]store.ThreadSummary{
				"$root": {Count: 2, LatestEventID: "$reply", Participated: false},
				// Ответ на событие со связью (правку) тредом не считается.
				"$edit": {Count: 1, LatestEventID: "$reply"},
			},
			Edits: map[string]string{
				"$edited": "$edit", "$redacted": "$edit", "$reply": "$replyedit",
				"$reaction": "$edit", "$state": "$edit",
			},
			References: map[string][]string{"$root": {"$ref1", "$ref2"}},
		},
		events: map[string]core.Event{"$reply": reply, "$edit": editEvent, "$replyedit": replyEdit},
		vis:    map[string]store.Visibility{"!r:test": {}},
	}

	views := eventViews([]core.Event{root, edited, redacted, editEvent, reaction, state})
	again := eventViews([]core.Event{root})

	bundleAggregations(context.Background(), core.NewAggregationsService(src, nil), alice, nil, views, again)

	rootRels := relationsOf(t, views[0])

	thread, ok := rootRels["m.thread"].(map[string]any)
	if !ok {
		t.Fatalf("у корня нет m.thread: %v", rootRels)
	}

	if thread["count"] != float64(2) || thread["current_user_participated"] != true {
		t.Fatalf("m.thread: %v (участие - автор корня)", thread)
	}

	latest, _ := thread["latest_event"].(map[string]any)
	if latest["event_id"] != "$reply" {
		t.Fatalf("latest_event: %v", latest)
	}

	latestRels, _ := latest["unsigned"].(map[string]any)["m.relations"].(map[string]any)
	if replace, _ := latestRels["m.replace"].(map[string]any); replace["event_id"] != "$replyedit" {
		t.Fatalf("latest_event без своей правки: %v", latest)
	}

	refs, _ := rootRels["m.reference"].(map[string]any)["chunk"].([]any)
	if len(refs) != 2 || refs[0].(map[string]any)["event_id"] != "$ref1" {
		t.Fatalf("m.reference: %v", rootRels["m.reference"])
	}

	if replace, _ := relationsOf(t, views[1])["m.replace"].(map[string]any); replace["event_id"] != "$edit" ||
		replace["sender"] != bob {
		t.Fatalf("m.replace правленого события: %v", relationsOf(t, views[1]))
	}

	if rels := relationsOf(t, views[2]); rels != nil {
		t.Fatalf("правка удаленного события применена: %v", rels)
	}

	for _, i := range []int{3, 4, 5} {
		if rels := relationsOf(t, views[i]); rels != nil {
			t.Fatalf("%s: агрегации у правки, реакции или состояния: %v", views[i].EventID, rels)
		}
	}

	// Повтор события в другом срезе получает те же агрегации.
	if _, ok := relationsOf(t, again[0])["m.thread"]; !ok {
		t.Fatalf("повтор корня без m.thread")
	}

	// Границы видимости дочитаны один раз для комнаты; запросов агрегаций - два
	// (события и последние ответы тредов).
	if len(src.gotRooms) != 1 || len(src.gotRooms[0]) != 1 || src.aggCalls != 2 {
		t.Fatalf("запросы: видимость %v, агрегации %d", src.gotRooms, src.aggCalls)
	}
}

// TestBundleAggregationsVisibility — переданные границы видимости не перечитываются,
// ошибка чтения видимости закрывает связи комнаты, ошибка агрегаций - выдача без них.
func TestBundleAggregationsVisibility(t *testing.T) {
	t.Parallel()

	root := aggEvent("$root", "@a:test", `{"body":"x"}`)
	known := map[string]store.Visibility{"!r:test": {Before: 5, From: 10}}

	src := &fakeAggregations{}
	bundleAggregations(context.Background(), core.NewAggregationsService(src, nil), "@a:test", known, eventViews([]core.Event{root}))

	if len(src.gotRooms) != 0 || src.gotVis[0]["!r:test"] != known["!r:test"] {
		t.Fatalf("видимость перечитана или потеряна: %v %v", src.gotRooms, src.gotVis)
	}

	src = &fakeAggregations{visErr: errors.New("boom")}
	bundleAggregations(context.Background(), core.NewAggregationsService(src, nil), "@a:test", nil, eventViews([]core.Event{root}))

	if got := src.gotVis[0]["!r:test"]; got.Visible(1) || got.Visible(1<<40) {
		t.Fatalf("при ошибке видимости связи не закрыты: %+v", got)
	}

	src = &fakeAggregations{aggsErr: errors.New("boom"), vis: map[string]store.Visibility{}}
	views := eventViews([]core.Event{root})
	bundleAggregations(context.Background(), core.NewAggregationsService(src, nil), "@a:test", nil, views)

	if rels := relationsOf(t, views[0]); rels != nil {
		t.Fatalf("при ошибке агрегаций: %v", rels)
	}

	// Без БД агрегации не собираются.
	aggTestServer().attachAggregations(context.Background(), "@a:test", nil, views)
}

func TestRelatesTo(t *testing.T) {
	t.Parallel()

	for content, want := range map[string]string{
		``:                    "",
		`{}`:                  "",
		`not json`:            "",
		`{"m.relates_to":{}}`: "",
		`{"m.relates_to":{"rel_type":"m.thread"}}`:             "m.thread",
		`{"m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`: "",
	} {
		if got := relatesTo(json.RawMessage(content)); got != want {
			t.Errorf("relatesTo(%s) = %q, want %q", content, got, want)
		}
	}
}
