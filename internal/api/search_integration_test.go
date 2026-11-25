//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestSearch — поиск по body только в комнатах пользователя.
func TestSearch(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "searcher", "searcher-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"search"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for i, body := range []string{"hello world", "matrix search test", "unrelated"} {
		rec = do(http.MethodPut, "/_matrix/client/v3/rooms/"+created.RoomID+"/send/m.room.message/s"+string(rune('1'+i)),
			`{"msgtype":"m.text","body":"`+body+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("send %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	rec = do(http.MethodPost, "/_matrix/client/v3/search",
		`{"search_categories":{"room_events":{"search_term":"matrix"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		SearchCategories struct {
			RoomEvents struct {
				Count   int `json:"count"`
				Results []struct {
					Result struct {
						EventID string `json:"event_id"`
						Content struct {
							Body string `json:"body"`
						} `json:"content"`
					} `json:"result"`
				} `json:"results"`
			} `json:"room_events"`
		} `json:"search_categories"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.SearchCategories.RoomEvents.Count != 1 {
		t.Fatalf("count = %d, ожидали 1: %s", resp.SearchCategories.RoomEvents.Count, rec.Body.String())
	}

	if got := resp.SearchCategories.RoomEvents.Results[0].Result.Content.Body; got != "matrix search test" {
		t.Fatalf("body = %q", got)
	}

	// Поиск без вхождения.
	rec = do(http.MethodPost, "/_matrix/client/v3/search",
		`{"search_categories":{"room_events":{"search_term":"zzz-not-found"}}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("пустой поиск: %d %s", rec.Code, rec.Body.String())
	}

	// Пустой search_term — 400.
	if rec = do(http.MethodPost, "/_matrix/client/v3/search", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("пустой term: %d", rec.Code)
	}
}

// TestSearchPaginationAndContext (Complement TestSearch): count - все
// совпадения, next_batch листает выдачу, order_by=recent - сначала новые,
// event_context отдает соседние события.
func TestSearchPaginationAndContext(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerHTTP(t, srv, "pager", "PDEV")

	rec := do(t, srv, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"pages"}`, token)

	var created struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	for i := range 12 {
		path := "/_matrix/client/v3/rooms/" + created.RoomID + "/send/m.room.message/p" + strconv.Itoa(i)
		if rec := do(t, srv, http.MethodPut, path, `{"msgtype":"m.text","body":"Message number `+strconv.Itoa(i)+`"}`, token); rec.Code != http.StatusOK {
			t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
		}
	}

	type response struct {
		SearchCategories struct {
			RoomEvents struct {
				Count     int    `json:"count"`
				NextBatch string `json:"next_batch"`
				Results   []struct {
					Result struct {
						Content struct {
							Body string `json:"body"`
						} `json:"content"`
					} `json:"result"`
					Context struct {
						EventsBefore []struct {
							Content struct {
								Body string `json:"body"`
							} `json:"content"`
						} `json:"events_before"`
						EventsAfter []json.RawMessage `json:"events_after"`
					} `json:"context"`
				} `json:"results"`
			} `json:"room_events"`
		} `json:"search_categories"`
	}

	search := func(path, body string) response {
		t.Helper()

		rec := do(t, srv, http.MethodPost, path, body, token)

		var resp response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
		}

		return resp
	}

	body := `{"search_categories":{"room_events":{"search_term":"Message","order_by":"recent",
		"filter":{"limit":5},"event_context":{"before_limit":2,"after_limit":1}}}}`

	first := search("/_matrix/client/v3/search", body).SearchCategories.RoomEvents
	if first.Count != 12 || len(first.Results) != 5 || first.NextBatch == "" {
		t.Fatalf("первая страница: count=%d results=%d next=%q", first.Count, len(first.Results), first.NextBatch)
	}

	if got := first.Results[0].Result.Content.Body; got != "Message number 11" {
		t.Fatalf("order_by=recent: первый %q", got)
	}

	if ctx := first.Results[1].Context; len(ctx.EventsBefore) != 2 || ctx.EventsBefore[0].Content.Body != "Message number 9" ||
		len(ctx.EventsAfter) != 1 {
		t.Fatalf("event_context: %+v", ctx)
	}

	// Полная страница несет next_batch, как в Synapse, даже если она последняя.
	full := search("/_matrix/client/v3/search?next_batch=7", body).SearchCategories.RoomEvents
	if len(full.Results) != 5 || full.NextBatch != "12" {
		t.Fatalf("полная последняя страница: results=%d next=%q", len(full.Results), full.NextBatch)
	}

	last := search("/_matrix/client/v3/search?next_batch=10", body).SearchCategories.RoomEvents
	if len(last.Results) != 2 || last.NextBatch != "" || last.Results[1].Result.Content.Body != "Message number 0" {
		t.Fatalf("последняя страница: %+v", last)
	}
}
