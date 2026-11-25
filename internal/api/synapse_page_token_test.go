package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// pageEvent - позиция события комнаты: depth и stream_ordering.
type pageEvent struct {
	ID     string
	Depth  int64
	Stream int64
}

// synapseTopoToken - токен /messages Synapse (StreamToken с topological-ключом
// комнаты), как его отдает Synapse в end/start: "t<depth>-<stream>_<прочие потоки>".
func synapseTopoToken(depth, stream int64) string {
	return fmt.Sprintf("t%d-%d_7_0_0_0_0_0_0_0_0_0_0_0_0", depth, stream)
}

// messagesPage - один запрос /messages: код, errcode, event_id chunk и end.
func messagesPage(
	t *testing.T, server *Server, token, roomID, from, dir string, limit int,
) (code int, errCode string, ids []string, end string) {
	t.Helper()

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=%s&limit=%d", rolePath(roomID), dir, limit)
	if from != "" {
		path += "&from=" + url.QueryEscape(from)
	}

	rec := do(t, server, http.MethodGet, path, "", token)

	var body struct {
		ErrCode string `json:"errcode"`
		Chunk   []struct {
			EventID string `json:"event_id"`
		} `json:"chunk"`
		End string `json:"end"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("разбор /messages: %v (%s)", err, rec.Body.String())
	}

	ids = make([]string, 0, len(body.Chunk))
	for _, ev := range body.Chunk {
		ids = append(ids, ev.EventID)
	}

	return rec.Code, body.ErrCode, ids, body.End
}

// collectMessages листает /messages от from до конца истории в направлении dir.
func collectMessages(t *testing.T, server *Server, token, roomID, from, dir string) []string {
	t.Helper()

	var out []string

	cursor := from

	for range 100 {
		code, errCode, ids, end := messagesPage(t, server, token, roomID, cursor, dir, 3)
		if code != http.StatusOK {
			t.Fatalf("/messages from=%q dir=%s: %d %s", cursor, dir, code, errCode)
		}

		if len(ids) == 0 {
			return out
		}

		out = append(out, ids...)
		cursor = end
	}

	t.Fatal("/messages не дошел до конца истории за 100 страниц")

	return nil
}

// eventIDs - event_id событий по порядку.
func eventIDs(events []pageEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.ID)
	}

	return out
}

// checkSynapseTokenPagination - от токена Synapse каждого события комнаты
// пагинация в обе стороны отдает ровно события по нужную сторону от границы, без
// повторов и пропусков. events - в порядке stream_ordering, depth не убывает.
func checkSynapseTokenPagination(t *testing.T, server *Server, token, roomID string, events []pageEvent) {
	t.Helper()

	reversed := func(in []pageEvent) []string {
		out := eventIDs(in)
		slices.Reverse(out)

		return out
	}

	for k, ev := range events {
		// dir=b: событие токена входит (граница Synapse включительная для b).
		got := collectMessages(t, server, token, roomID, synapseTopoToken(ev.Depth, ev.Stream), "b")
		if want := reversed(events[:k+1]); !slices.Equal(got, want) {
			t.Fatalf("dir=b от t%d-%d: %v, ожидали %v", ev.Depth, ev.Stream, got, want)
		}

		// end Synapse при dir=b указывает на stream-1: событие уже получено.
		got = collectMessages(t, server, token, roomID, synapseTopoToken(ev.Depth, ev.Stream-1), "b")
		if want := reversed(events[:k]); !slices.Equal(got, want) {
			t.Fatalf("dir=b от t%d-%d: %v, ожидали %v", ev.Depth, ev.Stream-1, got, want)
		}

		// dir=f: событие токена не входит.
		got = collectMessages(t, server, token, roomID, synapseTopoToken(ev.Depth, ev.Stream), "f")
		if want := eventIDs(events[k+1:]); !slices.Equal(got, want) {
			t.Fatalf("dir=f от t%d-%d: %v, ожидали %v", ev.Depth, ev.Stream, got, want)
		}
	}
}

// checkSynapseLiveAndInvalid - live-токен Synapse листает от текущего конца
// комнаты, неверный токен - 400 M_INVALID_PARAM.
func checkSynapseLiveAndInvalid(t *testing.T, server *Server, token, roomID string, events []pageEvent) {
	t.Helper()

	for _, live := range []string{"s999999_12_0_0_0_0_0_0_0_0_0_0_0_0", "m999999~1.999990~2.999999_3_0"} {
		_, _, fromEnd, _ := messagesPage(t, server, token, roomID, "", "b", 3)

		code, errCode, ids, _ := messagesPage(t, server, token, roomID, live, "b", 3)
		if code != http.StatusOK || !slices.Equal(ids, fromEnd) {
			t.Fatalf("dir=b от live %q: %d %s %v, ожидали %v", live, code, errCode, ids, fromEnd)
		}

		if got := collectMessages(t, server, token, roomID, live, "b"); len(got) != len(events) {
			t.Fatalf("dir=b от live %q: %d событий, ожидали %d", live, len(got), len(events))
		}

		code, errCode, ids, _ = messagesPage(t, server, token, roomID, live, "f", 3)
		if code != http.StatusOK || len(ids) != 0 {
			t.Fatalf("dir=f от live %q: %d %s %v, ожидали пустую страницу", live, code, errCode, ids)
		}
	}

	for _, bad := range []string{"tx-1", "t10", "garbage", "s5abc", "t1-2_x!"} {
		for _, dir := range []string{"b", "f"} {
			code, errCode, _, _ := messagesPage(t, server, token, roomID, bad, dir, 3)
			if code != http.StatusBadRequest || errCode != ErrCodeInvalidParam {
				t.Fatalf("from=%q dir=%s: %d %s, ожидали 400 M_INVALID_PARAM", bad, dir, code, errCode)
			}
		}
	}
}

// TestMessagesSynapsePageToken - после переезда клиент (Element X) присылает в
// /messages закэшированный токен Synapse: topological "t<depth>-<stream>"
// переводится по depth в нашу позицию той же комнаты, live - от конца комнаты,
// мусор - 400 M_INVALID_PARAM, а не M_BAD_JSON.
func TestMessagesSynapsePageToken(t *testing.T) {
	t.Parallel()

	server, mem := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomFor(t, server, alice.AccessToken)

	for i := range 6 {
		sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("st-%d", i),
			fmt.Sprintf(`{"msgtype":"m.text","body":"msg-%d"}`, i))
	}

	// depth как у импортированной истории: пары событий с равной depth (ветки DAG).
	var events []pageEvent

	for _, ev := range mem.Events() {
		if ev.RoomID == roomID {
			events = append(events, pageEvent{ID: ev.EventID, Stream: ev.StreamOrdering})
		}
	}

	slices.SortFunc(events, func(a, b pageEvent) int { return cmp.Compare(a.Stream, b.Stream) })

	depth := int64(100)
	for i := range events {
		if i%3 != 2 {
			depth++
		}

		events[i].Depth = depth
		mem.SetDepth(events[i].ID, depth)
	}

	checkSynapseTokenPagination(t, server, alice.AccessToken, roomID, events)
	checkSynapseLiveAndInvalid(t, server, alice.AccessToken, roomID, events)
}
