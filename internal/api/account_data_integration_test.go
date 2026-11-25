//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAccountData — глобальные и комнатные account data + /sync.
func TestAccountData(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "acctdata", "acctdata-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Чужой userId — 403.
	if rec := do(http.MethodPut, "/_matrix/client/v3/user/@other:test.local/account_data/m.direct", `{"a":"b"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("чужой пользователь: %d %s", rec.Code, rec.Body.String())
	}

	// Глобальные: PUT → GET.
	if rec := do(http.MethodPut, "/_matrix/client/v3/user/@acctdata:test.local/account_data/m.direct",
		`{"@bob:test.local":["!room:test.local"]}`); rec.Code != http.StatusOK {
		t.Fatalf("put global: %d %s", rec.Code, rec.Body.String())
	}

	rec := do(http.MethodGet, "/_matrix/client/v3/user/@acctdata:test.local/account_data/m.direct", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "@bob:test.local") {
		t.Fatalf("get global: %d %s", rec.Code, rec.Body.String())
	}

	// Неизвестный тип — 404.
	if rec = do(http.MethodGet, "/_matrix/client/v3/user/@acctdata:test.local/account_data/com.example.none", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("нет данных: %d", rec.Code)
	}

	// Комнатные: нужна комната — создаём и пишем.
	rec = do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"ad"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	path := "/_matrix/client/v3/user/@acctdata:test.local/rooms/" + created.RoomID + "/account_data/m.tag"
	if rec = do(http.MethodPut, path, `{"tags":{"u.work":{}}}`); rec.Code != http.StatusOK {
		t.Fatalf("put room: %d %s", rec.Code, rec.Body.String())
	}

	if rec = do(http.MethodGet, path, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "u.work") {
		t.Fatalf("get room: %d %s", rec.Code, rec.Body.String())
	}

	// /sync содержит глобальные account data и комнатные.
	rec = do(http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	var sync struct {
		AccountData struct {
			Events []struct {
				Type    string `json:"type"`
				Content struct {
					B []string `json:"@bob:test.local"`
				} `json:"content"`
			} `json:"events"`
		} `json:"account_data"`
		Rooms struct {
			Join map[string]struct {
				AccountData struct {
					Events []struct {
						Type string `json:"type"`
					} `json:"events"`
				} `json:"account_data"`
			} `json:"join"`
		} `json:"rooms"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &sync); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	foundGlobal := false
	for _, ev := range sync.AccountData.Events {
		if ev.Type == "m.direct" {
			foundGlobal = true
		}
	}

	if !foundGlobal {
		t.Fatalf("в /sync нет глобальных account data: %s", rec.Body.String())
	}

	room, ok := sync.Rooms.Join[created.RoomID]
	if !ok {
		t.Fatalf("в /sync нет комнаты %s", created.RoomID)
	}

	foundRoom := false
	for _, ev := range room.AccountData.Events {
		if ev.Type == "m.tag" {
			foundRoom = true
		}
	}

	if !foundRoom {
		t.Fatalf("в /sync нет комнатных account data: %s", rec.Body.String())
	}
}

// TestDeleteAccountData — DELETE account_data (глобальные и комнатные).
func TestDeleteAccountData(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "acctdel", "acctdel-pass-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	base := "/_matrix/client/v3/user/@acctdel:test.local/account_data/m.direct"

	if rec := do(http.MethodPut, base, `{"x":"y"}`); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodDelete, base, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodGet, base, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("после удаления: %d %s", rec.Code, rec.Body.String())
	}

	// Повторное удаление — 404.
	if rec := do(http.MethodDelete, base, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("повторное удаление: %d", rec.Code)
	}
}

// TestDeleteAccountDataUnstableMSC3391 (Complement msc3391): DELETE по
// unstable-префиксу org.matrix.msc3391 удаляет глобальные и комнатные данные.
func TestDeleteAccountDataUnstableMSC3391(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerHTTP(t, srv, "acctunst", "UDEV")

	rec := do(t, srv, http.MethodPost, "/_matrix/client/v3/createRoom", `{}`, token)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for _, path := range []string{
		"/user/@acctunst:test.local/account_data/org.example.test",
		"/user/@acctunst:test.local/rooms/" + created.RoomID + "/account_data/org.example.test",
	} {
		if rec := do(t, srv, http.MethodPut, "/_matrix/client/v3"+path, `{"x":"y"}`, token); rec.Code != http.StatusOK {
			t.Fatalf("put %s: %d %s", path, rec.Code, rec.Body.String())
		}

		if rec := do(t, srv, http.MethodDelete, "/_matrix/client/unstable/org.matrix.msc3391"+path, "", token); rec.Code != http.StatusOK {
			t.Fatalf("unstable delete %s: %d %s", path, rec.Code, rec.Body.String())
		}

		if rec := do(t, srv, http.MethodGet, "/_matrix/client/v3"+path, "", token); rec.Code != http.StatusNotFound {
			t.Fatalf("после удаления %s: %d", path, rec.Code)
		}
	}
}
