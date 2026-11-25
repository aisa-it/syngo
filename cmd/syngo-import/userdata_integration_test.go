//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/synapseexport"
	"github.com/aisa-it/syngo/internal/testdb"
)

// synapseFixture - таблицы живого Synapse 1.165 (tools/synapse-export/fixture/gen.sh):
// пароль alice с лигатурой при password_config.pepper, два устройства с ключами,
// кросс-подписи, бэкап ключей, account data, push rules, pusher, фильтр, to-device,
// пользователи admin/deactivated/locked/shadow_banned и без пароля.
const (
	synapseFixture = "testdata/synapse-1.165-users.sql"
	fixtureServer  = "fixture.local"
	fixturePepper  = "fixture-pepper"
	alicePassword  = "ﬁxture-Пароль-1"
	alice          = "@alice:fixture.local"
	bob            = "@bob:fixture.local"
)

// loadUsersFixture загружает дамп pg_dump в схему synapse той же тестовой базы.
func loadUsersFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	loadDumpFixture(t, pool, synapseFixture)
}

// loadDumpFixture загружает фикстуру gen.sh (pg_dump схемы synapse) в ту же
// тестовую базу; схема стирается после теста.
func loadDumpFixture(t *testing.T, pool *pgxpool.Pool, path string) {
	t.Helper()

	ctx := context.Background()

	dropSchema := func() { _, _ = pool.Exec(ctx, `DROP SCHEMA IF EXISTS synapse CASCADE`) }
	dropSchema()
	t.Cleanup(dropSchema)

	raw, err := os.ReadFile(path) //nolint:gosec // файл фикстуры
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Release()

	pg := conn.Conn().PgConn()

	var sqlBuf strings.Builder

	flush := func() {
		if strings.TrimSpace(sqlBuf.String()) == "" {
			return
		}

		if _, err := pg.Exec(ctx, sqlBuf.String()).ReadAll(); err != nil {
			t.Fatalf("фикстура SQL: %v", err)
		}

		sqlBuf.Reset()
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "COPY ") && strings.HasSuffix(line, "FROM stdin;") {
			flush()

			var data strings.Builder

			for scanner.Scan() && scanner.Text() != `\.` {
				data.WriteString(scanner.Text())
				data.WriteByte('\n')
			}

			if _, err := pg.CopyFrom(ctx, strings.NewReader(data.String()), strings.TrimSuffix(line, ";")); err != nil {
				t.Fatalf("фикстура %s: %v", line, err)
			}

			continue
		}

		sqlBuf.WriteString(line)
		sqlBuf.WriteByte('\n')
	}

	flush()

	// Фикстуры сняты со старой схемы без user_external_ids и
	// receipts_linearized; в живом Synapse таблицы есть всегда, export.sql их читает.
	// DDL - по схеме Synapse.
	if _, err := pg.Exec(ctx, `CREATE TABLE IF NOT EXISTS synapse.user_external_ids (
		auth_provider text NOT NULL, external_id text NOT NULL, user_id text NOT NULL,
		UNIQUE (auth_provider, external_id));
		CREATE TABLE IF NOT EXISTS synapse.receipts_linearized (
		stream_id bigint NOT NULL, room_id text NOT NULL, receipt_type text NOT NULL,
		user_id text NOT NULL, event_id text NOT NULL, data text NOT NULL, instance_name text,
		event_stream_ordering bigint, thread_id text,
		UNIQUE (room_id, receipt_type, user_id, thread_id))`).ReadAll(); err != nil {
		t.Fatal(err)
	}

	// pg_dump ставит пустой search_path; соединение вернется в пул.
	if _, err := pg.Exec(ctx, `RESET search_path`).ReadAll(); err != nil {
		t.Fatal(err)
	}
}

// exportSynapse собирает дамп как runbook миграции: tools/synapse-export/export.sql
// над схемой synapse (как psql -At), события - syngo-synapse-export
// (synapseexport.ExportSnapshot), членство - после событий (FK на events).
func exportSynapse(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	ctx := context.Background()

	script, err := os.ReadFile("../../tools/synapse-export/export.sql")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}

	defer conn.Release()

	if _, err := conn.Exec(ctx, "SET search_path TO synapse"); err != nil {
		t.Fatal(err)
	}

	defer func() { _, _ = conn.Exec(context.Background(), "RESET search_path") }()

	results, err := conn.Conn().PgConn().Exec(ctx, string(script)).ReadAll()
	if err != nil {
		t.Fatalf("export.sql: %v", err)
	}

	var head, membership bytes.Buffer

	for _, result := range results {
		for _, row := range result.Rows {
			target := &head
			if bytes.Contains(row[0], []byte(`"kind":"membership"`)) {
				target = &membership
			}

			target.Write(row[0])
			target.WriteByte('\n')
		}
	}

	if _, err := synapseexport.ExportSnapshot(ctx, conn.Conn(), &head, synapseexport.Options{BatchSize: 100}); err != nil {
		t.Fatalf("выгрузка событий: %v", err)
	}

	path := filepath.Join(t.TempDir(), "synapse-dump.jsonl")
	if err := os.WriteFile(path, append(head.Bytes(), membership.Bytes()...), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// client - HTTP-клиент к api.Server в тесте.
type client struct {
	t       *testing.T
	handler http.Handler
}

func (c client) do(method, path, token, body string) (int, map[string]any) {
	c.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return rec.Code, out
}

func (c client) ok(method, path, token, body string) map[string]any {
	c.t.Helper()

	code, out := c.do(method, path, token, body)
	if code != http.StatusOK {
		c.t.Fatalf("%s %s = %d %v", method, path, code, out)
	}

	return out
}

// dig достает значение по пути ключей.
func dig(value any, keys ...string) any {
	for _, key := range keys {
		obj, _ := value.(map[string]any)
		value = obj[key]
	}

	return value
}

// sourceToken - открытый токен устройства из таблицы access_tokens Synapse.
func sourceToken(t *testing.T, pool *pgxpool.Pool, deviceID string) string {
	t.Helper()

	var token string
	if err := pool.QueryRow(context.Background(),
		`SELECT token FROM synapse.access_tokens WHERE device_id = $1 ORDER BY id DESC LIMIT 1`, deviceID).Scan(&token); err != nil {
		t.Fatalf("токен %s: %v", deviceID, err)
	}

	return token
}

// TestImportSynapseUsers - после экспорта живого Synapse и
// импорта клиент со старым токеном получает /sync, /keys/query отдает перенесенные
// ключи и кросс-подписи, /room_keys/version - перенесенный бэкап; настройки клиента
// на месте; повторный импорт идемпотентен, сверка с источником без расхождений.
func TestImportSynapseUsers(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	ctx := context.Background()

	db, err := store.Open(ctx, dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	loadUsersFixture(t, db.Pool())

	dump := exportSynapse(t, db.Pool())

	for run := 1; run <= 2; run++ {
		im := newImporter(db, fixtureServer, 1000)
		if err := im.importFile(ctx, dump, 0); err != nil {
			t.Fatalf("импорт %d: %v", run, err)
		}

		if len(im.errors) > 0 {
			t.Fatalf("импорт %d: ошибки %+v", run, im.errors)
		}

		if run == 1 && (im.counts.Devices == 0 || im.counts.AccessTokens == 0 || im.counts.RoomKeys != 2 || im.counts.LockedAsDeactivated != 1) {
			t.Fatalf("импорт: счетчики %+v", im.counts)
		}

		report, err := verifyUsers(ctx, db, im.userStats)
		if err != nil {
			t.Fatalf("сверка: %v", err)
		}

		if len(report.Mismatches) > 0 || report.Users != 7 || report.Checked["access_tokens"] == 0 {
			t.Fatalf("сверка после импорта %d: %+v", run, report)
		}
	}

	cfg := config.Default()
	cfg.Matrix.ServerName = fixtureServer
	cfg.Auth.RefreshTokens = true
	cfg.Auth.PasswordPepper = fixturePepper
	cfg.RateLimit = config.RateLimit{}
	cfg.Sync.TokenSecret = "integration-secret"
	cfg.Media.Directory = t.TempDir()

	c := client{t: t, handler: api.New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()}

	laptop := sourceToken(t, db.Pool(), "ALICELAPTOP")
	bobToken := sourceToken(t, db.Pool(), "BOBDEV")

	// Старый токен Synapse действует: та же сессия и устройство.
	who := c.ok(http.MethodGet, "/_matrix/client/v3/account/whoami", laptop, "")
	if who["user_id"] != alice || who["device_id"] != "ALICELAPTOP" {
		t.Fatalf("whoami = %v", who)
	}

	// /sync со старым since от Synapse - initial sync без событий timeline
	// (разрыв): комната, account data; история - через /messages.
	sync := c.ok(http.MethodGet, "/_matrix/client/v3/sync?since=s21_7_0_1_5_1_3_7_0_1&timeout=0", laptop, "")
	rooms, _ := dig(sync, "rooms", "join").(map[string]any)

	if len(rooms) != 1 {
		t.Fatalf("/sync: комнаты %v", dig(sync, "rooms"))
	}

	var roomID string
	for id := range rooms {
		roomID = id
	}

	timeline, _ := json.Marshal(dig(rooms[roomID], "timeline"))
	if strings.Contains(string(timeline), "до переезда") || dig(rooms[roomID], "timeline", "limited") != true {
		t.Fatalf("/sync: на since Synapse timeline не пуст или не limited: %s", timeline)
	}

	prevBatch, _ := dig(rooms[roomID], "timeline", "prev_batch").(string)
	history := c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+
		"/messages?dir=b&limit=50&from="+url.QueryEscape(prevBatch), laptop, "")

	if chunk, _ := json.Marshal(history["chunk"]); !strings.Contains(string(chunk), "до переезда") ||
		!strings.Contains(string(chunk), "m.room.encrypted") {
		t.Fatalf("/messages от prev_batch: история без перенесенных сообщений: %s", chunk)
	}

	accountData, _ := json.Marshal(dig(sync, "account_data"))
	for _, typ := range []string{"m.secret_storage.default_key", "m.cross_signing.master", "m.megolm_backup.v1", "m.direct"} {
		if !strings.Contains(string(accountData), typ) {
			t.Fatalf("/sync: нет account data %s: %s", typ, accountData)
		}
	}

	roomData, _ := json.Marshal(dig(rooms[roomID], "account_data"))
	if !strings.Contains(string(roomData), "m.favourite") || !strings.Contains(string(roomData), "org.example.room.note") {
		t.Fatalf("/sync: нет комнатной account data и тегов: %s", roomData)
	}

	// /keys/query (от bob): устройства alice, кросс-подписи и подписи.
	keys := c.ok(http.MethodPost, "/_matrix/client/v3/keys/query", bobToken, `{"device_keys":{"`+alice+`":[]}}`)

	devices, _ := dig(keys, "device_keys", alice).(map[string]any)
	if len(devices) != 2 || devices["ALICEPHONE"] == nil || devices["ALICELAPTOP"] == nil {
		t.Fatalf("/keys/query: устройства %v", dig(keys, "device_keys"))
	}

	var sskPub, masterPub string

	if err := db.Pool().QueryRow(ctx, `SELECT
		(SELECT key_id FROM e2e_cross_signing_keys WHERE user_id = $1 AND key_type = 'self_signing'),
		(SELECT key_id FROM e2e_cross_signing_keys WHERE user_id = $1 AND key_type = 'master')`, alice).
		Scan(&sskPub, &masterPub); err != nil {
		t.Fatal(err)
	}

	if dig(devices["ALICELAPTOP"], "signatures", alice, "ed25519:"+sskPub) == nil {
		t.Fatalf("/keys/query: ALICELAPTOP без подписи self_signing: %v", devices["ALICELAPTOP"])
	}

	if dig(keys, "master_keys", alice, "keys", "ed25519:"+masterPub) == nil ||
		dig(keys, "self_signing_keys", alice) == nil {
		t.Fatalf("/keys/query: нет кросс-подписи alice: %v", keys)
	}

	if dig(keys, "master_keys", alice, "signatures", alice, "ed25519:ALICELAPTOP") == nil {
		t.Fatalf("/keys/query: мастер-ключ без подписи устройства: %v", dig(keys, "master_keys"))
	}

	// Свой user_signing и подпись мастер-ключа bob видны alice.
	own := c.ok(http.MethodPost, "/_matrix/client/v3/keys/query", laptop,
		`{"device_keys":{"`+alice+`":[],"`+bob+`":[]}}`)
	if dig(own, "user_signing_keys", alice) == nil {
		t.Fatalf("/keys/query: нет user_signing alice: %v", own)
	}

	bobMaster, _ := json.Marshal(dig(own, "master_keys", bob, "signatures", alice))
	if string(bobMaster) == "null" {
		t.Fatalf("/keys/query: мастер-ключ bob без подписи alice: %v", dig(own, "master_keys", bob))
	}

	// One-time keys: выданный в Synapse не возвращается, после них - fallback.
	for i, want := range []string{"OTK", "OTK", "FB"} {
		claim := c.ok(http.MethodPost, "/_matrix/client/v3/keys/claim", bobToken,
			`{"one_time_keys":{"`+alice+`":{"ALICELAPTOP":"signed_curve25519"}}}`)

		got, _ := dig(claim, "one_time_keys", alice, "ALICELAPTOP").(map[string]any)

		var keyID string
		for id := range got {
			keyID = id
		}

		if !strings.Contains(keyID, "ALICELAPTOP"+want) || keyID == "signed_curve25519:ALICELAPTOPOTK0" {
			t.Fatalf("/keys/claim %d: %v, ожидали %s", i, got, want)
		}
	}

	// Бэкап ключей: текущая версия 2 (версия 1 удалена в Synapse), два ключа.
	backup := c.ok(http.MethodGet, "/_matrix/client/v3/room_keys/version", laptop, "")
	if backup["version"] != "2" || backup["algorithm"] != "m.megolm_backup.v1.curve25519-aes-sha2" ||
		backup["count"] != float64(2) || backup["etag"] != "1" || dig(backup, "auth_data", "public_key") == nil {
		t.Fatalf("/room_keys/version = %v", backup)
	}

	sessions := c.ok(http.MethodGet, "/_matrix/client/v3/room_keys/keys?version=2", laptop, "")
	if dig(sessions, "rooms", roomID, "sessions", "session0", "session_data", "ciphertext") != "ct0" ||
		dig(sessions, "rooms", roomID, "sessions", "session1", "first_message_index") != float64(1) {
		t.Fatalf("/room_keys/keys = %v", sessions)
	}

	if code, _ := c.do(http.MethodGet, "/_matrix/client/v3/room_keys/version/1", laptop, ""); code != http.StatusNotFound {
		t.Fatalf("удаленная версия 1: %d", code)
	}

	// Push rules: порядок и флаги своих правил, переопределения серверных.
	rules := c.ok(http.MethodGet, "/_matrix/client/v3/pushrules/", laptop, "")

	override, _ := dig(rules, "global", "override").([]any)

	var order []string

	for _, rule := range override {
		id, _ := dig(rule, "rule_id").(string)
		order = append(order, id)

		if id == "first" && dig(rule, "enabled") != false || id == ".m.rule.invite_for_me" && dig(rule, "enabled") != false {
			t.Fatalf("push rule %s: enabled = %v", id, dig(rule, "enabled"))
		}
	}

	if !strings.Contains(strings.Join(order, ","), "first,second") {
		t.Fatalf("override: порядок %v, ожидали first перед second", order)
	}

	ruleJSON, _ := json.Marshal(rules)
	if !strings.Contains(string(ruleJSON), `"pattern":"coffee"`) || !strings.Contains(string(ruleJSON), roomID) {
		t.Fatalf("push rules: нет content/room правил: %s", ruleJSON)
	}

	message := c.ok(http.MethodGet, "/_matrix/client/v3/pushrules/global/underride/.m.rule.message/actions", laptop, "")
	if actions, _ := message["actions"].([]any); len(actions) != 0 {
		t.Fatalf(".m.rule.message actions = %v, ожидали []", message)
	}

	// Pusher, фильтр, профиль, теги.
	pushers, _ := c.ok(http.MethodGet, "/_matrix/client/v3/pushers", laptop, "")["pushers"].([]any)
	if len(pushers) != 1 || dig(pushers[0], "pushkey") != "pushkey-alice-1" {
		t.Fatalf("pushers = %v", pushers)
	}

	if filter := c.ok(http.MethodGet, "/_matrix/client/v3/user/"+alice+"/filter/0", laptop, ""); dig(filter, "room", "timeline", "limit") != float64(7) {
		t.Fatalf("filter 0 = %v", filter)
	}

	profile := c.ok(http.MethodGet, "/_matrix/client/v3/profile/"+alice, laptop, "")
	if profile["displayname"] != "Алиса" || profile["avatar_url"] != "mxc://fixture.local/avatar1" {
		t.Fatalf("profile = %v", profile)
	}

	tags := c.ok(http.MethodGet, "/_matrix/client/v3/user/"+alice+"/rooms/"+roomID+"/tags", laptop, "")
	if dig(tags, "tags", "m.favourite", "order") != 0.5 || dig(tags, "tags", "u.work") == nil {
		t.Fatalf("tags = %v", tags)
	}

	// Устройства с именами.
	deviceList, _ := json.Marshal(c.ok(http.MethodGet, "/_matrix/client/v3/devices", laptop, ""))
	if !strings.Contains(string(deviceList), "Alice laptop") || !strings.Contains(string(deviceList), "Alice phone") {
		t.Fatalf("devices = %s", deviceList)
	}

	// ALICEPHONE: у access-токена срок жизни Synapse (refreshable_access_token_lifetime
	// 5m) сохранен; клиент обновляет токен перенесенным refresh-токеном и получает
	// недоставленное to-device.
	var expiresMatch bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT t.expires_at = to_timestamp(s.valid_until_ms / 1000.0)
		FROM access_tokens t
		JOIN synapse.access_tokens s ON t.token_hash = sha256(convert_to(s.token, 'UTF8'))
		WHERE s.device_id = 'ALICEPHONE'`).Scan(&expiresMatch); err != nil || !expiresMatch {
		t.Fatalf("срок жизни токена ALICEPHONE не перенесен: %v, %v", expiresMatch, err)
	}

	var refreshToken string
	if err := db.Pool().QueryRow(ctx, `SELECT token FROM synapse.refresh_tokens WHERE device_id = 'ALICEPHONE'`).Scan(&refreshToken); err != nil {
		t.Fatal(err)
	}

	refreshed := c.ok(http.MethodPost, "/_matrix/client/v3/refresh", "", `{"refresh_token":"`+refreshToken+`"}`)

	newPhone, _ := refreshed["access_token"].(string)
	phoneSync, _ := json.Marshal(c.ok(http.MethodGet, "/_matrix/client/v3/sync?timeout=0", newPhone, ""))

	if !strings.Contains(string(phoneSync), "m.room_key_request") {
		t.Fatalf("ALICEPHONE: нет недоставленного to-device: %s", phoneSync)
	}

	// Флаги: admin, deactivated, locked -> deactivated.
	var carolAdmin, daveOff, eveOff bool
	if err := db.Pool().QueryRow(ctx, `SELECT
		(SELECT is_admin FROM users WHERE user_id = '@carol:fixture.local'),
		(SELECT deactivated FROM users WHERE user_id = '@dave:fixture.local'),
		(SELECT deactivated FROM users WHERE user_id = '@eve:fixture.local')`).Scan(&carolAdmin, &daveOff, &eveOff); err != nil {
		t.Fatal(err)
	}

	if !carolAdmin || !daveOff || !eveOff {
		t.Fatalf("флаги: carol admin %v, dave deactivated %v, eve (locked) deactivated %v", carolAdmin, daveOff, eveOff)
	}

	// Пароль Synapse с pepper и NFKC: вход в любой NFKC-форме, затем
	// хеш переписывается в argon2id.
	for _, password := range []string{alicePassword, "fixture-Пароль-1"} {
		c.ok(http.MethodPost, "/_matrix/client/v3/login", "",
			`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},"password":"`+password+`"}`)
	}

	var hash string
	if err := db.Pool().QueryRow(ctx, `SELECT password_hash FROM users WHERE user_id = $1`, alice).Scan(&hash); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("хеш alice после входа = %.12s, ожидали argon2id", hash)
	}

	// Пользователь без пароля (LDAP/SSO в Synapse): 403 M_FORBIDDEN, как неверный пароль.
	code, body := c.do(http.MethodPost, "/_matrix/client/v3/login", "",
		`{"type":"m.login.password","identifier":{"type":"m.id.user","user":"ldapuser"},"password":"x"}`)
	if code != http.StatusForbidden || body["errcode"] != "M_FORBIDDEN" {
		t.Fatalf("вход без пароля: %d %v", code, body)
	}
}
