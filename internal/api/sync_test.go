package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// newSyncServer собирает сервер с аутентификацией, комнатами и /sync на in-memory хранилище.
func newSyncServer(t *testing.T) (*Server, *coretest.MemRoomStore) {
	t.Helper()

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.RateLimit = config.RateLimit{} // лимиты проверяются отдельно
	cfg.Sync.TokenSecret = "test-secret"
	cfg.Sync.MaxTimeoutMilliseconds = 5000
	cfg.Sync.PerRoomLimit = 20

	server := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.auth = core.NewAuth(newMemStore(), fastHasher{}, &countingTokens{}, core.AuthOptions{
		ServerName:        "test.local",
		RegistrationOpen:  true,
		MinPasswordLength: 8,
	})

	memStore := coretest.NewMemRoomStore()
	server.rooms = core.NewRoomService(memStore, "test.local")
	server.syncStore = memStore
	server.syncLoader = core.NewSyncDataLoader(memStore)
	server.syncTokens = core.NewSyncTokenCodec("test.local", []byte("test-secret"))

	return server, memStore
}

// decodeSync разбирает ответ /sync.
func decodeSync(t *testing.T, body []byte) syncResponse {
	t.Helper()

	var resp syncResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal sync: %v (body %.200s)", err, body)
	}

	return resp
}

// TestSyncInitial — initial /sync: комната, timeline с сообщением, состояние и next_batch.
func TestSyncInitial(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"name":"sync-комната"}`)
	sendMessage(t, server, alice.AccessToken, roomID, "t1", `{"msgtype":"m.text","body":"привет"}`)

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: status %d, body %s", rec.Code, rec.Body.String())
	}

	resp := decodeSync(t, rec.Body.Bytes())

	if resp.NextBatch == "" {
		t.Fatal("next_batch пуст")
	}

	joined, ok := resp.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("комната %s отсутствует в rooms.join", roomID)
	}

	if len(joined.Timeline.Events) != 8 {
		t.Fatalf("timeline: %d событий, ожидали 8 (7 событий создания с пресетом + сообщение)",
			len(joined.Timeline.Events))
	}

	if len(joined.State.Events) == 0 {
		t.Fatal("state.events пуст")
	}

	if resp.DeviceLists.Changed == nil || resp.Rooms.Invite == nil {
		t.Fatal("пустые разделы должны быть объектами, а не null")
	}
}

// TestSyncIncremental — с since отдаётся только новое событие.
func TestSyncIncremental(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	first := decodeSync(t, rec.Body.Bytes())

	// Ничего не менялось: пустая дельта с тем же токеном.
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?since="+first.NextBatch, "", alice.AccessToken)
	empty := decodeSync(t, rec.Body.Bytes())

	if len(empty.Rooms.Join[roomID].Timeline.Events) != 0 {
		t.Fatalf("пустая дельта содержит %d событий", len(empty.Rooms.Join[roomID].Timeline.Events))
	}

	// Новое сообщение — в дельте ровно одно событие.
	sendMessage(t, server, alice.AccessToken, roomID, "t2", `{"msgtype":"m.text","body":"новое"}`)

	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?since="+first.NextBatch, "", alice.AccessToken)
	delta := decodeSync(t, rec.Body.Bytes())

	events := delta.Rooms.Join[roomID].Timeline.Events
	if len(events) != 1 {
		t.Fatalf("дельта содержит %d событий, ожидали 1", len(events))
	}

	var content struct {
		Body string `json:"body"`
	}

	if err := json.Unmarshal(events[0].Content, &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}

	if content.Body != "новое" {
		t.Fatalf("в дельте событие %q, ожидали «новое»", content.Body)
	}

	if delta.Rooms.Join[roomID].Timeline.PrevBatch == "" {
		t.Fatal("prev_batch пуст для incremental-порции")
	}
}

// TestSyncInvalidToken — испорченный since отбивается M_UNKNOWN_POS.
func TestSyncInvalidToken(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	for _, since := range []string{"garbage", "v1.e10", "v1.e10.Zm9v"} {
		rec := do(t, server, http.MethodGet,
			"/_matrix/client/v3/sync?since="+since, "", alice.AccessToken)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("since=%q: status %d, ожидали 400", since, rec.Code)
		}

		if errResp := decodeMatrixError(t, rec); errResp.Code != ErrCodeUnknownPos {
			t.Fatalf("since=%q: errcode %q, ожидали %q", since, errResp.Code, ErrCodeUnknownPos)
		}
	}
}

// TestSyncLongPollTimeout — long-poll без событий отдаёт пустой ответ по таймауту.
func TestSyncLongPollTimeout(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	first := decodeSync(t, rec.Body.Bytes())

	// Таймаут 50 мс: событий нет, ответ — пустой, токен тот же.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/sync?since=%s&timeout=50", first.NextBatch), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("long-poll: status %d, body %s", rec.Code, rec.Body.String())
	}

	resp := decodeSync(t, rec.Body.Bytes())
	if resp.NextBatch != first.NextBatch {
		t.Fatalf("next_batch изменился без событий: %q → %q", first.NextBatch, resp.NextBatch)
	}
}

// TestSyncLazyLoadMembers — с lazy_load_members в state остаются только нужные m.room.member.
func TestSyncLazyLoadMembers(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bob.UserID), alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: status %d", rec.Code)
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status %d", rec.Code)
	}

	sendMessage(t, server, alice.AccessToken, roomID, "t3", `{"msgtype":"m.text","body":"от алисы"}`)

	filter := `{"room":{"state":{"lazy_load_members":true}}}`
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?filter="+filter, "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync с фильтром: status %d, body %s", rec.Code, rec.Body.String())
	}

	resp := decodeSync(t, rec.Body.Bytes())

	members := []string{}

	for _, event := range resp.Rooms.Join[roomID].State.Events {
		if event.Type == "m.room.member" && event.StateKey != nil {
			members = append(members, *event.StateKey)
		}
	}

	if len(members) == 0 {
		t.Fatal("lazy_load_members вырезал все membership-события")
	}

	for _, member := range members {
		if member != alice.UserID && member != bob.UserID {
			t.Fatalf("в state лишний участник %q (ожидались только %s и %s)", member, alice.UserID, bob.UserID)
		}
	}
}

// TestSyncInviteRoom — приглашённая комната приходит в rooms.invite со stripped state.
func TestSyncInviteRoom(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"name":"приглашение"}`)

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bob.UserID), alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: status %d", rec.Code)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", bob.AccessToken)
	resp := decodeSync(t, rec.Body.Bytes())

	invited, ok := resp.Rooms.Invite[roomID]
	if !ok {
		t.Fatalf("комната %s отсутствует в rooms.invite", roomID)
	}

	if len(invited.InviteState.Events) == 0 {
		t.Fatal("invite_state пуст")
	}
}

// TestSyncTokenSecretRequiresConfig — без секрета сервер стартует только
// в dev/test, и случайные секреты не совпадают между процессами.
func TestSyncTokenSecretRequiresConfig(t *testing.T) {
	t.Parallel()

	buildServer := func(env, secret string) *Server {
		cfg := config.Default()
		cfg.Env = env
		cfg.Matrix.ServerName = "test.local"
		cfg.Sync.TokenSecret = secret
		cfg.RateLimit = config.RateLimit{}

		return New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// dev без секрета: сервер поднимается, но токены процесса случайны.
	first := buildServer("dev", "")
	second := buildServer("dev", "")

	if first.syncTokens == nil {
		t.Fatal("в dev токены должны работать со случайным секретом")
	}

	if first.syncTokens.Encode(5) == second.syncTokens.Encode(5) {
		t.Fatal("два процесса с пустым секретом выдали одинаковый токен: секрет дефолтный")
	}

	// Явный секрет: токены совпадают между процессами.
	third := buildServer("prod", "shared-secret")
	fourth := buildServer("prod", "shared-secret")

	if third.syncTokens.Encode(5) != fourth.syncTokens.Encode(5) {
		t.Fatal("с явным секретом токены должны совпадать")
	}

	// prod без секрета до New не доходит: конфиг не проходит валидацию.
	prodCfg := config.Default()
	prodCfg.Env = "prod"

	if err := prodCfg.Validate(); err == nil {
		t.Fatal("prod без sync.token_secret должен быть невалиден")
	}
}

// TestSyncTypingAndReceipts — typing и receipt приходят второму клиенту
// через ephemeral, не попадая в timeline.
func TestSyncTypingAndReceipts(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bob.UserID), alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d", rec.Code)
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	messageID := sendMessage(t, server, alice.AccessToken, roomID, "ep1", `{"body":"привет"}`)

	// alice печатает и ставит receipt.
	rec = do(t, server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/typing/%s", roomID, alice.UserID),
		`{"typing":true,"timeout":30000}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("typing: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/receipt/m.read/%s", roomID, messageID), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt: %d %s", rec.Code, rec.Body.String())
	}

	// bob видит их в ephemeral своей комнаты.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", bob.AccessToken)
	resp := decodeSync(t, rec.Body.Bytes())

	room, ok := resp.Rooms.Join[roomID]
	if !ok {
		t.Fatalf("комната отсутствует в /sync bob")
	}

	types := make([]string, 0, len(room.Ephemeral.Events))

	for _, event := range room.Ephemeral.Events {
		types = append(types, event.Type)
	}

	if !contains(strings.Join(types, ","), "m.typing") || !contains(strings.Join(types, ","), "m.receipt") {
		t.Fatalf("ephemeral = %v, ожидали m.typing и m.receipt", types)
	}

	// В timeline эфемерных событий быть не должно.
	for _, event := range room.Timeline.Events {
		if event.Type == "m.typing" || event.Type == "m.receipt" {
			t.Fatalf("эфемерное событие попало в timeline: %s", event.Type)
		}
	}
}

// TestPresence — presence своего статуса и истечение в offline.
func TestPresence(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	rec := do(t, server, http.MethodPut,
		"/_matrix/client/v3/presence/"+alice.UserID+"/status",
		`{"presence":"online","status_msg":"на связи"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("presence PUT: %d", rec.Code)
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/presence/"+alice.UserID+"/status", "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("presence GET: %d", rec.Code)
	}

	if body := rec.Body.String(); !contains(body, `"online"`) || !contains(body, "на связи") {
		t.Fatalf("presence = %s", body)
	}

	// Чужой статус менять нельзя.
	rec = do(t, server, http.MethodPut,
		"/_matrix/client/v3/presence/"+alice.UserID+"/status",
		`{"presence":"offline"}`, bob.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("presence чужой: %d, ожидали 403", rec.Code)
	}
}

// TestTypingRequiresMembership — typing запрещён не-участнику.
func TestTypingRequiresMembership(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)

	rec := do(t, server, http.MethodPut,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/typing/%s", roomID, bob.UserID),
		`{"typing":true}`, bob.AccessToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("typing не-участника: %d, ожидали 403", rec.Code)
	}
}

// TestSyncLimitedFlag — limited=true только когда история обрезана: при
// количестве событий ≤ limit флаг false, prev_batch при этом есть (как в
// Synapse sync.py — token по первому событию).
func TestSyncLimitedFlag(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomFor(t, server, alice.AccessToken)

	// 3 сообщения + служебные события комнаты, лимит 50: не обрезано.
	for i, text := range []string{"a", "b", "c"} {
		sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("lim-%d", i), `{"msgtype":"m.text","body":"`+text+`"}`)
	}

	rec := do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?filter="+url.QueryEscape(`{"room":{"timeline":{"limit":50}}}`), "", alice.AccessToken)
	sync := decodeSync(t, rec.Body.Bytes())

	timeline := sync.Rooms.Join[roomID].Timeline
	if len(timeline.Events) < 3 {
		t.Fatalf("timeline = %d событий, ожидали минимум 3", len(timeline.Events))
	}

	if timeline.Limited {
		t.Fatal("limited=true, хотя событий меньше лимита 50")
	}

	if timeline.PrevBatch == "" {
		t.Fatal("prev_batch пуст, хотя события есть (как в Synapse)")
	}

	// Лимит 2 при большем числе событий: обрезано, отдаются последние 2.
	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?filter="+url.QueryEscape(`{"room":{"timeline":{"limit":2}}}`), "", alice.AccessToken)
	sync = decodeSync(t, rec.Body.Bytes())

	timeline = sync.Rooms.Join[roomID].Timeline
	if len(timeline.Events) != 2 {
		t.Fatalf("timeline = %d событий при лимите 2", len(timeline.Events))
	}

	if !timeline.Limited {
		t.Fatal("limited=false, хотя история обрезана")
	}

	if timeline.PrevBatch == "" {
		t.Fatal("prev_batch пуст при обрезке")
	}
}

// TestMessagesPaginationNoLoss — пагинация /messages назад выдаёт все события
// ровно по одному разу, а from принимает prev_batch из /sync (единый формат
// токенов).
func TestMessagesPaginationNoLoss(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomFor(t, server, alice.AccessToken)

	const total = 5

	for i := 0; i < total; i++ {
		sendMessage(t, server, alice.AccessToken, roomID, fmt.Sprintf("pg-%d", i),
			fmt.Sprintf(`{"msgtype":"m.text","body":"msg-%d"}`, i))
	}

	syncRec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	syncResp := decodeSync(t, syncRec.Body.Bytes())

	// prev_batch из /sync обязан приниматься как from (иначе 400).
	prevBatch := syncResp.Rooms.Join[roomID].Timeline.PrevBatch
	if prevBatch == "" {
		t.Fatal("prev_batch пуст в initial sync при наличии событий")
	}

	recPrev := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=2&from=%s",
			rolePath(roomID), url.QueryEscape(prevBatch)), "", alice.AccessToken)
	if recPrev.Code != http.StatusOK {
		t.Fatalf("messages с prev_batch из /sync: %d %s", recPrev.Code, recPrev.Body.String())
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0

	for pages < 20 {
		path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=2", rolePath(roomID))
		if cursor != "" {
			path += "&from=" + url.QueryEscape(cursor)
		}

		rec := do(t, server, http.MethodGet, path, "", alice.AccessToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("messages: %d %s", rec.Code, rec.Body.String())
		}

		var page struct {
			Chunk []struct {
				EventID string `json:"event_id"`
				Type    string `json:"type"`
			} `json:"chunk"`
			End string `json:"end"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode messages: %v", err)
		}

		for _, ev := range page.Chunk {
			seen[ev.EventID]++
		}

		if page.End == cursor || len(page.Chunk) == 0 {
			break
		}

		cursor = page.End
		pages++
	}

	for id, count := range seen {
		if count > 1 {
			t.Fatalf("событие %s выдано %d раза", id, count)
		}
	}

	// Все сообщения комнаты должны быть получены хотя бы раз.
	if len(seen) < total {
		t.Fatalf("получено %d событий, ожидали минимум %d (без потерь)", len(seen), total)
	}
}

// TestSyncLongPollTimeoutKeepsToken — по таймауту long-poll next_batch равен
// since: иначе события, пришедшие во время ожидания, но не попавшие в ответ,
// теряются навсегда.
func TestSyncLongPollTimeoutKeepsToken(t *testing.T) {
	t.Parallel()

	server, _ := newSyncServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice.AccessToken)
	first := decodeSync(t, rec.Body.Bytes())

	rec = do(t, server, http.MethodGet,
		"/_matrix/client/v3/sync?timeout=1&since="+url.QueryEscape(first.NextBatch), "", alice.AccessToken)
	second := decodeSync(t, rec.Body.Bytes())

	if second.NextBatch != first.NextBatch {
		t.Fatalf("next_batch по таймауту = %q, ожидали since %q", second.NextBatch, first.NextBatch)
	}
}

// TestSyncNotifierAddressing — адресное пробуждение: NotifyUser(alice) не будит
// ожидание bob, а общий Notify будит всех.
func TestSyncNotifierAddressing(t *testing.T) {
	t.Parallel()

	notifier := newSyncNotifier()

	aliceWait := make(chan bool, 1)
	bobWait := make(chan bool, 1)

	go func() { aliceWait <- notifier.WaitFor(context.Background(), 2*time.Second, "@alice:test") }()
	go func() { bobWait <- notifier.WaitFor(context.Background(), 2*time.Second, "@bob:test") }()

	for notifier.Waiters() < 2 {
		time.Sleep(time.Millisecond)
	}

	notifier.NotifyUser("@alice:test")

	if !<-aliceWait {
		t.Fatal("NotifyUser не разбудил адресата")
	}

	select {
	case <-bobWait:
		t.Fatal("NotifyUser разбудил чужое ожидание")
	case <-time.After(50 * time.Millisecond):
	}

	notifier.Notify()

	if !<-bobWait {
		t.Fatal("Notify не разбудил ожидание")
	}
}
