package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestJoinVersionSupported — версия комнаты должна быть в ver
// (по умолчанию ["1"]) и не выше FederationMaxRoomVersion.
func TestJoinVersionSupported(t *testing.T) {
	t.Parallel()

	request := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/make_join"+query, http.NoBody)
	}

	if joinVersionSupported(request(""), 10) {
		t.Fatal("без ver версия 10 не поддерживается (по умолчанию [\"1\"])")
	}

	if !joinVersionSupported(request("?ver=10&ver=11"), 10) {
		t.Fatal("ver=10 не принят")
	}

	// v12 по федерации (раньше - отказ при любом ver).
	if !joinVersionSupported(request("?ver=10&ver=11&ver=12"), 12) {
		t.Fatal("v12 с ver=12 не принят")
	}

	if joinVersionSupported(request("?ver=10&ver=11"), 12) || joinVersionSupported(request("?ver=13"), 13) {
		t.Fatal("версия вне ver или старше 12 принята")
	}
}

// TestRestrictedJoinHelpers — разбор allow-списка и join_authorised_via_users_server.
func TestRestrictedJoinHelpers(t *testing.T) {
	t.Parallel()

	rooms := restrictedAllowRooms(json.RawMessage(`{"join_rule":"restricted","allow":[
		{"type":"m.room_membership","room_id":"!a:test.local"},{"type":"other","room_id":"!b:test.local"},
		{"type":"m.room_membership"}]}`))
	if len(rooms) != 1 || rooms[0] != "!a:test.local" {
		t.Fatalf("allow = %v", rooms)
	}

	if restrictedAllowRooms(json.RawMessage(`[]`)) != nil {
		t.Fatal("битый join_rules")
	}

	if got := joinAuthoriser(json.RawMessage(`{"membership":"join","join_authorised_via_users_server":"@a:test.local"}`)); got != "@a:test.local" {
		t.Fatalf("authoriser = %q", got)
	}

	if !isRestrictedJoinRule("knock_restricted") || isRestrictedJoinRule("public") {
		t.Fatal("isRestrictedJoinRule")
	}
}

// incomingRequest - запрос ручки с origin в X-Matrix и eventId в пути.
func incomingRequest(t *testing.T, origin, eventID string) *http.Request {
	t.Helper()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatal(err)
	}

	header, err := signing.MatrixAuthHeader(key, origin, "test.local", http.MethodPut, "/x", nil)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPut, "/x", http.NoBody)
	r.Header.Set("Authorization", header)

	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("eventId", eventID)

	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, routeCtx))
}

// TestParseIncomingMembership — тип, membership, домен sender ==
// origin, room_id пути, event_id - по содержимому.
func TestParseIncomingMembership(t *testing.T) {
	t.Parallel()

	server, _ := newFederationServer(t)

	event := func(mutate func(map[string]any)) (map[string]any, string) {
		object := map[string]any{
			"room_id": "!r:test.local", "type": "m.room.member", "sender": "@bob:remote.test",
			"state_key": "@bob:remote.test", "content": map[string]any{"membership": "join"},
			"depth": 4, "origin_server_ts": 1, "prev_events": []any{}, "auth_events": []any{},
		}

		if mutate != nil {
			mutate(object)
		}

		id, err := core.EventID(object, 10)
		if err != nil {
			t.Fatal(err)
		}

		return object, id
	}

	good, goodID := event(nil)

	in, rejected := server.parseIncomingMembership(incomingRequest(t, "remote.test", goodID), good, "!r:test.local", "join", 10)
	if rejected != nil {
		t.Fatalf("корректный join отклонен: %v", rejected)
	}

	if in.pdu.EventID != goodID || in.origin != "remote.test" || requireSelfMembership(in) != nil {
		t.Fatalf("разбор: %+v", in)
	}

	notSelf, notSelfID := event(func(o map[string]any) { o["state_key"] = "@carol:remote.test" })

	in, rejected = server.parseIncomingMembership(incomingRequest(t, "remote.test", notSelfID), notSelf, "!r:test.local", "join", 10)
	if rejected != nil || requireSelfMembership(in) == nil {
		t.Fatalf("join не за себя: %v", rejected)
	}

	withBodyID, withBodyIDRef := event(func(o map[string]any) { o["event_id"] = "$forged" })

	cases := []struct {
		name    string
		object  map[string]any
		pathID  string
		origin  string
		roomID  string
		version int
		status  int
	}{
		{"другой origin", good, goodID, "other.test", "!r:test.local", 10, http.StatusForbidden},
		{"без X-Matrix", good, goodID, "", "!r:test.local", 10, http.StatusUnauthorized},
		{"другая комната", good, goodID, "remote.test", "!other:test.local", 10, http.StatusBadRequest},
		{"event_id пути", good, "$forged", "remote.test", "!r:test.local", 10, http.StatusBadRequest},
		{"event_id тела", withBodyID, withBodyIDRef, "remote.test", "!r:test.local", 10, http.StatusBadRequest},
		{"версия v12", good, goodID, "remote.test", "!r:test.local", 12, http.StatusBadRequest},
	}

	for _, tc := range cases {
		r := incomingRequest(t, "remote.test", tc.pathID)
		if tc.origin != "remote.test" {
			r = incomingRequest(t, tc.origin, tc.pathID)
		}

		if tc.origin == "" {
			r.Header.Del("Authorization")
		}

		if _, rejected := server.parseIncomingMembership(r, tc.object, tc.roomID, "join", tc.version); rejected == nil || rejected.status != tc.status {
			t.Errorf("%s: %v, ожидали %d", tc.name, rejected, tc.status)
		}
	}

	for name, mutate := range map[string]func(map[string]any){
		"тип power_levels": func(o map[string]any) { o["type"] = "m.room.power_levels" },
		"без state_key":    func(o map[string]any) { delete(o, "state_key") },
		"membership ban":   func(o map[string]any) { o["content"] = map[string]any{"membership": "ban"} },
		"без sender":       func(o map[string]any) { delete(o, "sender") },
	} {
		object, id := event(mutate)

		rejected := func() *membershipError {
			_, err := server.parseIncomingMembership(incomingRequest(t, "remote.test", id), object, "!r:test.local", "join", 10)

			return err
		}()
		if rejected == nil || rejected.status != http.StatusBadRequest {
			t.Errorf("%s: %v, ожидали 400", name, rejected)
		}
	}

	rec := httptest.NewRecorder()
	badMembership("x").write(rec)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrCodeInvalidParam) || badMembership("x").Error() != "x" {
		t.Fatalf("membershipError: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAddOurSignature — наша подпись добавляется к подписям
// отправителя, а не заменяет их.
func TestAddOurSignature(t *testing.T) {
	t.Parallel()

	server, key := newFederationServer(t)

	remote, err := signing.Generate("a_remote")
	if err != nil {
		t.Fatal(err)
	}

	object := map[string]any{
		"room_id": "!r:remote.test", "type": "m.room.member", "sender": "@bob:remote.test",
		"state_key": "@carol:test.local", "content": map[string]any{"membership": "invite"},
		"depth": 4, "origin_server_ts": 1, "prev_events": []any{}, "auth_events": []any{},
	}

	object, err = eventjson.SignVersioned(remote, "remote.test", object, 10)
	if err != nil {
		t.Fatal(err)
	}

	generic := map[string]any{}

	raw, _ := json.Marshal(object)
	_ = json.Unmarshal(raw, &generic)

	signed, err := server.addOurSignature(generic, 10)
	if err != nil {
		t.Fatal(err)
	}

	keys := map[string]signing.Key{"test.local": key, "remote.test": remote}
	lookup := func(origin, _ string) (string, int64, error) { return keys[origin].Public(), 0, nil }

	for _, origin := range []string{"test.local", "remote.test"} {
		if err := eventjson.VerifyVersioned(signed, origin, 10, lookup); err != nil {
			t.Fatalf("подпись %s: %v", origin, err)
		}
	}

	if _, err := (&Server{}).addOurSignature(generic, 10); err == nil {
		t.Fatal("без ключа федерации подпись не должна получаться")
	}
}

// TestFederationOriginHeader — origin из X-Matrix; без заголовка - пусто.
func TestFederationOriginHeader(t *testing.T) {
	t.Parallel()

	if got := federationOrigin(incomingRequest(t, "remote.test", "")); got != "remote.test" {
		t.Fatalf("origin = %q", got)
	}

	if got := federationOrigin(httptest.NewRequest(http.MethodGet, "/", http.NoBody)); got != "" {
		t.Fatalf("origin без заголовка = %q", got)
	}

	if membershipOf(json.RawMessage(`{"membership":"leave"}`)) != "leave" || withEventID(map[string]any{}, "$x")["event_id"] != "$x" {
		t.Fatal("membershipOf/withEventID")
	}
}
