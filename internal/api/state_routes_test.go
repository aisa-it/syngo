package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestStateRoutesRequireAuth — новые маршруты состояния и модерации подключены
// и требуют токен. Проверяется точный ответ auth-middleware (401 с
// M_MISSING_TOKEN/M_UNKNOWN_TOKEN): раньше тест принимал и 503 сервера без
// хранилищ, то есть не отличал маршрут от отказа.
func TestStateRoutesRequireAuth(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)

	tests := []struct {
		method, path string
	}{
		{http.MethodGet, "/_matrix/client/v3/rooms/!r:test.local/state"},
		{http.MethodGet, "/_matrix/client/v3/rooms/!r:test.local/state/m.room.topic/"},
		{http.MethodPut, "/_matrix/client/v3/rooms/!r:test.local/state/m.room.topic/"},
		{http.MethodPut, "/_matrix/client/v3/rooms/!r:test.local/state/m.room.topic"},
		{http.MethodPost, "/_matrix/client/v3/rooms/!r:test.local/kick"},
		{http.MethodPost, "/_matrix/client/v3/rooms/!r:test.local/ban"},
		{http.MethodPost, "/_matrix/client/v3/rooms/!r:test.local/unban"},
		{http.MethodPut, "/_matrix/client/v3/rooms/!r:test.local/redact/$e:test.local/txn1"},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()

			assertTokenRequired(t, srv, tc.method, tc.path, "{}")
		})
	}
}

// Element читает состояние с пустым state_key: GET /state/m.room.encryption/
// (и без слэша). Оба пути отдают содержимое события, а не 405;
// раньше тест проверял только «не 405».
func TestRoomStateGetEmptyKeyRoutes(t *testing.T) {
	t.Parallel()

	srv := newRoomsServer(t)
	token := registerUser(t, srv, "stateget", "1234567890").AccessToken
	roomID := createRoomWith(t, srv, token,
		`{"initial_state":[{"type":"m.room.encryption","state_key":"","content":{"algorithm":"m.megolm.v1.aes-sha2"}}]}`)

	for _, path := range []string{
		"/_matrix/client/v3/rooms/" + roomID + "/state/m.room.encryption",
		"/_matrix/client/v3/rooms/" + roomID + "/state/m.room.encryption/",
	} {
		rec := do(t, srv, http.MethodGet, path, "", token)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}

		var content struct {
			Algorithm string `json:"algorithm"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &content); err != nil || content.Algorithm != "m.megolm.v1.aes-sha2" {
			t.Fatalf("%s: содержимое %s (%v)", path, rec.Body.String(), err)
		}
	}
}

// Element кодирует event_id (%24…): маршруты должны декодировать его.
func TestEventIDParamDecodes(t *testing.T) {
	t.Parallel()

	got, err := decodePathValue("%24abc%3Atest")
	if err != nil || got != "$abc:test" {
		t.Fatalf("decodePathValue = %q, %v", got, err)
	}
}

// Element запрашивает контекст события для permalink: маршрут закрыт токеном и с
// percent-encoded event_id отдает само событие. Раньше тест проверял
// только «не 404/405».
func TestEventContextRouteExists(t *testing.T) {
	t.Parallel()

	srv := newRoomsServer(t)
	assertTokenRequired(t, srv, http.MethodGet, "/_matrix/client/v3/rooms/!r:test/context/$e:test", "")

	token := registerUser(t, srv, "ctx", "1234567890").AccessToken
	roomID := createRoomWith(t, srv, token, `{}`)
	eventID := sendMessage(t, srv, token, roomID, "ctx1", `{"msgtype":"m.text","body":"permalink"}`)

	// Как Element: «$» и «:» в event_id закодированы.
	encoded := strings.NewReplacer("$", "%24", ":", "%3A").Replace(eventID)

	rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/rooms/"+roomID+"/context/"+encoded, "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("context: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Event struct {
			EventID string `json:"event_id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Event.EventID != eventID {
		t.Fatalf("context: event_id %q, ожидали %q (%v)", resp.Event.EventID, eventID, err)
	}
}
