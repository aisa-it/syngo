package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestEventRelations — /relations отдаёт события по m.relates_to с фильтрами.
func TestEventRelations(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	parentID := sendMessage(t, server, alice.AccessToken, roomID, "parent", `{"msgtype":"m.text","body":"родитель"}`)

	reaction := fmt.Sprintf(`{"m.relates_to":{"rel_type":"m.annotation","event_id":%q,"key":"👍"}}`, parentID)
	edit := fmt.Sprintf(`{"m.new_content":{"msgtype":"m.text","body":"правка"},"m.relates_to":{"rel_type":"m.replace","event_id":%q}}`, parentID)
	other := `{"msgtype":"m.text","body":"не связь"}`

	sendMessage(t, server, alice.AccessToken, roomID, "react", reaction)
	sendMessage(t, server, alice.AccessToken, roomID, "edit", edit)
	sendMessage(t, server, alice.AccessToken, roomID, "other", other)

	// Все связи события.
	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/relations/%s", roomID, url.PathEscape(parentID)), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("relations: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp relationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Chunk) != 2 {
		t.Fatalf("связей %d, ожидали 2 (реакция и правка): %s", len(resp.Chunk), rec.Body.String())
	}

	// Страница неполная — next_batch не отдаём, иначе клиент сделает лишний
	// пустой запрос.
	if resp.NextBatch != "" {
		t.Fatalf("next_batch = %q на неполной странице", resp.NextBatch)
	}

	// Фильтр по rel_type.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/relations/%s/m.annotation", roomID, url.PathEscape(parentID)),
		"", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("relations(relType): %d", rec.Code)
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Chunk) != 1 || resp.Chunk[0].Type != "m.room.message" {
		t.Fatalf("фильтр по rel_type вернул %d событий", len(resp.Chunk))
	}
}

// TestEventRelationsRequiresMembership — посторонний получает 403.
func TestEventRelationsRequiresMembership(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")
	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/relations/%s", roomID, url.PathEscape("$x")), "", bob.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, ожидали 403", rec.Code)
	}
}
