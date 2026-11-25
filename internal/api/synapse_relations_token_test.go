package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// TestRelationsThreadsSynapsePageToken - from Synapse в /relations (StreamToken,
// ключ комнаты topological или live) и в /threads (ThreadsNextBatch
// "<depth>_<stream>") переводится в нашу позицию комнаты, как в /messages;
// неверный токен - 400 M_INVALID_PARAM.
func TestRelationsThreadsSynapsePageToken(t *testing.T) {
	t.Parallel()

	server, mem := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomFor(t, server, alice.AccessToken)

	parent := sendMessage(t, server, alice.AccessToken, roomID, "parent", `{"msgtype":"m.text","body":"родитель"}`)

	reactions := make([]string, 0, 4)

	for i := range 4 {
		reactions = append(reactions, sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("re-%d", i),
			fmt.Sprintf(`{"m.relates_to":{"rel_type":"m.annotation","event_id":%q,"key":"k%d"}}`, parent, i)))
	}

	// depth импортированной истории не совпадает со stream_ordering.
	pos := map[string]pageEvent{}

	for _, ev := range mem.Events() {
		if ev.RoomID != roomID {
			continue
		}

		depth := 1000 + ev.StreamOrdering
		mem.SetDepth(ev.EventID, depth)
		pos[ev.EventID] = pageEvent{ID: ev.EventID, Depth: depth, Stream: ev.StreamOrdering}
	}

	relations := func(query string) (int, string, []string) {
		t.Helper()

		rec := do(t, server, http.MethodGet,
			fmt.Sprintf("/_matrix/client/v1/rooms/%s/relations/%s%s", roomID, url.PathEscape(parent), query), "", alice.AccessToken)

		var body struct {
			ErrCode string `json:"errcode"`
			relationsResponse
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		ids := make([]string, 0, len(body.Chunk))
		for i := range body.Chunk {
			ids = append(ids, body.Chunk[i].EventID)
		}

		return rec.Code, body.ErrCode, ids
	}

	pivot := pos[reactions[1]]
	from := url.QueryEscape(synapseTopoToken(pivot.Depth, pivot.Stream))

	newestFirst := slices.Clone(reactions)
	slices.Reverse(newestFirst)

	checks := []struct {
		query string
		want  []string
	}{
		{"?dir=b&from=" + from, []string{reactions[1], reactions[0]}},
		{"?dir=f&from=" + from, []string{reactions[2], reactions[3]}},
		// live-токен Synapse - от текущего конца комнаты.
		{"?dir=b&from=" + url.QueryEscape("s1_0_0_0_0_0_0_0_0_0_0_0_0_0"), newestFirst},
		{"?dir=f&from=" + url.QueryEscape("m1~1.1_0_0"), []string{}},
		// Наш курсор работает как раньше.
		{fmt.Sprintf("?dir=b&from=s%d", pivot.Stream), []string{reactions[0]}},
	}

	for _, tc := range checks {
		code, errCode, ids := relations(tc.query)
		if code != http.StatusOK || !slices.Equal(ids, tc.want) {
			t.Fatalf("relations%s: %d %s %v, ожидали %v", tc.query, code, errCode, ids, tc.want)
		}
	}

	for _, bad := range []string{"tx-1", "garbage", "5_6", "s1abc"} {
		if code, errCode, _ := relations("?from=" + url.QueryEscape(bad)); code != http.StatusBadRequest || errCode != ErrCodeInvalidParam {
			t.Fatalf("relations from=%q: %d %s, ожидали 400 M_INVALID_PARAM", bad, code, errCode)
		}
	}

	// Треды: корни по последнему ответу, самые свежие первыми.
	type thread struct {
		root  string
		reply pageEvent
	}

	var roots []thread

	for i := range 3 {
		root := sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("root-%d", i), `{"msgtype":"m.text","body":"корень"}`)
		reply := sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("reply-%d", i),
			fmt.Sprintf(`{"msgtype":"m.text","body":"ответ","m.relates_to":{"rel_type":"m.thread","event_id":%q}}`, root))

		for _, ev := range mem.Events() {
			if ev.EventID == reply {
				mem.SetDepth(reply, 5000+ev.StreamOrdering)
				roots = append(roots, thread{root: root, reply: pageEvent{ID: reply, Depth: 5000 + ev.StreamOrdering, Stream: ev.StreamOrdering}})
			}
		}
	}

	threads := func(from string) (int, string, []string) {
		t.Helper()

		rec := do(t, server, http.MethodGet,
			"/_matrix/client/v1/rooms/"+roomID+"/threads?from="+url.QueryEscape(from), "", alice.AccessToken)

		var body struct {
			ErrCode string `json:"errcode"`
			relationsResponse
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		ids := make([]string, 0, len(body.Chunk))
		for i := range body.Chunk {
			ids = append(ids, body.Chunk[i].EventID)
		}

		return rec.Code, body.ErrCode, ids
	}

	// next_batch Synapse после треда 2 (позиция его последнего ответа): дальше треды 1 и 0.
	last := roots[2].reply
	want := []string{roots[1].root, roots[0].root}

	if code, errCode, ids := threads(fmt.Sprintf("%d_%d", last.Depth, last.Stream)); code != http.StatusOK || !slices.Equal(ids, want) {
		t.Fatalf("threads от токена Synapse: %d %s %v, ожидали %v", code, errCode, ids, want)
	}

	if code, errCode, _ := threads("garbage"); code != http.StatusBadRequest || errCode != ErrCodeInvalidParam {
		t.Fatalf("threads from=garbage: %d %s, ожидали 400 M_INVALID_PARAM", code, errCode)
	}
}

// TestMembersAtSynapseToken - /members?at=<since Synapse>: Element Web после
// переезда присылает закэшированный sync-токен Synapse. Раньше он читался как
// курсор по state_key, и список участников приходил пустым; теперь - текущие
// участники, как без at.
func TestMembersAtSynapseToken(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")
	roomID := createRoomFor(t, server, alice.AccessToken)
	joinRoom(t, server, bob.AccessToken, roomID)

	for _, at := range []string{"s123_4_5_6_7_8_9_10_0_0_0_0_0_0", "m123~1.120~2.123_4_0", "t10165-123456_789_0", "s123"} {
		rec := do(t, server, http.MethodGet,
			"/_matrix/client/v3/rooms/"+roomID+"/members?at="+url.QueryEscape(at), "", alice.AccessToken)

		var members struct {
			Chunk []struct {
				StateKey string `json:"state_key"`
			} `json:"chunk"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &members); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("members at=%q: %d %s", at, rec.Code, rec.Body.String())
		}

		got := make([]string, 0, len(members.Chunk))
		for _, m := range members.Chunk {
			got = append(got, m.StateKey)
		}

		slices.Sort(got)

		if want := []string{alice.UserID, bob.UserID}; !slices.Equal(got, want) {
			t.Fatalf("members at=%q: %v, ожидали %v", at, got, want)
		}
	}
}
