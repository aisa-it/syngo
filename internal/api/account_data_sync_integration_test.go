//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestAccountDataWakesLaterSync — account data, записанная между двумя /sync, сдвигает
// позицию: следующий long-poll отвечает сразу, а не через таймаут. Element
// ждет каждый секрет восстановления через /sync, и настройка ключа восстановления
// растягивалась на минуты. Удаление доходит как content {}.
func TestAccountDataWakesLaterSync(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "adsync", "DEV")

	syncOnce := func(since string, timeout int) (map[string]json.RawMessage, string, time.Duration) {
		t.Helper()

		url := "/_matrix/client/v3/sync?timeout=" + strconv.Itoa(timeout)
		if since != "" {
			url += "&since=" + since
		}

		start := time.Now()
		rec := do(t, server, http.MethodGet, url, "", tok)

		var resp struct {
			NextBatch   string `json:"next_batch"`
			AccountData struct {
				Events []struct {
					Type    string          `json:"type"`
					Content json.RawMessage `json:"content"`
				} `json:"events"`
			} `json:"account_data"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		out := map[string]json.RawMessage{}
		for _, ev := range resp.AccountData.Events {
			out[ev.Type] = ev.Content
		}

		return out, resp.NextBatch, time.Since(start)
	}

	_, next, _ := syncOnce("", 0)

	path := "/_matrix/client/v3/user/@adsync:test.local/account_data/m.secret_storage.default_key"
	if rec := do(t, server, http.MethodPut, path, `{"key":"abc"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d", rec.Code)
	}

	data, next, took := syncOnce(next, 10000)
	if took > 3*time.Second || string(data["m.secret_storage.default_key"]) != `{"key":"abc"}` {
		t.Fatalf("sync после записи: %v за %s, ожидали сразу", data, took)
	}

	if rec := do(t, server, http.MethodDelete, path, "", tok); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d", rec.Code)
	}

	data, _, took = syncOnce(next, 10000)
	if took > 3*time.Second || string(data["m.secret_storage.default_key"]) != `{}` {
		t.Fatalf("sync после удаления: %v за %s, ожидали content {} сразу", data, took)
	}

	if rec := do(t, server, http.MethodGet, path, "", tok); rec.Code != http.StatusNotFound {
		t.Fatalf("GET удаленной: %d, ожидали 404", rec.Code)
	}

	if rec := do(t, server, http.MethodGet, "/_matrix/client/v3/account/3pid", "", tok); rec.Code != http.StatusOK || rec.Body.String() != "{\"threepids\":[]}\n" {
		t.Fatalf("3pid: %d %s", rec.Code, rec.Body.String())
	}
}
