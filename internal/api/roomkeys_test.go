package api

import (
	"net/http"
	"testing"
)

// Element проверяет GET /room_keys/version при первом входе: маршрут должен
// существовать и требовать токен. Точный ответ auth-middleware, а не
// «не 404/405»: 500 и 503 тест не пропускает. Работа с бэкапом - в
// roomkeys_integration_test.go.
func TestRoomKeysRoutesRegistered(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/_matrix/client/v3/room_keys/version"},
		{http.MethodPost, "/_matrix/client/v3/room_keys/version"},
		{http.MethodGet, "/_matrix/client/v3/room_keys/keys"},
	} {
		assertTokenRequired(t, srv, tc.method, tc.path, "")
	}
}
