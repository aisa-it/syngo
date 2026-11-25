//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// TestFederationOutboundInvite — приглашение на чужой сервер уходит
// отдельным федеративным эндпоинтом, destination запоминается для fan-out.
func TestFederationOutboundInvite(t *testing.T) {
	env := newFedEnv(t)

	var (
		mu       sync.Mutex
		paths    []string
		payloads [][]byte
	)

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)

		mu.Lock()
		paths = append(paths, r.URL.Path)
		payloads = append(payloads, body)
		mu.Unlock()

		// Сервер приглашенного возвращает событие со своей подписью: без нее
		// приглашение не записывается. Раньше мок отвечал пустым
		// событием, и тест опирался на то, что ответ не проверяется.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(countersignInvite(t, env, body))
	}))
	defer remote.Close()

	host := strings.TrimPrefix(remote.URL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.TargetsOverride = func(context.Context, string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: "remote.test", Host: host, Scheme: "http"}}, nil
	}

	env.server.SetFederationTransport(transport.NewClient(transport.Options{
		Resolver: resolver, Key: env.server.federationKey, Origin: "test.local",
		AddressPolicy: transport.AllowAllAddresses(),
	}))

	// Клиентская сессия alice: токен кладём в БД (хранятся только хеши).
	ctx := context.Background()
	const accessToken = "syt_test46"

	if _, err := env.server.db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ('@alice:test.local','alice','x')
		 ON CONFLICT (user_id) DO NOTHING`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx,
		`INSERT INTO devices (user_id, device_id, display_name) VALUES ('@alice:test.local','DEV46','')
		 ON CONFLICT (user_id, device_id) DO NOTHING`); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	if _, err := env.server.db.Pool().Exec(ctx,
		`INSERT INTO access_tokens (token_hash, user_id, device_id) VALUES ($1,'@alice:test.local','DEV46')
		 ON CONFLICT (token_hash) DO NOTHING`, core.HashToken(accessToken)); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	client := func(method, path string, body any) *httptest.ResponseRecorder {
		var payload []byte

		if body != nil {
			var err error

			payload, err = json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
		}

		req := httptest.NewRequest(method, path, strings.NewReader(string(payload)))
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		env.server.Handler().ServeHTTP(rec, req)

		return rec
	}

	// Создаём комнату и приглашаем пользователя чужого сервера.
	rec := client(http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{"name": "invite"})
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: status %d, body %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal createRoom: %v", err)
	}

	rec = client(http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/invite",
		map[string]any{"user_id": "@bob:remote.test"})
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: status %d, body %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()

	if len(paths) == 0 {
		t.Fatal("чужой сервер не получил ни одного запроса: invite не доставлен")
	}

	if !strings.Contains(paths[0], "/_matrix/federation/v2/invite/") {
		t.Fatalf("invite ушёл не тем эндпоинтом: %s", paths[0])
	}

	var body struct {
		RoomVersion     string          `json:"room_version"`
		Event           json.RawMessage `json:"event"`
		InviteRoomState []struct {
			Type     string          `json:"type"`
			StateKey string          `json:"state_key"`
			Sender   string          `json:"sender"`
			Content  json.RawMessage `json:"content"`
		} `json:"invite_room_state"`
	}

	if err := json.Unmarshal(payloads[0], &body); err != nil {
		t.Fatalf("unmarshal invite body: %v (body %s)", err, payloads[0])
	}

	if body.RoomVersion == "" || len(body.Event) == 0 {
		t.Fatalf("тело invite без room_version/event: %s", payloads[0])
	}

	// Для комнаты, которой у пира нет, Synapse требует invite_room_state.
	if len(body.InviteRoomState) == 0 {
		t.Fatalf("invite_room_state пуст: %s", payloads[0])
	}

	stateTypes := map[string]bool{}
	for _, ev := range body.InviteRoomState {
		if ev.Sender == "" || len(ev.Content) == 0 {
			t.Fatalf("invite_room_state: неполное событие %+v", ev)
		}

		stateTypes[ev.Type] = true
	}

	// create обязателен; join_rules попадает, только если наша комната его пишет.
	if !stateTypes["m.room.create"] {
		t.Fatalf("invite_room_state без m.room.create: %v", stateTypes)
	}

	if len(body.InviteRoomState) < 2 {
		t.Fatalf("invite_room_state слишком короткий: %v", stateTypes)
	}

	var event struct {
		Type       string                       `json:"type"`
		StateKey   *string                      `json:"state_key"`
		Content    map[string]any               `json:"content"`
		Signatures map[string]map[string]string `json:"signatures"`
	}

	if err := json.Unmarshal(body.Event, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}

	if event.Type != "m.room.member" || event.StateKey == nil || *event.StateKey != "@bob:remote.test" {
		t.Fatalf("invite-событие = %+v", event)
	}

	if event.Content["membership"] != "invite" {
		t.Fatalf("membership = %v", event.Content["membership"])
	}

	if len(event.Signatures["test.local"]) == 0 {
		t.Fatal("invite-событие ушло без подписи нашего сервера")
	}

	// Synapse отвечает «Invite event ID must match», если event_id в URL
	// не равен reference-hash тела. Проверяем это сами.
	var full map[string]any
	if err := json.Unmarshal(body.Event, &full); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}

	if _, ok := full["hashes"]; !ok {
		t.Fatalf("в PDU нет hashes: %v", full)
	}

	if _, ok := full["event_id"]; ok {
		t.Fatalf("в PDU не должно быть event_id: %v", full)
	}

	if _, ok := full["origin"]; ok {
		t.Fatalf("в PDU не должно быть origin: %v", full)
	}

	referenceID, err := signing.ReferenceHash(full)
	if err != nil {
		t.Fatalf("ReferenceHash() error: %v", err)
	}

	if !strings.HasSuffix(paths[0], "/"+url.PathEscape(referenceID)) {
		t.Fatalf("event_id в URL (%s) не равен reference-hash (%s)", paths[0], referenceID)
	}

	// Сервер приглашённого не становится получателем событий комнаты до входа
	// его участника. Тест закреплял обратное: remote.test попадал в
	// destination_rooms сразу при приглашении.
	destinations, err := env.server.db.DestinationsForRoom(context.Background(), created.RoomID, "test.local")
	if err != nil {
		t.Fatalf("DestinationsForRoom() error: %v", err)
	}

	if slices.Contains(destinations, "remote.test") {
		t.Fatalf("remote.test стал получателем до входа: %v", destinations)
	}
}
