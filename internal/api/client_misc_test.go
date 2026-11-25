package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Element Web валит синхронизацию, если pushrules/protocols/turnServer отвечают 404.
// thirdparty/protocols - публичный; pushrules и turnServer требуют сессию: без нее они
// отдавали дефолт и пустые креды. Проверяются точные ответы: раньше тест
// принимал 503 сервера без auth-хранилища и не отличал маршрут от отказа.
func TestElementStartupEndpoints(t *testing.T) {
	t.Parallel()

	srv := newRouteAuthServer(t)

	rec := do(t, srv, http.MethodGet, "/_matrix/client/v3/thirdparty/protocols", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("thirdparty/protocols: код %d, тело %s", rec.Code, rec.Body.String())
	}

	assertTokenRequired(t, srv, http.MethodGet, "/_matrix/client/v3/pushrules/", "")
	assertTokenRequired(t, srv, http.MethodGet, "/_matrix/client/v3/voip/turnServer", "")

	token := registerUser(t, srv, "element", "1234567890").AccessToken

	// С сессией pushrules отдает набор правил по умолчанию (global.override не пуст).
	rec = do(t, srv, http.MethodGet, "/_matrix/client/v3/pushrules/", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("pushrules: %d %s", rec.Code, rec.Body.String())
	}

	var rules struct {
		Global struct {
			Override []json.RawMessage `json:"override"`
		} `json:"global"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rules); err != nil || len(rules.Global.Override) == 0 {
		t.Fatalf("pushrules: нет правил по умолчанию (%v): %.200s", err, rec.Body.String())
	}

	// TURN не настроен: 200 и пустой объект (Element ждет 200 на старте).
	rec = do(t, srv, http.MethodGet, "/_matrix/client/v3/voip/turnServer", "", token)

	var turn map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &turn); rec.Code != http.StatusOK || err != nil || len(turn) != 0 {
		t.Fatalf("turnServer без TURN: %d %q, ожидали 200 {}", rec.Code, rec.Body.String())
	}
}
