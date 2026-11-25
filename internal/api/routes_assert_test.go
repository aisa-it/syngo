package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Помощники проверок маршрутов. Раньше тесты маршрутов проверяли только
// «не 404»: сервер без хранилищ отвечал 503, и тест проходил и при 500/503 от
// сломанного обработчика. Теперь маршрут проверяется по таблице роутера, а закрытость
// токеном - по точному ответу auth-middleware.

// assertRouteRegistered - роутер знает method+path (без запуска обработчика).
func assertRouteRegistered(t *testing.T, srv *Server, method, path string) {
	t.Helper()

	routes, ok := srv.Handler().(chi.Routes)
	if !ok {
		t.Fatalf("роутер %T не реализует chi.Routes", srv.Handler())
	}

	if !routes.Match(chi.NewRouteContext(), method, path) {
		t.Fatalf("%s %s: маршрут не зарегистрирован", method, path)
	}
}

// assertTokenRequired - маршрут зарегистрирован и закрыт requireAuth: без токена -
// ровно 401 M_MISSING_TOKEN, с неизвестным токеном - 401 M_UNKNOWN_TOKEN. Сервер с
// in-memory аутентификацией, поэтому 503 «хранилище не настроено» здесь - ошибка.
func assertTokenRequired(t *testing.T, srv *Server, method, path, body string) {
	t.Helper()

	assertRouteRegistered(t, srv, method, path)

	for _, tc := range []struct{ token, errcode string }{
		{"", ErrCodeMissingToken},
		{"syt_unknown", ErrCodeUnknownToken},
	} {
		rec := do(t, srv, method, path, body, tc.token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s (token=%q): код %d, ожидали 401; тело %s", method, path, tc.token, rec.Code, rec.Body.String())
		}

		if got := decodeMatrixError(t, rec).Code; got != tc.errcode {
			t.Fatalf("%s %s (token=%q): errcode %q, ожидали %q", method, path, tc.token, got, tc.errcode)
		}
	}
}

// newRouteAuthServer - сервер с in-memory аутентификацией для проверок маршрутов.
func newRouteAuthServer(t *testing.T) *Server {
	t.Helper()

	return newAuthServer(t, core.AuthOptions{})
}
