//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRoomKeysBackupFlow — полный цикл серверного бэкапа ключей, как его проходит
// Element: версия, загрузка, выбор лучшего ключа, выдача по комнате и
// сессии, M_WRONG_ROOM_KEYS_VERSION после смены версии, удаление.
func TestRoomKeysBackupFlow(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "alice", "ADEV")

	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()

		rec := do(t, server, method, "/_matrix/client/v3"+path, body, tok)

		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)

		return rec.Code, out
	}

	if code, _ := call(http.MethodGet, "/room_keys/version", ""); code != http.StatusNotFound {
		t.Fatalf("без бэкапа: %d, ожидали 404", code)
	}

	const alg = "m.megolm_backup.v1.curve25519-aes-sha2"

	code, body := call(http.MethodPost, "/room_keys/version", `{"algorithm":"`+alg+`","auth_data":{"public_key":"pk1"}}`)
	if code != http.StatusOK || body["version"] != "1" {
		t.Fatalf("создание версии: %d %v", code, body)
	}

	room := url.PathEscape("!r:test.local")
	key := func(fmi, fwd int, verified bool, data string) string {
		b, _ := json.Marshal(map[string]any{
			"first_message_index": fmi, "forwarded_count": fwd, "is_verified": verified,
			"session_data": map[string]any{"ciphertext": data},
		})

		return string(b)
	}

	code, body = call(http.MethodPut, "/room_keys/keys?version=1",
		`{"rooms":{"!r:test.local":{"sessions":{"s1":`+key(5, 1, false, "A")+`,"s2":`+key(0, 0, false, "B")+`}}}}`)
	if code != http.StatusOK || body["count"] != float64(2) || body["etag"] != "1" {
		t.Fatalf("загрузка: %d %v", code, body)
	}

	// Худший ключ (больший first_message_index) не заменяет лучший, etag не меняется.
	code, body = call(http.MethodPut, "/room_keys/keys/"+room+"/s1?version=1", key(9, 0, false, "worse"))
	if code != http.StatusOK || body["etag"] != "1" {
		t.Fatalf("худший ключ: %d %v", code, body)
	}

	// Проверенный ключ лучше непроверенного.
	if code, body = call(http.MethodPut, "/room_keys/keys/"+room+"?version=1",
		`{"sessions":{"s1":`+key(7, 0, true, "verified")+`}}`); code != http.StatusOK || body["etag"] != "2" {
		t.Fatalf("лучший ключ: %d %v", code, body)
	}

	code, body = call(http.MethodGet, "/room_keys/keys/"+room+"/s1?version=1", "")
	if code != http.StatusOK || !strings.Contains(mustJSON(t, body), `"verified"`) {
		t.Fatalf("ключ сессии: %d %v", code, body)
	}

	if code, _ = call(http.MethodGet, "/room_keys/keys/"+room+"/nope?version=1", ""); code != http.StatusNotFound {
		t.Fatalf("неизвестная сессия: %d, ожидали 404", code)
	}

	code, body = call(http.MethodGet, "/room_keys/keys/"+url.PathEscape("!other:test.local")+"?version=1", "")
	if code != http.StatusOK || mustJSON(t, body) != `{"sessions":{}}` {
		t.Fatalf("пустая комната: %d %v", code, body)
	}

	code, body = call(http.MethodGet, "/room_keys/version/1", "")
	if code != http.StatusOK || body["count"] != float64(2) || body["algorithm"] != alg {
		t.Fatalf("версия 1: %d %v", code, body)
	}

	if code, _ = call(http.MethodPut, "/room_keys/version/1", `{"algorithm":"other","auth_data":{}}`); code != http.StatusBadRequest {
		t.Fatalf("смена algorithm: %d, ожидали 400", code)
	}

	if code, _ = call(http.MethodPut, "/room_keys/keys?version=1", `{"rooms":{"!r:test.local":{"sessions":{"s3":{"x":1}}}}}`); code != http.StatusBadRequest {
		t.Fatalf("некорректный ключ: %d, ожидали 400", code)
	}

	// Новая версия: запись в старую - 403 M_WRONG_ROOM_KEYS_VERSION с current_version.
	if code, body = call(http.MethodPost, "/room_keys/version", `{"algorithm":"`+alg+`","auth_data":{"public_key":"pk2"}}`); body["version"] != "2" {
		t.Fatalf("версия 2: %d %v", code, body)
	}

	code, body = call(http.MethodPut, "/room_keys/keys?version=1", `{"rooms":{}}`)
	if code != http.StatusForbidden || body["errcode"] != "M_WRONG_ROOM_KEYS_VERSION" || body["current_version"] != "2" {
		t.Fatalf("запись в старую версию: %d %v", code, body)
	}

	if code, _ = call(http.MethodPut, "/room_keys/keys?version=99", `{"rooms":{}}`); code != http.StatusNotFound {
		t.Fatalf("несуществующая версия: %d, ожидали 404", code)
	}

	// Удаление ключей и версий; номер удаленной версии не выдается повторно.
	if code, body = call(http.MethodDelete, "/room_keys/keys/"+room+"/s2?version=1", ""); code != http.StatusOK || body["count"] != float64(1) {
		t.Fatalf("удаление сессии: %d %v", code, body)
	}

	if code, _ = call(http.MethodDelete, "/room_keys/version/2", ""); code != http.StatusOK {
		t.Fatalf("удаление версии 2: %d", code)
	}

	if code, body = call(http.MethodGet, "/room_keys/version", ""); code != http.StatusOK || body["version"] != "1" {
		t.Fatalf("после удаления 2 текущая: %d %v", code, body)
	}

	if code, body = call(http.MethodPost, "/room_keys/version", `{"algorithm":"`+alg+`","auth_data":{}}`); body["version"] != "3" {
		t.Fatalf("после удаления выдана версия %v (%d), ожидали 3", body["version"], code)
	}

	if code, _ = call(http.MethodGet, "/room_keys/version/2", ""); code != http.StatusNotFound {
		t.Fatalf("удаленная версия: %d, ожидали 404", code)
	}
}
