//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoomTags — теги комнаты через account data m.tag.
func TestRoomTags(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "tagger", "tagger-password-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := do(http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"tags"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	base := "/_matrix/client/v3/user/@tagger:test.local/rooms/" + created.RoomID + "/tags"

	// Пустой список.
	rec = do(http.MethodGet, base, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tags":{}`) {
		t.Fatalf("пустые теги: %d %s", rec.Code, rec.Body.String())
	}

	// PUT тега.
	if rec = do(http.MethodPut, base+"/u.work", `{"order":0.5}`); rec.Code != http.StatusOK {
		t.Fatalf("put tag: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, base, "")

	var got struct {
		Tags map[string]struct {
			Order float64 `json:"order"`
		} `json:"tags"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if tag, ok := got.Tags["u.work"]; !ok || tag.Order != 0.5 {
		t.Fatalf("теги = %+v", got.Tags)
	}

	// Чужой пользователь — 403.
	if rec = do(http.MethodGet, "/_matrix/client/v3/user/@other:test.local/rooms/"+created.RoomID+"/tags", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("чужой: %d", rec.Code)
	}

	// DELETE тега.
	if rec = do(http.MethodDelete, base+"/u.work", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete tag: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, base, "")
	if !strings.Contains(rec.Body.String(), `"tags":{}`) {
		t.Fatalf("после удаления: %s", rec.Body.String())
	}
}
