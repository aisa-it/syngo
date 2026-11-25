package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// TestThreadsList — GET /v1/rooms/{roomId}/threads отдает корни тредов
// по последнему ответу, include=participated - только треды с участием, next_batch
// у полной страницы листает дальше без повторов; посторонний получает 403.
func TestThreadsList(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")
	carol := registerUser(t, server, "carol", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"preset":"public_chat"}`)
	joinRoom(t, server, bob.AccessToken, roomID)

	reply := func(token, txn, root string) {
		sendMessage(t, server, token, roomID, txn,
			fmt.Sprintf(`{"msgtype":"m.text","body":"ответ","m.relates_to":{"rel_type":"m.thread","event_id":%q}}`, root))
	}

	first := sendMessage(t, server, alice.AccessToken, roomID, "r1", `{"msgtype":"m.text","body":"первый"}`)
	second := sendMessage(t, server, bob.AccessToken, roomID, "r2", `{"msgtype":"m.text","body":"второй"}`)
	sendMessage(t, server, alice.AccessToken, roomID, "plain", `{"msgtype":"m.text","body":"без треда"}`)

	reply(bob.AccessToken, "a1", first)
	reply(bob.AccessToken, "b1", second)
	// Свежий ответ поднимает первый тред наверх.
	reply(bob.AccessToken, "a2", first)

	list := func(token, query string) (int, relationsResponse) {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v1/rooms/"+roomID+"/threads"+query, "", token)

		var resp relationsResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return rec.Code, resp
	}

	ids := func(resp relationsResponse) []string {
		out := make([]string, 0, len(resp.Chunk))
		for i := range resp.Chunk {
			out = append(out, resp.Chunk[i].EventID)
		}

		return out
	}

	code, resp := list(alice.AccessToken, "")
	if code != http.StatusOK || fmt.Sprint(ids(resp)) != fmt.Sprint([]string{first, second}) || resp.NextBatch != "" {
		t.Fatalf("threads: %d %v next=%q", code, ids(resp), resp.NextBatch)
	}

	// Пагинация по одному треду.
	code, resp = list(alice.AccessToken, "?limit=1")
	if code != http.StatusOK || fmt.Sprint(ids(resp)) != fmt.Sprint([]string{first}) || resp.NextBatch == "" {
		t.Fatalf("threads limit=1: %d %v next=%q", code, ids(resp), resp.NextBatch)
	}

	code, resp = list(alice.AccessToken, "?limit=1&from="+resp.NextBatch)
	if code != http.StatusOK || fmt.Sprint(ids(resp)) != fmt.Sprint([]string{second}) {
		t.Fatalf("threads вторая страница: %d %v", code, ids(resp))
	}

	// alice - автор только первого корня, ответов не писала.
	code, resp = list(alice.AccessToken, "?include=participated")
	if code != http.StatusOK || fmt.Sprint(ids(resp)) != fmt.Sprint([]string{first}) {
		t.Fatalf("participated alice: %d %v", code, ids(resp))
	}

	// bob отвечал в оба треда.
	if code, resp = list(bob.AccessToken, "?include=participated"); code != http.StatusOK || len(resp.Chunk) != 2 {
		t.Fatalf("participated bob: %d %v", code, ids(resp))
	}

	for _, query := range []string{"?include=mine", "?limit=0", "?limit=x", "?from=x"} {
		if code, _ := list(alice.AccessToken, query); code != http.StatusBadRequest {
			t.Fatalf("%s: %d, ожидали 400", query, code)
		}
	}

	if code, _ := list(carol.AccessToken, ""); code != http.StatusForbidden {
		t.Fatalf("посторонний: %d, ожидали 403", code)
	}
}
