//go:build integration

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// newIntegrationServer — сервер на живой БД (регистрация, to-device, /sync).
func newIntegrationServer(t *testing.T) *Server {
	t.Helper()

	return newIntegrationServerWithTracer(t, nil)
}

// newIntegrationServerWithTracer - то же с трейсером SQL на пуле (число и вид
// запросов HTTP-пути /sync).
func newIntegrationServerWithTracer(t *testing.T, tracer pgx.QueryTracer) *Server {
	t.Helper()

	return newIntegrationServerWithConfig(t, tracer, nil)
}

// newIntegrationServerWithConfig - то же с правкой конфига до сборки сервера
// (настройки, которые New читает один раз, например server.trusted_proxies).
func newIntegrationServerWithConfig(t *testing.T, tracer pgx.QueryTracer, mutate func(*config.Config)) *Server {
	t.Helper()

	dsn := testdb.DSN(t)

	m, err := migrate.New("file://../../migrations", "pgx5://"+strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://"))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db, err := store.OpenWithOptions(context.Background(), store.Options{DSN: dsn, MaxConns: 8, Tracer: tracer})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"
	cfg.Auth.RefreshTokens = true
	cfg.Auth.RegistrationOpen = true   // тесты регистрируют пользователей через /register
	cfg.RateLimit = config.RateLimit{} // лимиты проверяются отдельно
	cfg.Sync.TokenSecret = "integration-secret"
	cfg.Media.Directory = t.TempDir()
	cfg.Federation.Enabled = true
	cfg.Federation.ValidUntilDays = 7
	cfg.RateLimit = config.RateLimit{}

	if mutate != nil {
		mutate(&cfg)
	}

	srv := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Разбор push-действий фоновый: тесты проверяют счетчики сразу
	// после записи, а под -race и общей БД 200 мс по умолчанию мало. Ограничение
	// ожидания проверяется юнит-тестом core.
	srv.pushSyncWait = 10 * time.Second
	if srv.pushActions != nil {
		srv.pushActions.SetSyncWait(srv.pushSyncWait)
		t.Cleanup(func() { _ = srv.pushActions.Close(context.Background()) })
	}

	// Ключ федерации: тестам нужны подписи (в проде грузится из файла в main).
	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("federation key: %v", err)
	}

	srv.SetFederationKey(&key)

	return srv
}

// registerHTTP регистрирует пользователя через UIA (dummy) и возвращает токен+device.
func registerHTTP(t *testing.T, server *Server, localpart, deviceID string) (token string) {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/register",
		`{"username":"`+localpart+`","password":"1234567890"}`, "")
	// Первый шаг UIA всегда отвечает 401 с flows и session.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("register stage 1: %d %s", rec.Code, rec.Body.String())
	}

	var stage struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stage); err != nil {
		t.Fatalf("decode stage: %v", err)
	}

	body := `{"username":"` + localpart + `","password":"1234567890","device_id":"` + deviceID +
		`","auth":{"type":"m.login.dummy","session":"` + stage.Session + `"}}`

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/register", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register stage 2: %d %s", rec.Code, rec.Body.String())
	}

	var reg struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || reg.AccessToken == "" {
		t.Fatalf("decode register: %v %s", err, rec.Body.String())
	}

	return reg.AccessToken
}

// TestToDeviceEndToEnd — A отправляет ключ устройству B, B получает
// его в /sync ровно один раз; OTK-счётчики и /keys/changes отдают реальные данные.
func TestToDeviceEndToEnd(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	body := `{"messages":{"@bob:test.local":{"BOB":{"ciphertext":"hello-e2ee"}}}}`

	rec := do(t, server, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room.encrypted/txn-1", body, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("sendToDevice: %d %s", rec.Code, rec.Body.String())
	}

	// Повторный PUT с тем же txn — идемпотентен (не дублирует доставку).
	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room.encrypted/txn-1", body, alice); rec.Code != http.StatusOK {
		t.Fatalf("sendToDevice repeat: %d", rec.Code)
	}

	first := syncOnce(t, server, bob)
	if len(first.ToDevice.Events) != 1 {
		t.Fatalf("первый /sync: %d to-device событий, ожидали 1", len(first.ToDevice.Events))
	}

	if first.ToDevice.Events[0].Type != "m.room.encrypted" {
		t.Fatalf("тип события: %s", first.ToDevice.Events[0].Type)
	}

	second := syncOnce(t, server, bob)
	if len(second.ToDevice.Events) != 0 {
		t.Fatalf("второй /sync без since не должен дублировать to-device: %d", len(second.ToDevice.Events))
	}

	// OTK: загружаем один ключ — счётчик в /sync должен стать 1.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload",
		`{"device_keys":{"user_id":"@bob:test.local","device_id":"BOB"},"one_time_keys":{"signed_curve25519:AAA":{"key":"x"}}}`, bob)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/upload: %d %s", rec.Code, rec.Body.String())
	}

	// device_lists.changed - только в incremental sync, как в Synapse.
	afterKeys := syncWithSince(t, server, bob, second.NextBatch)
	if afterKeys.OTKCount["signed_curve25519"] != 1 {
		t.Fatalf("otk count = %v, ожидали 1", afterKeys.OTKCount)
	}

	if len(afterKeys.DeviceLists.Changed) == 0 {
		t.Fatalf("device_lists.changed пуст после загрузки ключей")
	}

	// /keys/changes видит изменение ключей.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/keys/changes", "", bob)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/changes: %d %s", rec.Code, rec.Body.String())
	}

	var changes keysChangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &changes); err != nil {
		t.Fatalf("decode keys/changes: %v", err)
	}

	if len(changes.Changed) == 0 {
		t.Fatalf("keys/changes: changed пуст")
	}

	// Мусор вместо кросс-ключей и поддельные подписи не принимаются (полный сценарий -
	// TestCrossSigningElementFlow): неизвестное поле - 400, подпись без ключей - отказ по ключу.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_keys":{"user_id":"@bob:test.local"}}`, bob); rec.Code != http.StatusBadRequest {
		t.Fatalf("device_signing/upload с мусором: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/signatures/upload",
		`{"@bob:test.local":{"BOB":{"ed25519:BOB":{"signature":"sig"}}}}`, bob); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "failures") || !strings.Contains(rec.Body.String(), "errcode") {
		t.Fatalf("signatures/upload с поддельной подписью: %d %s", rec.Code, rec.Body.String())
	}
}

// TestToDevicePagination - >100 сообщений в очереди не теряются.
// next_batch не уходит выше недоставленного to-device, поэтому остаток приходит следующим /sync.
func TestToDevicePagination(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	const total = 150

	for i := 0; i < total; i++ {
		body := `{"messages":{"@bob:test.local":{"BOB":{"n":"` + strconv.Itoa(i) + `"}}}}`

		txn := "txn-" + strconv.Itoa(i)
		if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room.encrypted/"+txn, body, alice); rec.Code != http.StatusOK {
			t.Fatalf("sendToDevice %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	delivered := 0

	for round := 0; round < 3; round++ {
		resp := syncOnce(t, server, bob)
		delivered += len(resp.ToDevice.Events)

		if delivered == total {
			break
		}
	}

	if delivered != total {
		t.Fatalf("доставлено %d из %d to-device сообщений — потери при пагинации", delivered, total)
	}

	// После полной доставки очередь пуста и новых сообщений нет.
	final := syncOnce(t, server, bob)
	if len(final.ToDevice.Events) != 0 {
		t.Fatalf("после полной доставки пришло ещё %d сообщений", len(final.ToDevice.Events))
	}
}

// syncOnce делает /sync без long-poll и возвращает разобранный ответ.
func syncOnce(t *testing.T, server *Server, token string) syncResponse {
	t.Helper()

	rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	var resp syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	return resp
}

// TestSyncToDeviceEmptyQueueThenSend - /sync по устройству
// с пустой очередью выдаёт токен, после этого приходит to-device сообщение, и
// следующий /sync С ЭТИМ токеном обязан его отдать.
//
// Плюс инвариант закладки: при пустой очереди она остаётся на `since` (0 у initial
// sync). Если её поднимать до свежей глобальной позиции, сообщение, вставленное
// между чтением очереди и чтением позиции, окажется «позади» закладки и не доедет
// никогда.
func TestSyncToDeviceEmptyQueueThenSend(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	// Нужна ненулевая глобальная позиция: иначе «поднять закладку до свежей
	// позиции» и «оставить на since» дают одно и то же число и дефект не виден.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"td"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	empty := syncOnce(t, server, bob)
	if len(empty.ToDevice.Events) != 0 {
		t.Fatalf("ожидали пустую очередь, получили %d событий", len(empty.ToDevice.Events))
	}

	if empty.NextBatch == "" {
		t.Fatal("next_batch пуст")
	}

	if _, toDevicePos, err := server.syncTokens.Decode2(empty.NextBatch); err != nil {
		t.Fatalf("decode next_batch: %v", err)
	} else if toDevicePos != 0 {
		t.Fatalf("закладка to-device в токене пустой очереди = %d, ожидали 0", toDevicePos)
	}

	body := `{"messages":{"@bob:test.local":{"BOB":{"ciphertext":"late-key"}}}}`

	rec := do(t, server, http.MethodPut, "/_matrix/client/v3/sendToDevice/m.room.encrypted/txn-late", body, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("sendToDevice: %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet,
		"/_matrix/client/v3/sync?timeout=0&since="+url.QueryEscape(empty.NextBatch), http.NoBody)
	req.Header.Set("Authorization", "Bearer "+bob)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("sync since: %d %s", rec.Code, rec.Body.String())
	}

	var next syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &next); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	if len(next.ToDevice.Events) != 1 {
		t.Fatalf("ожидали 1 to-device событие после отправки, получили %d", len(next.ToDevice.Events))
	}

	third := syncOnce(t, server, bob)
	if len(third.ToDevice.Events) != 0 {
		t.Fatalf("повторная доставка: %d событий", len(third.ToDevice.Events))
	}
}

// syncAll — /sync с токеном, возвращает разобранный ответ и новый токен.
func syncWithSince(t *testing.T, server *Server, token, since string) syncResponse {
	t.Helper()

	path := "/_matrix/client/v3/sync?timeout=0"
	if since != "" {
		path += "&since=" + url.QueryEscape(since)
	}

	rec := do(t, server, http.MethodGet, path, "", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	var resp syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode sync: %v", err)
	}

	return resp
}

// TestLeftUserStopsReceivingEvents — после выхода (и после бана) клиент
// получает по комнате только событие выхода и один раз; дальше комната молчит.
// До правки leave-комната попадала в rooms.leave на каждом инкрементальном /sync
// и вышедший читал переписку бессрочно.
func TestLeftUserStopsReceivingEvents(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","name":"leave-test"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode createRoom: %v %s", err, rec.Body.String())
	}

	roomID := url.PathEscape(created.RoomID)

	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	// Базовая позиция: bob видит комнату как joined.
	base := syncWithSince(t, server, bob, "")
	if _, ok := base.Rooms.Join[created.RoomID]; !ok {
		t.Fatalf("после join комната не в rooms.join: %+v", base.Rooms.Join)
	}

	// bob выходит, затем alice пишет три сообщения.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rec.Code, rec.Body.String())
	}

	for i := 0; i < 3; i++ {
		body := `{"msgtype":"m.text","body":"after-leave"}`
		rec := do(t, server, http.MethodPut,
			"/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/txn-leave-"+strconv.Itoa(i), body, alice)
		if rec.Code != http.StatusOK {
			t.Fatalf("send %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	first := syncWithSince(t, server, bob, base.NextBatch)

	left, ok := first.Rooms.Leave[created.RoomID]
	if !ok {
		t.Fatalf("комната не в rooms.leave после выхода: %+v", first.Rooms.Leave)
	}

	if len(left.Timeline.Events) != 1 || left.Timeline.Events[0].Type != "m.room.member" {
		t.Fatalf("в rooms.leave ожидали только событие выхода, получили %d: %+v",
			len(left.Timeline.Events), left.Timeline.Events)
	}

	// Следующий sync: комнаты быть не должно вообще.
	second := syncWithSince(t, server, bob, first.NextBatch)
	if _, ok := second.Rooms.Leave[created.RoomID]; ok {
		t.Fatal("комната повторно в rooms.leave — вышедший продолжает получать комнату")
	}

	if _, ok := second.Rooms.Join[created.RoomID]; ok {
		t.Fatal("комната в rooms.join у вышедшего пользователя")
	}
}

// TestBannedUserSeesBanAsLeave — забаненный узнаёт о бане: комната
// приходит в rooms.leave один раз, дальше сообщения alice ему не доставляются.
func TestBannedUserSeesBanAsLeave(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "alice", "ALICE")
	bob := registerHTTP(t, server, "bob", "BOB")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"public_chat","name":"ban-test"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
		t.Fatalf("decode createRoom: %v %s", err, rec.Body.String())
	}

	roomPath := "/_matrix/client/v3/rooms/" + url.PathEscape(created.RoomID)

	if rec := do(t, server, http.MethodPost, roomPath+"/join", `{}`, bob); rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	base := syncWithSince(t, server, bob, "")

	if rec := do(t, server, http.MethodPost, roomPath+"/ban",
		`{"user_id":"@bob:test.local","reason":"spam"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPut, roomPath+"/send/m.room.message/txn-ban-1",
		`{"msgtype":"m.text","body":"after-ban"}`, alice); rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}

	first := syncWithSince(t, server, bob, base.NextBatch)

	left, ok := first.Rooms.Leave[created.RoomID]
	if !ok {
		t.Fatalf("ban не пришёл как покинутая комната: join=%v leave=%v",
			first.Rooms.Join, first.Rooms.Leave)
	}

	if len(left.Timeline.Events) != 1 || left.Timeline.Events[0].Type != "m.room.member" {
		t.Fatalf("ожидали одно событие бана, получили %d", len(left.Timeline.Events))
	}

	second := syncWithSince(t, server, bob, first.NextBatch)
	if _, ok := second.Rooms.Leave[created.RoomID]; ok {
		t.Fatal("комната повторно в rooms.leave после бана")
	}
}
