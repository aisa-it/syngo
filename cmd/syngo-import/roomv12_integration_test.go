//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/synapseexport"
	"github.com/aisa-it/syngo/internal/testdb"
)

// synapseV12Fixture - живой Synapse 1.165 с комнатами версии 12 (room_id без домена):
// комната версии по умолчанию с дополнительным создателем и комната v11, замененная
// на v12 (tools/synapse-export/fixture/gen.sh, SCENARIO=v12).
const synapseV12Fixture = "testdata/synapse-1.165-v12.sql"

// TestImportSynapseRoomsV12 - комнаты v12 переносятся из живого Synapse
// (export.sql, syngo-synapse-export, syngo-import -server-name), event_id сходятся с
// reference hash PDU, сверка с Synapse проходит; после переезда Element работает с
// ними через /sync и /messages, создатели из m.room.create сохраняют права (их нет
// в power_levels), новые события не ссылаются на create в auth_events; комната v12
// создается и на SYNGO. Раньше импорт отбрасывал комнаты без домена в room_id
// (фильтр -server-name и CHECK rooms_room_id_format), а их события - вслед за ними.
func TestImportSynapseRoomsV12(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	ctx := context.Background()

	db, err := store.Open(ctx, dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	loadDumpFixture(t, db.Pool(), synapseV12Fixture)

	var roomV12, roomUpgraded, roomV11 string
	if err := db.Pool().QueryRow(ctx, `SELECT
		(SELECT room_id FROM synapse.room_aliases WHERE room_alias = '#v12:fixture.local'),
		(SELECT room_id FROM synapse.rooms WHERE room_version = '12'
		   AND room_id <> (SELECT room_id FROM synapse.room_aliases WHERE room_alias = '#v12:fixture.local')),
		(SELECT room_id FROM synapse.rooms WHERE room_version = '11')`).Scan(&roomV12, &roomUpgraded, &roomV11); err != nil {
		t.Fatalf("комнаты фикстуры: %v", err)
	}

	if strings.Contains(roomV12, ":") || strings.Contains(roomUpgraded, ":") {
		t.Fatalf("фикстура без комнат v12: %s %s", roomV12, roomUpgraded)
	}

	dump := exportSynapse(t, db.Pool())

	im := newImporter(db, fixtureServer, 1000)
	if err := im.importFile(ctx, dump, 0); err != nil {
		t.Fatalf("импорт: %v", err)
	}

	if len(im.errors) > 0 || im.counts.Rooms != 3 {
		t.Fatalf("импорт: комнат %d, ошибки %+v", im.counts.Rooms, im.errors)
	}

	verifyV12AgainstSynapse(t, dsn, db)

	cfg := config.Default()
	cfg.Matrix.ServerName = fixtureServer
	cfg.RateLimit = config.RateLimit{}
	cfg.Sync.TokenSecret = "integration-secret"
	cfg.Media.Directory = t.TempDir()

	c := client{t: t, handler: api.New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()}

	aliceTok := sourceToken(t, db.Pool(), "ALICEDEV")
	bobTok := sourceToken(t, db.Pool(), "BOBDEV")
	carolTok := sourceToken(t, db.Pool(), "CAROLDEV")

	// Element: /sync отдает комнаты v12 под их room_id с историей.
	sync := c.ok(http.MethodGet, "/_matrix/client/v3/sync?timeout=0", aliceTok, "")
	joined, _ := dig(sync, "rooms", "join").(map[string]any)

	for _, id := range []string{roomV12, roomUpgraded, roomV11} {
		if joined[id] == nil {
			t.Fatalf("/sync: нет комнаты %s: %v", id, keysOf(joined))
		}
	}

	timeline, _ := json.Marshal(dig(joined[roomV12], "timeline"))
	if !strings.Contains(string(timeline), "после модерации") {
		t.Fatalf("/sync: история v12 без сообщений: %s", timeline)
	}

	// /messages назад от конца: вся история, удаленное сообщение без текста.
	room := url.PathEscape(roomV12)
	messages, _ := json.Marshal(c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/messages?dir=b&limit=100", bobTok, ""))

	for _, want := range []string{"привет из v12", "ответ дополнительного создателя", `"type":"m.room.create"`, `"additional_creators"`} {
		if !strings.Contains(string(messages), want) {
			t.Fatalf("/messages: нет %s: %s", want, messages)
		}
	}

	if strings.Contains(string(messages), "лишнее сообщение carol") {
		t.Fatalf("/messages: текст удаленного сообщения виден: %s", messages)
	}

	// Создатель не записан в power_levels (уровень бесконечный): name требует 50.
	c.ok(http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/state/m.room.name", aliceTok, `{"name":"после переезда"}`)

	// Новое событие не ссылается на create в auth_events (правило v12).
	var authRefs []string
	if err := db.Pool().QueryRow(ctx, `SELECT auth_events FROM events
		WHERE room_id = $1 AND type = 'm.room.name' ORDER BY stream_ordering DESC LIMIT 1`, roomV12).Scan(&authRefs); err != nil {
		t.Fatal(err)
	}

	if len(authRefs) == 0 || containsString(authRefs, "$"+roomV12[1:]) {
		t.Fatalf("auth_events нового события: %v", authRefs)
	}

	// Создателя нельзя вписать в power_levels.users (правило v12).
	levels := c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/state/m.room.power_levels", aliceTok, "")
	users, _ := levels["users"].(map[string]any)

	if users["@carol:fixture.local"] != float64(50) || users[alice] != nil {
		t.Fatalf("power_levels из Synapse: %v", levels)
	}

	users[alice] = 100
	withCreator, _ := json.Marshal(levels)

	if code, body := c.do(http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/state/m.room.power_levels", aliceTok, string(withCreator)); code != http.StatusForbidden {
		t.Fatalf("создатель в power_levels.users: %d %v", code, body)
	}

	// Tombstone требует 150 - выше любого уровня, кроме создателей.
	if code, body := c.do(http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/state/m.room.tombstone", carolTok,
		`{"body":"x","replacement_room":"!x"}`); code != http.StatusForbidden {
		t.Fatalf("tombstone от carol (50): %d %v", code, body)
	}

	// Дополнительный создатель (bob) выше carol, carol не может выгнать создателя.
	kickBob := `{"user_id":"@bob:fixture.local","reason":"x"}`
	if code, body := c.do(http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/kick", carolTok, kickBob); code != http.StatusForbidden {
		t.Fatalf("carol выгоняет создателя: %d %v", code, body)
	}

	c.ok(http.MethodPost, "/_matrix/client/v3/rooms/"+room+"/kick", bobTok, `{"user_id":"@carol:fixture.local"}`)

	// Замененная комната: Element находит замену по tombstone старой комнаты.
	tombstone := c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(roomV11)+"/state/m.room.tombstone", aliceTok, "")
	if tombstone["replacement_room"] != roomUpgraded {
		t.Fatalf("tombstone v11: %v", tombstone)
	}

	c.ok(http.MethodPut, "/_matrix/client/v3/rooms/"+url.PathEscape(roomUpgraded)+"/send/m.room.message/n1", bobTok,
		`{"msgtype":"m.text","body":"на SYNGO"}`)

	// Комната v12, созданная на SYNGO: room_id - хеш m.room.create, создатель вне users.
	created := c.ok(http.MethodPost, "/_matrix/client/v3/createRoom", aliceTok,
		`{"room_version":"12","name":"новая v12","invite":["@bob:fixture.local"]}`)

	newRoom, _ := created["room_id"].(string)
	if !strings.HasPrefix(newRoom, "!") || strings.Contains(newRoom, ":") {
		t.Fatalf("createRoom v12: room_id %q", newRoom)
	}

	var createID string
	if err := db.Pool().QueryRow(ctx, `SELECT event_id FROM events WHERE room_id = $1 AND type = 'm.room.create'`, newRoom).Scan(&createID); err != nil {
		t.Fatal(err)
	}

	if createID != "$"+newRoom[1:] {
		t.Fatalf("room_id %s не хеш create %s", newRoom, createID)
	}

	escaped := url.PathEscape(newRoom)
	newLevels := c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+escaped+"/state/m.room.power_levels", aliceTok, "")

	if dig(newLevels, "users", alice) != nil || dig(newLevels, "events", "m.room.tombstone") != float64(150) {
		t.Fatalf("power_levels новой комнаты v12: %v", newLevels)
	}

	c.ok(http.MethodPost, "/_matrix/client/v3/join/"+escaped, bobTok, "{}")
	c.ok(http.MethodPut, "/_matrix/client/v3/rooms/"+escaped+"/send/m.room.message/n2", bobTok, `{"msgtype":"m.text","body":"в новой v12"}`)

	newMessages, _ := json.Marshal(c.ok(http.MethodGet, "/_matrix/client/v3/rooms/"+escaped+"/messages?dir=b&limit=50", aliceTok, ""))
	if !strings.Contains(string(newMessages), "в новой v12") {
		t.Fatalf("/messages новой v12: %s", newMessages)
	}

	again := c.ok(http.MethodGet, "/_matrix/client/v3/sync?timeout=0", aliceTok, "")
	if dig(again, "rooms", "join", newRoom) == nil {
		t.Fatalf("/sync: нет новой комнаты v12: %v", keysOf(dig(again, "rooms", "join")))
	}
}

// verifyV12AgainstSynapse - syngo-verify-migration против схемы synapse фикстуры.
func verifyV12AgainstSynapse(t *testing.T, dsn string, db *store.DB) {
	t.Helper()

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	src, err := pgxpool.New(context.Background(), dsn+sep+"search_path=synapse")
	if err != nil {
		t.Fatalf("пул Synapse: %v", err)
	}

	defer src.Close()

	report, err := synapseexport.Verify(context.Background(), src, db.Pool(),
		synapseexport.VerifyOptions{ServerName: fixtureServer, SkipMedia: true})
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}

	if !report.OK() || report.RoomsChecked != 3 || report.Aliases == 0 || report.Redacted == 0 {
		t.Fatalf("сверка с Synapse:\n%s", report.Text())
	}
}

func keysOf(value any) []string {
	obj, _ := value.(map[string]any)

	out := make([]string, 0, len(obj))
	for key := range obj {
		out = append(out, key)
	}

	return out
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}

	return false
}
