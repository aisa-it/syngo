package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// newRoomsServer собирает сервер с аутентификацией и комнатами на in-memory хранилище.
func newRoomsServer(t *testing.T) *Server {
	t.Helper()

	server := newAuthServer(t, core.AuthOptions{})
	server.rooms = core.NewRoomService(coretest.NewMemRoomStore(), "test.local")

	return server
}

// createRoom создаёт комнату от имени владельца токена и возвращает room_id.
func createRoomWith(t *testing.T, server *Server, token, body string) string {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal createRoom: %v", err)
	}

	if resp.RoomID == "" {
		t.Fatal("createRoom не вернул room_id")
	}

	return resp.RoomID
}

// sendMessage отправляет событие и возвращает event_id.
func sendMessage(t *testing.T, server *Server, token, roomID, txnID, body string) string {
	t.Helper()

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s", rolePath(roomID), txnID)

	rec := do(t, server, http.MethodPut, path, body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("send(%s): status %d, body %s", txnID, rec.Code, rec.Body.String())
	}

	var resp struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal send: %v", err)
	}

	return resp.EventID
}

// rolePath подставляет room_id в путь (room_id содержит «!» и «:», но chi это допускает).
func rolePath(roomID string) string {
	return roomID
}

// TestRoomScenario — A создаёт комнату → 3 сообщения →
// B вступает → читает историю в обратном порядке.
func TestRoomScenario(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)

	aliceReg := registerUser(t, server, "alice", "correct horse battery")
	bobReg := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, aliceReg.AccessToken, `{"name":"Сценарная комната"}`)

	// B не участник: отправка и чтение запрещены.
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/early", roomID)

	rec := do(t, server, http.MethodPut, path, `{"body":"нельзя"}`, bobReg.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("send до join: status %d, ожидали 403", rec.Code)
	}

	messages := []string{"первое", "второе", "третье"}

	eventIDs := make([]string, 0, len(messages))

	for i, body := range messages {
		eventID := sendMessage(t, server, aliceReg.AccessToken, roomID,
			fmt.Sprintf("txn%d", i+1), fmt.Sprintf(`{"msgtype":"m.text","body":%q}`, body))
		eventIDs = append(eventIDs, eventID)
	}

	// Повтор с тем же txn_id не создаёт дубль.
	repeated := sendMessage(t, server, aliceReg.AccessToken, roomID, "txn1", `{"msgtype":"m.text","body":"первое"}`)
	if repeated != eventIDs[0] {
		t.Fatalf("повтор txn1 вернул %q, ожидали %q", repeated, eventIDs[0])
	}

	// A приглашает B, B вступает.
	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bobReg.UserID), aliceReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bobReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status %d, body %s", rec.Code, rec.Body.String())
	}

	// B читает историю в обратном порядке.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=50", roomID), "", bobReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages: status %d, body %s", rec.Code, rec.Body.String())
	}

	var history messagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}

	var bodies []string

	for _, event := range history.Chunk {
		if event.Type != "m.room.message" {
			continue
		}

		var content struct {
			Body string `json:"body"`
		}

		if err := json.Unmarshal(event.Content, &content); err != nil {
			t.Fatalf("unmarshal content: %v", err)
		}

		bodies = append(bodies, content.Body)
	}

	if len(bodies) != 3 {
		t.Fatalf("сообщений в истории %d, ожидали 3: %v", len(bodies), bodies)
	}

	wantOrder := []string{"третье", "второе", "первое"}
	for i := range wantOrder {
		if bodies[i] != wantOrder[i] {
			t.Fatalf("порядок истории %v, ожидали %v", bodies, wantOrder)
		}
	}

	// Чтение события по id доступно участнику.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", roomID, eventIDs[0]), "", bobReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("event: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestRoomPermissions(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)

	aliceReg := registerUser(t, server, "alice", "correct horse battery")
	bobReg := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, aliceReg.AccessToken, `{}`)

	// Посторонний не читает историю и не получает событие: /messages - 403,
	// /event - 404, существование события не раскрывается (как в Synapse).
	if rec := do(t, server, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b", roomID),
		"", bobReg.AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("messages посторонним: status %d, ожидали 403", rec.Code)
	}

	if rec := do(t, server, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/$nope", roomID),
		"", bobReg.AccessToken); rec.Code != http.StatusNotFound {
		t.Fatalf("event посторонним: status %d, ожидали 404", rec.Code)
	}

	// Не участник не может приглашать.
	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bobReg.UserID), bobReg.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invite посторонним: status %d, ожидали 403", rec.Code)
	}

	// Неизвестная комната — 403 как в Synapse: членства нет, значит нет и доступа.
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/rooms/!missing:test.local/messages?dir=b", "", aliceReg.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("messages в неизвестной комнате: status %d, ожидали 403", rec.Code)
	}

	// Неизвестное событие в своей комнате — 404 M_NOT_FOUND.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/$missing", roomID), "", aliceReg.AccessToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("event($missing): status %d, ожидали 404", rec.Code)
	}
}

func TestRoomValidation(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	aliceReg := registerUser(t, server, "alice", "correct horse battery")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			name: "версия комнаты v9", method: http.MethodPost, path: "/_matrix/client/v3/createRoom",
			body: `{"room_version":"9"}`, status: 400, code: ErrCodeUnsupportedRoomVersion,
		},
		{
			name: "битый JSON createRoom", method: http.MethodPost, path: "/_matrix/client/v3/createRoom",
			body: `{"name":`, status: 400, code: ErrCodeNotJSON,
		},
		{
			name: "неверный limit", method: http.MethodGet,
			path: "/_matrix/client/v3/rooms/!x:test.local/messages?limit=abc", status: 400, code: ErrCodeBadJSON,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, server, tc.method, tc.path, tc.body, aliceReg.AccessToken)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, ожидали %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}

			if errResp := decodeMatrixError(t, rec); errResp.Code != tc.code {
				t.Fatalf("errcode = %q, ожидали %q", errResp.Code, tc.code)
			}
		})
	}
}

func TestRoomRequiresAuth(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, ожидали 401", rec.Code)
	}

	if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeMissingToken {
		t.Fatalf("errcode = %q, ожидали %q", errResp.Code, ErrCodeMissingToken)
	}
}

func TestRoomEventFormat(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	aliceReg := registerUser(t, server, "alice", "correct horse battery")

	roomID := createRoomWith(t, server, aliceReg.AccessToken, `{}`)
	eventID := sendMessage(t, server, aliceReg.AccessToken, roomID, "fmt", `{"body":"формат"}`)

	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", roomID, eventID), "", aliceReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("event: status %d", rec.Code)
	}

	var event eventView
	if err := json.Unmarshal(rec.Body.Bytes(), &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}

	if !strings.HasPrefix(event.EventID, "$") || event.EventID != eventID {
		t.Fatalf("event_id = %q, ожидали $… и %q", event.EventID, eventID)
	}

	if event.RoomID != roomID || event.Sender != aliceReg.UserID || event.Type != "m.room.message" {
		t.Fatalf("поля события: %+v", event)
	}

	if event.OriginServerTS <= 0 || len(event.Content) == 0 {
		t.Fatalf("content/origin_server_ts пустые: %+v", event)
	}
}

// TestSendRateLimited - /send подключает rate_limit.send
// (rc_message), третий запрос при burst=2 получает 429 M_LIMIT_EXCEEDED.
func TestSendRateLimited(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{
		Send: config.RateLimitAction{Enabled: true, PerSecond: 0.001, Burst: 2},
	}

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName:        "test.local",
		RegistrationOpen:  true,
		MinPasswordLength: 8,
	})
	server.rooms = core.NewRoomService(coretest.NewMemRoomStore(), "test.local")

	aliceReg := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomWith(t, server, aliceReg.AccessToken, `{}`)

	send := func(txn string) int {
		rec := do(t, server, http.MethodPut,
			fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s", roomID, txn),
			`{"body":"hi"}`, aliceReg.AccessToken)

		return rec.Code
	}

	if code := send("t1"); code != http.StatusOK {
		t.Fatalf("1-й send: status %d, ожидали 200", code)
	}

	if code := send("t2"); code != http.StatusOK {
		t.Fatalf("2-й send: status %d, ожидали 200 (burst=2)", code)
	}

	rec := do(t, server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/t3", roomID),
		`{"body":"hi"}`, aliceReg.AccessToken)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3-й send: status %d, ожидали 429", rec.Code)
	}

	var body limitErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal 429: %v", err)
	}

	if body.Code != ErrCodeLimitExceeded || body.RetryAfterMS <= 0 {
		t.Fatalf("тело 429 = %+v", body)
	}

	// Другой пользователь имеет своё ведро.
	bobReg := registerUser(t, server, "bob", "correct horse battery")
	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bobReg.UserID), aliceReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: status %d", rec.Code)
	}

	if code := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bobReg.AccessToken).Code; code != http.StatusOK {
		t.Fatalf("join: status %d", code)
	}

	rec = do(t, server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/bob1", roomID),
		`{"body":"hi"}`, bobReg.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("send от другого пользователя: status %d, ожидали 200", rec.Code)
	}
}

// TestInviteValidatesUserID — MXID без @…:… отбивается 400 M_INVALID_PARAM.
func TestInviteValidatesUserID(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	aliceReg := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomWith(t, server, aliceReg.AccessToken, `{}`)

	for _, userID := range []string{"", "alice", "@alice", "alice:test.local", "@:test.local", "@alice:"} {
		body := fmt.Sprintf(`{"user_id":%q}`, userID)
		rec := do(t, server, http.MethodPost,
			fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID), body, aliceReg.AccessToken)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invite(%q): status %d, ожидали 400 (body %s)", userID, rec.Code, rec.Body.String())
		}

		if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeInvalidParam {
			t.Fatalf("invite(%q): errcode %q, ожидали %q", userID, errResp.Code, ErrCodeInvalidParam)
		}
	}
}

// TestCreateRoomIsDirect - is_direct в createRoom ставит is_direct=true
// в content приглашений (по нему Element показывает комнату приглашенному как личный
// чат); без is_direct поля нет.
func TestCreateRoomIsDirect(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	registerUser(t, server, "bob", "correct horse battery")

	for _, tc := range []struct {
		body string
		want any
	}{
		{`{"preset":"trusted_private_chat","is_direct":true,"invite":["@bob:test.local"]}`, true},
		{`{"preset":"private_chat","invite":["@bob:test.local"]}`, nil},
	} {
		roomID := createRoomWith(t, server, alice.AccessToken, tc.body)

		code, member := upgradeState(t, server, alice.AccessToken, roomID, "m.room.member", "@bob:test.local")
		if code != http.StatusOK || member["membership"] != "invite" || member["is_direct"] != tc.want {
			t.Errorf("%s: invite %d %v, want is_direct=%v", tc.body, code, member, tc.want)
		}
	}
}

// TestCreateRoomV11WithoutCreator - в v11 m.room.create без creator
// (создатель - отправитель события), комната при этом работает: создатель меняет
// состояние и приглашает; в v10 creator остается.
func TestCreateRoomV11WithoutCreator(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	for _, version := range []string{"10", "11"} {
		roomID := createRoomWith(t, server, alice.AccessToken, `{"room_version":"`+version+`"}`)

		_, create := upgradeState(t, server, alice.AccessToken, roomID, "m.room.create", "")
		creator, has := create["creator"]

		if version == "11" && has {
			t.Errorf("v11: creator в m.room.create: %v", create)
		}

		if version == "10" && creator != alice.UserID {
			t.Errorf("v10: creator = %v", creator)
		}

		rec := do(t, server, http.MethodPut,
			fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.name/", roomID), `{"name":"n"}`, alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Errorf("v%s: смена имени создателем: %d %s", version, rec.Code, rec.Body.String())
		}

		rec = do(t, server, http.MethodPost,
			fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID), fmt.Sprintf(`{"user_id":%q}`, bob.UserID), alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Errorf("v%s: invite: %d %s", version, rec.Code, rec.Body.String())
		}
	}
}
