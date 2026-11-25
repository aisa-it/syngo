package api

import (
	"net/http"
	"testing"
)

// Element создаёт sync-фильтр при старте: без POST /user/{id}/filter он не синкается.
// Маршруты закрыты токеном, а с токеном доходят до обработчика: чужой user_id в пути -
// 403 от обработчика, а не 404/503 (раньше проверялось только «не 200/404»).
func TestFilterRoutesRequireAuth(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)

	assertTokenRequired(t, srv, http.MethodPost, "/_matrix/client/v3/user/@a:test.local/filter",
		`{"room":{"timeline":{"limit":20}}}`)
	assertTokenRequired(t, srv, http.MethodGet, "/_matrix/client/v3/user/@a:test.local/filter/1", "")

	token := registerUser(t, srv, "filterer", "1234567890").AccessToken

	rec := do(t, srv, http.MethodPost, "/_matrix/client/v3/user/@other:test.local/filter",
		`{"room":{"timeline":{"limit":20}}}`, token)
	if rec.Code != http.StatusForbidden || decodeMatrixError(t, rec).Code != ErrCodeForbidden {
		t.Fatalf("фильтр для чужого пользователя: %d %s, ожидали 403 M_FORBIDDEN", rec.Code, rec.Body.String())
	}
}
