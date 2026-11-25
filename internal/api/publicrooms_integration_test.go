//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPublicRooms — публичный каталог отдаёт только is_public комнаты.
func TestPublicRooms(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "catalog", "catalog-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Приватная и публичная комнаты.
	if rec := do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"private"}`); rec.Code != http.StatusOK {
		t.Fatalf("private room: %d %s", rec.Code, rec.Body.String())
	}

	rec := do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"public","preset":"public_chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("public room: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Каталог доступен без токена.
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_matrix/client/v3/publicRooms?limit=10", http.NoBody)
	srv.Handler().ServeHTTP(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("publicRooms: %d %s", rec2.Code, rec2.Body.String())
	}

	var resp struct {
		Chunk []struct {
			RoomID    string `json:"room_id"`
			NumJoined int    `json:"num_joined_members"`
		} `json:"chunk"`
		NextBatch string `json:"next_batch"`
	}

	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	found := false

	for _, room := range resp.Chunk {
		if room.RoomID == created.RoomID {
			found = true

			if room.NumJoined != 1 {
				t.Fatalf("num_joined = %d, ожидали 1", room.NumJoined)
			}
		}
	}

	if !found {
		t.Fatalf("публичная комната не попала в каталог: %s", rec2.Body.String())
	}
}

// TestPublicRoomsDirectoryFields (Complement TestPublicRooms): каталог
// отдает name, topic, canonical_alias и join_rule, ищет по generic_search_term,
// а /directory/list/room снимает комнату с публикации.
func TestPublicRoomsDirectoryFields(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "dirfields", "dirfields-password-1")
	other := registerPusherProbe(t, srv, "dirother", "dirother-password-1")

	do := func(method, path, body, bearer string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}

		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"visibility":"public","room_alias_name":"wombles","name":"Test Name","topic":"Test Topic Wombles"}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	type chunk struct {
		RoomID         string `json:"room_id"`
		Name           string `json:"name"`
		Topic          string `json:"topic"`
		CanonicalAlias string `json:"canonical_alias"`
		JoinRule       string `json:"join_rule"`
		NumJoined      int    `json:"num_joined_members"`
	}

	var resp struct {
		Chunk []chunk `json:"chunk"`
		Total int     `json:"total_room_count_estimate"`
	}

	rec = do(http.MethodPost, "/_matrix/client/v3/publicRooms", `{"filter":{"generic_search_term":"wombles"}}`, token)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("publicRooms: %d %s", rec.Code, rec.Body.String())
	}

	want := chunk{
		RoomID: created.RoomID, Name: "Test Name", Topic: "Test Topic Wombles",
		CanonicalAlias: "#wombles:test.local", JoinRule: "public", NumJoined: 1,
	}
	if len(resp.Chunk) != 1 || resp.Chunk[0] != want || resp.Total != 1 {
		t.Fatalf("поиск wombles: %s", rec.Body.String())
	}

	rec = do(http.MethodPost, "/_matrix/client/v3/publicRooms", `{"filter":{"generic_search_term":"nothing-like-this"}}`, token)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Chunk) != 0 {
		t.Fatalf("поиск без совпадений: %s", rec.Body.String())
	}

	listPath := "/_matrix/client/v3/directory/list/room/" + created.RoomID

	rec = do(http.MethodGet, listPath, "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"public"`) {
		t.Fatalf("GET visibility: %d %s", rec.Code, rec.Body.String())
	}

	// Не участник снять публикацию не может.
	if rec = do(http.MethodPut, listPath, `{"visibility":"private"}`, other); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT visibility чужим: %d %s", rec.Code, rec.Body.String())
	}

	if rec = do(http.MethodPut, listPath, `{"visibility":"private"}`, token); rec.Code != http.StatusOK {
		t.Fatalf("PUT visibility: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, listPath, "", "")
	if !strings.Contains(rec.Body.String(), `"private"`) {
		t.Fatalf("GET visibility после снятия: %s", rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/publicRooms", "", "")
	if strings.Contains(rec.Body.String(), created.RoomID) {
		t.Fatalf("снятая комната осталась в каталоге: %s", rec.Body.String())
	}

	if rec = do(http.MethodGet, "/_matrix/client/v3/directory/list/room/!nope:test.local", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET visibility неизвестной комнаты: %d", rec.Code)
	}
}
