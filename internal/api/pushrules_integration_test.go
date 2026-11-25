//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPushRules — дефолтные правила и CRUD пользовательских.
func TestPushRules(t *testing.T) {
	srv := newIntegrationServer(t)
	token := registerPusherProbe(t, srv, "pushrules", "pushrules-pass-1")

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Дефолтные правила.
	rec := do(http.MethodGet, "/_matrix/client/v3/pushrules/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get rules: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Global map[string][]struct {
			RuleID  string `json:"rule_id"`
			Enabled bool   `json:"enabled"`
		} `json:"global"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Global["override"]) == 0 || len(resp.Global["underride"]) == 0 {
		t.Fatalf("нет дефолтных правил: %s", rec.Body.String())
	}

	// Своё правило.
	rulePath := "/_matrix/client/v3/pushrules/global/content/my.rule"
	if rec = do(http.MethodPut, rulePath, `{"actions":["notify"],"pattern":"hello","conditions":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("put rule: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, rulePath, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("get rule: %d %s", rec.Code, rec.Body.String())
	}

	// Отключение.
	if rec = do(http.MethodPut, rulePath+"/enabled", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, rulePath, "")

	var rule struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)

	if rule.Enabled {
		t.Fatalf("правило не отключилось: %s", rec.Body.String())
	}

	// Удаление.
	if rec = do(http.MethodDelete, rulePath, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	if rec = do(http.MethodGet, rulePath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("после удаления: %d", rec.Code)
	}
}
