//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

// admin-контур за TLS-прокси - IP клиента в аудите, лимит неудач по
// учетке общий с CS-логином.

// newProxiedAdminServer - сервер с admin API за доверенным прокси 10.0.0.0/8,
// администратор adminuser и лимит неудач по учетке (burst 2); возвращает
// функцию запроса от прокси 10.0.0.5 с клиентом 203.0.113.10 в X-Forwarded-For.
func newProxiedAdminServer(t *testing.T) func(method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()

	srv := newIntegrationServerWithConfig(t, nil, func(cfg *config.Config) {
		cfg.Admin.Enabled = true
		cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	})

	_ = registerPusherProbe(t, srv, "adminuser", "admin-user-pass-1")

	if _, err := srv.db.Pool().Exec(t.Context(),
		`UPDATE users SET is_admin = true WHERE localpart = 'adminuser'`); err != nil {
		t.Fatalf("flag admin: %v", err)
	}

	// Лимиты включаются после регистрации: /register тоже под лимитом по IP.
	srv.limiters = newLimiters(config.RateLimit{
		LoginAccount: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 2},
	})

	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "10.0.0.5:40000"
		req.Header.Set("X-Forwarded-For", "203.0.113.10")
		req.Header.Set("Content-Type", "application/json")

		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	return call
}

// TestAdminAuditClientIPBehindProxy - аудит admin хранит IP клиента из
// X-Forwarded-For доверенного прокси, а не адрес прокси.
func TestAdminAuditClientIPBehindProxy(t *testing.T) {
	call := newProxiedAdminServer(t)

	rec := call(http.MethodPost, "/_syngo/admin/v1/login", `{"username":"adminuser","password":"admin-user-pass-1"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}

	var login struct {
		AdminToken string `json:"admin_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || login.AdminToken == "" {
		t.Fatalf("admin login body: %s", rec.Body.String())
	}

	// Аудит входа хранит IP клиента, а не адрес прокси.
	rec = call(http.MethodGet, "/_syngo/admin/v1/audit?after_id=0&limit=50", "", login.AdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body.String())
	}

	var audit struct {
		Items []struct {
			Action  string         `json:"action"`
			Details map[string]any `json:"details"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil {
		t.Fatalf("decode audit: %v (%s)", err, rec.Body.String())
	}

	found := false

	for _, item := range audit.Items {
		if item.Action == "login" {
			found = true

			if item.Details["ip"] != "203.0.113.10" {
				t.Fatalf("ip в аудите входа = %v, ожидали 203.0.113.10", item.Details["ip"])
			}
		}
	}

	if !found {
		t.Fatalf("нет записи login в аудите: %s", rec.Body.String())
	}
}

// TestAdminLoginAccountLimitShared - неудачи admin-входа и CS-логина - одно
// ведро учетки: перебор попеременно через оба входа упирается в общий лимит.
func TestAdminLoginAccountLimitShared(t *testing.T) {
	call := newProxiedAdminServer(t)

	if rec := call(http.MethodPost, "/_syngo/admin/v1/login", `{"username":"AdminUser","password":"wrong-pass-1"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("неверный пароль admin: %d %s", rec.Code, rec.Body.String())
	}

	if rec := call(http.MethodPost, "/_matrix/client/v3/login",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"adminuser"},"password":"wrong-pass-2"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("неверный пароль CS: %d %s", rec.Code, rec.Body.String())
	}

	if rec := call(http.MethodPost, "/_syngo/admin/v1/login", `{"username":"@adminuser:test.local","password":"admin-user-pass-1"}`, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("admin login после двух неудач: %d %s, ожидали 429", rec.Code, rec.Body.String())
	}
}
