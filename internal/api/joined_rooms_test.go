package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// joined_rooms нужен клиентам при старте: без токена - 401, с токеном - 200 и
// массив (не null), даже без БД (раньше тест проверял только «не 404» и
// проходил при 500/503). Содержимое списка на живой БД - TestJoinedRoomsListsMemberships.
func TestJoinedRoomsRouteExists(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)
	assertTokenRequired(t, srv, http.MethodGet, "/_matrix/client/v3/joined_rooms", "")

	token := registerUser(t, srv, "joiner", "1234567890").AccessToken

	rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/joined_rooms", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("joined_rooms: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		JoinedRooms *[]string `json:"joined_rooms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JoinedRooms == nil {
		t.Fatalf("joined_rooms: нет массива в ответе (%v): %s", err, rec.Body.String())
	}
}
