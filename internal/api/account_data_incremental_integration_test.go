//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// adSyncResult - account data из ответа /sync: глобальные и по комнатам.
type adSyncResult struct {
	next   string
	global map[string]string
	rooms  map[string]map[string]string
}

// adSync делает /sync и раскладывает account data по типам.
func adSync(t *testing.T, server *Server, token, since string, timeout int) adSyncResult {
	t.Helper()

	url := "/_matrix/client/v3/sync?timeout=" + strconv.Itoa(timeout)
	if since != "" {
		url += "&since=" + since
	}

	rec := do(t, server, http.MethodGet, url, "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	type events struct {
		Events []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		} `json:"events"`
	}

	var resp struct {
		NextBatch   string `json:"next_batch"`
		AccountData events `json:"account_data"`
		Rooms       struct {
			Join map[string]struct {
				AccountData events `json:"account_data"`
			} `json:"join"`
		} `json:"rooms"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	out := adSyncResult{next: resp.NextBatch, global: map[string]string{}, rooms: map[string]map[string]string{}}

	for _, ev := range resp.AccountData.Events {
		out.global[ev.Type] = string(ev.Content)
	}

	for roomID, room := range resp.Rooms.Join {
		for _, ev := range room.AccountData.Events {
			if out.rooms[roomID] == nil {
				out.rooms[roomID] = map[string]string{}
			}

			out.rooms[roomID][ev.Type] = string(ev.Content)
		}
	}

	return out
}

// createTestRoom создает комнату и возвращает ее id.
func createTestRoom(t *testing.T, server *Server, token string) string {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"ad"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("createRoom: %s", rec.Body.String())
	}

	return created.RoomID
}

// TestAccountDataIncrementalSync - инкрементальный /sync отдает только
// account data, измененную после since (глобальную и комнатную); без изменений -
// ничего, в том числе в ответе по таймауту long-poll. Initial sync отдает все, кроме
// удаленных. m.push_rules приходит, когда правила менялись, и с текущими правилами.
// Раньше каждый /sync отдавал все account data заново.
func TestAccountDataIncrementalSync(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "adinc", "DEV")
	roomID := createTestRoom(t, server, tok)

	const base = "/_matrix/client/v3/user/@adinc:test.local/"

	put := func(path, body string) {
		t.Helper()

		if rec := do(t, server, http.MethodPut, base+path, body, tok); rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	put("account_data/m.direct", `{"@bob:test.local":["!r:test.local"]}`)
	put("account_data/com.example.gone", `{"x":1}`)
	put("rooms/"+roomID+"/account_data/com.example.room", `{"y":2}`)

	if rec := do(t, server, http.MethodDelete, base+"account_data/com.example.gone", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("DELETE: %d", rec.Code)
	}

	initial := adSync(t, server, tok, "", 0)
	if initial.global["m.direct"] == "" || initial.rooms[roomID]["com.example.room"] == "" {
		t.Fatalf("initial sync без account data: %+v", initial)
	}

	if _, ok := initial.global["com.example.gone"]; ok {
		t.Fatalf("initial sync отдал удаленную запись: %+v", initial.global)
	}

	if !strings.Contains(initial.global[core.AccountDataPushRules], `"global"`) {
		t.Fatalf("initial sync без m.push_rules: %+v", initial.global)
	}

	// Без изменений: ни глобальной, ни комнатной account data.
	empty := adSync(t, server, tok, initial.next, 0)
	if len(empty.global) != 0 || len(empty.rooms) != 0 {
		t.Fatalf("инкрементальный sync без изменений: %+v", empty)
	}

	// Таймаут long-poll тоже без account data.
	timedOut := adSync(t, server, tok, empty.next, 300)
	if len(timedOut.global) != 0 || len(timedOut.rooms) != 0 {
		t.Fatalf("sync по таймауту: %+v", timedOut)
	}

	// Изменилась одна глобальная запись - только она.
	put("account_data/m.direct", `{"@carol:test.local":["!r:test.local"]}`)

	changed := adSync(t, server, tok, timedOut.next, 0)
	if len(changed.global) != 1 || !strings.Contains(changed.global["m.direct"], "carol") || len(changed.rooms) != 0 {
		t.Fatalf("после изменения m.direct: %+v", changed)
	}

	// Изменилась комнатная - только она, глобальных нет.
	put("rooms/"+roomID+"/tags/u.work", `{"order":0.5}`)

	roomChanged := adSync(t, server, tok, changed.next, 0)
	if len(roomChanged.global) != 0 || len(roomChanged.rooms[roomID]) != 1 || roomChanged.rooms[roomID]["m.tag"] == "" {
		t.Fatalf("после тега: %+v", roomChanged)
	}

	// Изменение push-правил приходит как m.push_rules с текущими правилами.
	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/pushrules/global/content/kw1",
		`{"pattern":"релиз","actions":["notify"]}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("pushrule: %d %s", rec.Code, rec.Body.String())
	}

	rules := adSync(t, server, tok, roomChanged.next, 0)
	if len(rules.global) != 1 || !strings.Contains(rules.global[core.AccountDataPushRules], `"rule_id":"kw1"`) {
		t.Fatalf("после изменения правил: %+v", rules)
	}

	if again := adSync(t, server, tok, rules.next, 0); len(again.global) != 0 || len(again.rooms) != 0 {
		t.Fatalf("повторный sync после правил: %+v", again)
	}
}

// TestAccountDataServerTypes - m.fully_read и m.push_rules - серверные
// типы, через account data API их не записать и не удалить: 405 M_BAD_JSON, как в
// Synapse (m.fully_read - через /read_markers, правила - через /pushrules). Чтение
// разрешено.
func TestAccountDataServerTypes(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "adsrv", "DEV")
	roomID := createTestRoom(t, server, tok)

	const base = "/_matrix/client/v3/user/@adsrv:test.local/"

	for _, path := range []string{
		"account_data/m.push_rules",
		"account_data/m.fully_read",
		"rooms/" + roomID + "/account_data/m.fully_read",
		"rooms/" + roomID + "/account_data/m.push_rules",
	} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rec := do(t, server, method, base+path, `{"event_id":"$x","global":{}}`, tok)
			if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), ErrCodeBadJSON) {
				t.Fatalf("%s %s: %d %s, ожидали 405 M_BAD_JSON", method, path, rec.Code, rec.Body.String())
			}
		}
	}

	if rec := do(t, server, http.MethodGet, base+"account_data/m.push_rules", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("GET m.push_rules: %d", rec.Code)
	}
}

// TestRoomTagsConcurrent - параллельные PUT разных тегов одной комнаты
// сохраняют все теги. Раньше теги менялись чтением-изменением-записью без
// блокировки, и параллельный PUT затирал тег соседа.
func TestRoomTagsConcurrent(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "adtags", "DEV")
	roomID := createTestRoom(t, server, tok)

	base := "/_matrix/client/v3/user/@adtags:test.local/rooms/" + roomID + "/tags"

	const tags = 16

	for round := range 3 {
		var wg sync.WaitGroup

		names := make([]string, 0, tags)

		for i := range tags {
			name := "u.r" + strconv.Itoa(round) + "t" + strconv.Itoa(i)
			names = append(names, name)

			wg.Go(func() {
				if rec := do(t, server, http.MethodPut, base+"/"+name, `{"order":0.1}`, tok); rec.Code != http.StatusOK {
					t.Errorf("PUT %s: %d %s", name, rec.Code, rec.Body.String())
				}
			})
		}

		wg.Wait()

		rec := do(t, server, http.MethodGet, base, "", tok)

		var got tagsContent
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET tags: %d %s", rec.Code, rec.Body.String())
		}

		for _, name := range names {
			if _, ok := got.Tags[name]; !ok {
				t.Fatalf("раунд %d: тег %s потерян, есть %d из %d", round, name, len(got.Tags), (round+1)*tags)
			}
		}

		// Параллельные DELETE тоже ничего лишнего не удаляют.
		var del sync.WaitGroup

		for _, name := range names[:tags/2] {
			del.Go(func() {
				if rec := do(t, server, http.MethodDelete, base+"/"+name, "", tok); rec.Code != http.StatusOK {
					t.Errorf("DELETE %s: %d", name, rec.Code)
				}
			})
		}

		del.Wait()

		rec = do(t, server, http.MethodGet, base, "", tok)
		got = tagsContent{}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)

		for i, name := range names {
			_, ok := got.Tags[name]
			if ok == (i < tags/2) {
				t.Fatalf("раунд %d после DELETE: тег %s присутствует=%v", round, name, ok)
			}
		}

		if !slices.ContainsFunc(names, func(n string) bool { _, ok := got.Tags[n]; return ok }) {
			t.Fatalf("раунд %d: все теги пропали", round)
		}
	}
}

// TestAccountDataStorageError - ошибка БД при чтении account data и
// тегов - 500, а не 404 M_NOT_FOUND (клиент принял бы сбой за отсутствие данных и
// мог перезаписать их пустыми).
func TestAccountDataStorageError(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "aderr", "DEV")
	roomID := createTestRoom(t, server, tok)

	ctx := context.Background()
	if _, err := server.db.Pool().Exec(ctx, `ALTER TABLE account_data RENAME TO account_data_broken`); err != nil {
		t.Fatalf("rename: %v", err)
	}

	t.Cleanup(func() {
		_, _ = server.db.Pool().Exec(ctx, `ALTER TABLE account_data_broken RENAME TO account_data`)
	})

	const base = "/_matrix/client/v3/user/@aderr:test.local/"

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base + "account_data/m.direct"},
		{http.MethodGet, base + "rooms/" + roomID + "/account_data/m.tag"},
		{http.MethodDelete, base + "account_data/m.direct"},
		{http.MethodGet, base + "rooms/" + roomID + "/tags"},
	} {
		if rec := do(t, server, tc.method, tc.path, "", tok); rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s %s при сбое БД: %d %s, ожидали 500", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
