//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type pushRuleView struct {
	RuleID     string            `json:"rule_id"`
	Default    bool              `json:"default"`
	Enabled    bool              `json:"enabled"`
	Pattern    string            `json:"pattern"`
	Actions    []json.RawMessage `json:"actions"`
	Conditions []map[string]any  `json:"conditions"`
}

func pushRulesClient(t *testing.T, srv *Server, token string) func(method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	return func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		srv.Handler().ServeHTTP(rec, req)

		return rec
	}
}

func getRuleset(t *testing.T, do func(method, path, body string) *httptest.ResponseRecorder) map[string][]pushRuleView {
	t.Helper()

	rec := do(http.MethodGet, "/_matrix/client/v3/pushrules/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /pushrules/: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Global map[string][]pushRuleView `json:"global"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return resp.Global
}

func ruleIDs(rules []pushRuleView) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.RuleID)
	}

	return out
}

func findRule(rules []pushRuleView, id string) (pushRuleView, bool) {
	for _, r := range rules {
		if r.RuleID == id {
			return r, true
		}
	}

	return pushRuleView{}, false
}

// TestPushRulesElementFlow - запросы, которые Element Web делает из
// настроек уведомлений. Раньше /enabled серверных правил отвечал 404, а
// /actions выключал правило и затирал его условия.
func TestPushRulesElementFlow(t *testing.T) {
	srv := newIntegrationServer(t)
	do := pushRulesClient(t, srv, registerPusherProbe(t, srv, "prflow", "prflow-pass-1"))
	base := "/_matrix/client/v3/pushrules/global/"

	global := getRuleset(t, do)

	master, ok := findRule(global["override"], ".m.rule.master")
	if !ok || !master.Default || master.Enabled || master.Conditions == nil {
		t.Fatalf("нет .m.rule.master по спецификации: %v", global["override"])
	}

	for kind, id := range map[string]string{
		"override": ".m.rule.invite_for_me", "underride": ".m.rule.encrypted", "content": ".m.rule.contains_user_name",
	} {
		if _, ok := findRule(global[kind], id); !ok {
			t.Fatalf("нет %s/%s: %v", kind, id, ruleIDs(global[kind]))
		}
	}

	// Выключить все уведомления: .m.rule.master/enabled.
	if rec := do(http.MethodPut, base+"override/.m.rule.master/enabled", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("master enabled: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodGet, base+"override/.m.rule.master/enabled", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("GET master enabled: %d %s", rec.Code, rec.Body.String())
	}

	// Действия серверного правила: условия и enabled остаются.
	if rec := do(http.MethodPut, base+"underride/.m.rule.message/actions", `{"actions":["dont_notify"]}`); rec.Code != http.StatusOK {
		t.Fatalf("message actions: %d %s", rec.Code, rec.Body.String())
	}

	rec := do(http.MethodGet, base+"underride/.m.rule.message", "")

	var msgRule pushRuleView
	_ = json.Unmarshal(rec.Body.Bytes(), &msgRule)

	if rec.Code != http.StatusOK || !msgRule.Enabled || !msgRule.Default || len(msgRule.Conditions) != 1 ||
		len(msgRule.Actions) != 1 || string(msgRule.Actions[0]) != `"dont_notify"` {
		t.Fatalf(".m.rule.message после /actions: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodPut, base+"override/.m.rule.contains_display_name/enabled", `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("display_name enabled: %d %s", rec.Code, rec.Body.String())
	}

	// Mute комнаты: override-правило с room_id (id - room id, кодируется в пути).
	roomID := "!muted:test.local"
	muteBody := `{"conditions":[{"kind":"event_match","key":"room_id","pattern":"` + roomID + `"}],"actions":[]}`

	if rec := do(http.MethodPut, base+"override/"+url.PathEscape(roomID), muteBody); rec.Code != http.StatusOK {
		t.Fatalf("mute: %d %s", rec.Code, rec.Body.String())
	}

	global = getRuleset(t, do)

	if ids := ruleIDs(global["override"]); len(ids) < 2 || ids[0] != ".m.rule.master" || ids[1] != roomID {
		t.Fatalf("порядок override: %v", ids)
	}

	mute, _ := findRule(global["override"], roomID)
	if !mute.Enabled || mute.Default || len(mute.Conditions) != 1 {
		t.Fatalf("правило mute: %+v", mute)
	}

	if m, _ := findRule(global["override"], ".m.rule.master"); !m.Enabled {
		t.Fatal("master не включился")
	}

	if d, _ := findRule(global["override"], ".m.rule.contains_display_name"); d.Enabled {
		t.Fatal("contains_display_name не выключился")
	}

	// Снять mute.
	if rec := do(http.MethodDelete, base+"override/"+url.PathEscape(roomID), ""); rec.Code != http.StatusOK {
		t.Fatalf("unmute: %d %s", rec.Code, rec.Body.String())
	}

	if _, ok := findRule(getRuleset(t, do)["override"], roomID); ok {
		t.Fatal("mute остался после DELETE")
	}

	// Ключевые слова: content-правило; /actions не трогает pattern и enabled.
	if rec := do(http.MethodPut, base+"content/kw1", `{"pattern":"релиз","actions":["notify"]}`); rec.Code != http.StatusOK {
		t.Fatalf("keyword: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodPut, base+"content/kw1/actions", `{"actions":["notify",{"set_tweak":"highlight"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("keyword actions: %d %s", rec.Code, rec.Body.String())
	}

	kw, _ := findRule(getRuleset(t, do)["content"], "kw1")
	if !kw.Enabled || kw.Pattern != "релиз" || len(kw.Actions) != 2 {
		t.Fatalf("keyword после /actions: %+v", kw)
	}

	// m.push_rules в account data /sync: по нему Element обновляет состояние.
	rec = do(http.MethodGet, "/_matrix/client/v3/sync", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"m.push_rules"`) {
		t.Fatalf("нет m.push_rules в /sync: %d", rec.Code)
	}

	// Устаревший снимок (параллельные PUT Element) не должен попадать в /sync:
	// m.push_rules собирается из текущих правил. Записать снимок через account
	// data клиент не может - серверный тип, 405.
	userID := "@prflow:test.local"
	if rec := do(http.MethodPut, "/_matrix/client/v3/user/"+userID+"/account_data/m.push_rules", `{"global":{}}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put account data: %d %s, ожидали 405", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/sync", "")
	if !strings.Contains(rec.Body.String(), `"rule_id":"kw1"`) {
		t.Fatal("в /sync устаревший m.push_rules без kw1")
	}

	rec = do(http.MethodGet, "/_matrix/client/v3/user/"+userID+"/account_data/m.push_rules", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rule_id":"kw1"`) {
		t.Fatalf("GET account_data m.push_rules: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPushRulesBeforeAfter - порядок пользовательских правил по
// before/after; без них новое правило первое (как в Synapse).
func TestPushRulesBeforeAfter(t *testing.T) {
	srv := newIntegrationServer(t)
	do := pushRulesClient(t, srv, registerPusherProbe(t, srv, "prorder", "prorder-pass-1"))
	base := "/_matrix/client/v3/pushrules/global/content/"

	put := func(id, query string) {
		t.Helper()

		rec := do(http.MethodPut, base+id+query, `{"pattern":"`+id+`","actions":["notify"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("put %s%s: %d %s", id, query, rec.Code, rec.Body.String())
		}
	}

	put("a", "")
	put("b", "")
	put("c", "?before=a")
	put("d", "?after=a")
	put("e", "?after=b")

	want := "b,e,c,a,d,.m.rule.contains_user_name"
	if got := strings.Join(ruleIDs(getRuleset(t, do)["content"]), ","); got != want {
		t.Fatalf("порядок content: %s, ожидали %s", got, want)
	}

	// Повторный PUT без before/after переносит правило в начало.
	put("d", "")

	if got := strings.Join(ruleIDs(getRuleset(t, do)["content"]), ","); got != "d,b,e,c,a,.m.rule.contains_user_name" {
		t.Fatalf("порядок после повторного PUT: %s", got)
	}

	if rec := do(http.MethodPut, base+"x?before=nope", `{"pattern":"x","actions":["notify"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("before на несуществующее: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodPut, base+"x?after=.m.rule.contains_user_name", `{"pattern":"x","actions":["notify"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("after на серверное: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPushRulesErrors - коды ошибок по спецификации/Synapse.
func TestPushRulesErrors(t *testing.T) {
	srv := newIntegrationServer(t)
	do := pushRulesClient(t, srv, registerPusherProbe(t, srv, "prerr", "prerr-pass-1"))
	prefix := "/_matrix/client/v3/pushrules/"

	cases := []struct {
		method, path, body string
		code               int
		errcode            string
	}{
		{http.MethodGet, prefix + "device/override/x", "", http.StatusBadRequest, ErrCodeUnrecognized},
		{http.MethodPut, prefix + "global/bogus/x", `{"actions":["notify"]}`, http.StatusBadRequest, ErrCodeUnrecognized},
		{http.MethodPut, prefix + "global/override/.m.rule.custom", `{"actions":["notify"],"conditions":[]}`, http.StatusBadRequest, ErrCodeInvalidParam},
		{http.MethodPut, prefix + "global/content/nopattern", `{"actions":["notify"]}`, http.StatusBadRequest, ErrCodeInvalidParam},
		{http.MethodPut, prefix + "global/override/noactions", `{"conditions":[]}`, http.StatusBadRequest, ErrCodeInvalidParam},
		{http.MethodPut, prefix + "global/override/badaction", `{"conditions":[],"actions":["explode"]}`, http.StatusBadRequest, ErrCodeInvalidParam},
		{http.MethodPut, prefix + "global/override/.m.rule.master/actions", `{"actions":"notify"}`, http.StatusBadRequest, ErrCodeInvalidParam},
		{http.MethodPut, prefix + "global/override/.m.rule.master/enabled", `{}`, http.StatusBadRequest, ErrCodeMissingParam},
		{http.MethodPut, prefix + "global/override/.m.rule.nope/enabled", `{"enabled":true}`, http.StatusNotFound, ErrCodeNotFound},
		{http.MethodPut, prefix + "global/room/!none:test.local/enabled", `{"enabled":true}`, http.StatusNotFound, ErrCodeNotFound},
		{http.MethodGet, prefix + "global/override/missing", "", http.StatusNotFound, ErrCodeNotFound},
		{http.MethodDelete, prefix + "global/override/missing", "", http.StatusNotFound, ErrCodeNotFound},
		{http.MethodDelete, prefix + "global/override/.m.rule.master", "", http.StatusBadRequest, ErrCodeInvalidParam},
	}

	for _, tc := range cases {
		rec := do(tc.method, tc.path, tc.body)

		var body struct {
			Errcode string `json:"errcode"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		if rec.Code != tc.code || body.Errcode != tc.errcode {
			t.Errorf("%s %s: %d %s, ожидали %d %s", tc.method, tc.path, rec.Code, body.Errcode, tc.code, tc.errcode)
		}
	}

	// GET серверного правила и /actions.
	if rec := do(http.MethodGet, prefix+"global/override/.m.rule.suppress_notices", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"default":true`) {
		t.Fatalf("GET серверного правила: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodGet, prefix+"global/underride/.m.rule.call/actions", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"ring"`) {
		t.Fatalf("GET /actions: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(http.MethodGet, prefix+"global/", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"override"`) {
		t.Fatalf("GET /pushrules/global/: %d %s", rec.Code, rec.Body.String())
	}
}
