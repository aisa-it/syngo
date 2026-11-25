package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestRegisterComplementExpectations — поведение /register, которое проверяет
// Complement (TestRegistration): допустимые символы localpart, приведение к
// нижнему регистру, device_id из запроса, flows и обязательность UIA-сессии.
func TestRegisterComplementExpectations(t *testing.T) {
	t.Parallel()

	t.Run("допустимые символы localpart", func(t *testing.T) {
		t.Parallel()

		for _, localpart := range []string{"a-b", "a.b", "a/b", "a3", "a=b", "a_b", "q"} {
			server := newAuthServer(t, core.AuthOptions{})
			plain := fmt.Sprintf(`{"username":%q,"password":"1234567890"}`, localpart)
			session := uiaSession(t, server, plain)

			rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
				registerBody(localpart, "1234567890", session), "")

			if rec.Code != http.StatusOK {
				t.Errorf("localpart %q: status %d, body %s", localpart, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("спецсимволы отклоняются", func(t *testing.T) {
		t.Parallel()

		server := newAuthServer(t, core.AuthOptions{})

		// Имя проверяется до UIA: 400 без шага auth, как в Synapse.
		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
			`{"username":"a$b","password":"1234567890"}`, "")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, ожидали 400 для a$b", rec.Code)
		}
	})

	t.Run("capitals downcased", func(t *testing.T) {
		t.Parallel()

		server := newAuthServer(t, core.AuthOptions{})
		plain := `{"username":"Bob","password":"1234567890"}`
		session := uiaSession(t, server, plain)

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
			registerBody("Bob", "1234567890", session), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}

		var resp registerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if resp.UserID != "@bob:test.local" {
			t.Fatalf("user_id = %q, ожидали @bob:test.local", resp.UserID)
		}
	})

	t.Run("device_id из запроса", func(t *testing.T) {
		t.Parallel()

		server := newAuthServer(t, core.AuthOptions{})
		plain := `{"username":"dev","password":"1234567890","device_id":"MYDEVICE"}`
		session := uiaSession(t, server, plain)

		body := fmt.Sprintf(`{"username":"dev","password":"1234567890","device_id":"MYDEVICE",
			"auth":{"type":"m.login.dummy","session":%q}}`, session)

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register", body, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}

		var resp registerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if resp.DeviceID != "MYDEVICE" {
			t.Fatalf("device_id = %q, ожидали MYDEVICE", resp.DeviceID)
		}
	})

	t.Run("без UIA-сессии 401 с flows", func(t *testing.T) {
		t.Parallel()

		server := newAuthServer(t, core.AuthOptions{})

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
			`{"username":"nosession","password":"1234567890"}`, "")

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status %d, ожидали 401", rec.Code)
		}

		var body struct {
			Flows   []map[string]any `json:"flows"`
			Session string           `json:"session"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if len(body.Flows) == 0 || body.Session == "" {
			t.Fatalf("flows=%v session=%q", body.Flows, body.Session)
		}
	})
}

// TestRegisterValidatesBeforeUIA - тело /register проверяется до UIA, как в Synapse:
// недопустимое и занятое имя дают 400 сразу, без 401 (Complement
// TestRegistration), а невалидный UTF-8 в JSON - 400 M_NOT_JSON
// (TestRequestEncodingFails).
func TestRegisterValidatesBeforeUIA(t *testing.T) {
	t.Parallel()

	server := newAuthServer(t, core.AuthOptions{})

	expect := func(t *testing.T, body string, status int, errcode string) {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register", body, "")
		if rec.Code != status {
			t.Fatalf("status %d, ожидали %d (body %s)", rec.Code, status, rec.Body.String())
		}

		var resp matrixError
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		if resp.Code != errcode {
			t.Fatalf("errcode %q, ожидали %q", resp.Code, errcode)
		}
	}

	t.Run("недопустимое имя без UIA", func(t *testing.T) {
		expect(t, `{"username":"user-£-reject","password":"sUp3rs3kr1t"}`,
			http.StatusBadRequest, ErrCodeInvalidUsername)
	})

	t.Run("занятое имя без UIA", func(t *testing.T) {
		session := uiaSession(t, server, `{"username":"taken","password":"sUp3rs3kr1t"}`)

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
			registerBody("taken", "sUp3rs3kr1t", session), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("регистрация: status %d, body %s", rec.Code, rec.Body.String())
		}

		expect(t, `{"username":"taken","password":"sUp3rs3kr1t"}`,
			http.StatusBadRequest, ErrCodeUserInUse)
	})

	t.Run("невалидный UTF-8", func(t *testing.T) {
		expect(t, "{ \"test\":\"a\x81\" }", http.StatusBadRequest, ErrCodeNotJSON)
	})
}
