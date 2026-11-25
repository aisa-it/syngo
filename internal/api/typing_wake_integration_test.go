//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTypingStopBetweenSyncsWakesLongPoll - typing=false, пришедший, пока клиент
// был между двумя /sync, отдается следующим long-poll сразу, а не через полный
// таймаут: у эфемерных событий нет позиции в потоке, и раньше /sync видел
// «ничего нового» и ждал (индикатор набора в Element не гас).
func TestTypingStopBetweenSyncsWakesLongPoll(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "twalice", "ADEV")
	bob := registerHTTP(t, server, "twbob", "BDEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`, alice)

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	room := url.PathEscape(created.RoomID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	sync := func(since string, timeout int) (string, string, time.Duration) {
		path := "/_matrix/client/v3/sync?timeout=" + itoaMS(timeout)
		if since != "" {
			path += "&since=" + url.QueryEscape(since)
		}

		start := time.Now()
		rec := do(t, server, http.MethodGet, path, "", bob)

		var resp struct {
			NextBatch string `json:"next_batch"`
		}

		_ = json.Unmarshal(rec.Body.Bytes(), &resp)

		return resp.NextBatch, rec.Body.String(), time.Since(start)
	}

	typing := "/_matrix/client/v3/rooms/" + room + "/typing/" + url.PathEscape("@twalice:test.local")

	next, _, _ := sync("", 0)

	do(t, server, http.MethodPut, typing, `{"typing":true,"timeout":30000}`, alice)

	next, body, _ := sync(next, 10000)
	if !strings.Contains(body, `"user_ids":["@twalice:test.local"]`) {
		t.Fatalf("нет индикатора набора: %s", body)
	}

	// Клиент между запросами: остановка набора приходит до следующего /sync.
	do(t, server, http.MethodPut, typing, `{"typing":false}`, alice)

	_, body, took := sync(next, 10000)
	if took > 3*time.Second || !strings.Contains(body, `{"type":"m.typing","content":{"user_ids":[]}}`) {
		t.Fatalf("остановка набора пришла за %s (ожидали сразу): %s", took, body)
	}
}

func itoaMS(ms int) string {
	b, _ := json.Marshal(ms)

	return string(b)
}
