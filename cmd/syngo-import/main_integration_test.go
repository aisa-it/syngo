//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// migrateSchema накатывает миграции на тестовую БД.
func migrateSchema(t *testing.T, dsn string) {
	t.Helper()

	migrator, err := migrate.New("file://../../migrations", "pgx5://"+stripScheme(dsn))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})
}

// stripScheme убирает postgres:// для драйвера pgx5 golang-migrate.
func stripScheme(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(dsn) > len(prefix) && dsn[:len(prefix)] == prefix {
			return dsn[len(prefix):]
		}
	}

	return dsn
}

// TestImportFixture — комната из дампа читается с историей и состоянием.
func TestImportFixture(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	db, err := store.Open(context.Background(), dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	now := time.Now().UnixMilli()

	dump := []dumpRecord{
		{Kind: "user", UserID: "@alice:test.local", Localpart: "alice", Password: "$2b$12$hash"},
		{Kind: "room", RoomID: "!imported:test.local", RoomVersion: "10", Creator: "@alice:test.local"},
		{
			Kind: "event", RoomID: "!imported:test.local", EventID: "$e1", Type: "m.room.create",
			Sender: "@alice:test.local", StateKey: strPtr(""), Content: json.RawMessage(`{"creator":"@alice:test.local"}`),
			OriginTS: now, Depth: 1, Stream: int64Ptr(101),
		},
		{
			Kind: "event", RoomID: "!imported:test.local", EventID: "$e2", Type: "m.room.member",
			Sender: "@alice:test.local", StateKey: strPtr("@alice:test.local"), Content: json.RawMessage(`{"membership":"join"}`),
			OriginTS: now + 1, Depth: 2, Stream: int64Ptr(102), PrevEvents: []string{"$e1"},
		},
		{
			Kind: "event", RoomID: "!imported:test.local", EventID: "$e3", Type: "m.room.message",
			Sender: "@alice:test.local", Content: json.RawMessage(`{"body":"первое"}`),
			OriginTS: now + 2, Depth: 3, Stream: int64Ptr(103), PrevEvents: []string{"$e2"},
		},
		{
			Kind: "event", RoomID: "!imported:test.local", EventID: "$e4", Type: "m.room.message",
			Sender: "@alice:test.local", Content: json.RawMessage(`{"body":"второе"}`),
			OriginTS: now + 3, Depth: 4, Stream: int64Ptr(104), PrevEvents: []string{"$e3"},
		},
		{
			Kind: "membership", RoomID: "!imported:test.local", UserID: "@alice:test.local",
			Membership: "join", EventID: "$e2",
		},
		{
			Kind: "room_alias", RoomID: "!imported:test.local", Alias: "#imported:test.local",
			Creator: "@alice:test.local",
		},
	}

	// Ещё 8 state-событий: импорт не должен терять ни одного event_id и обязан
	// оставить в room_state последнее по stream_ordering (топик $s12).
	for i := 5; i <= 12; i++ {
		dump = append(dump, dumpRecord{
			Kind: "event", RoomID: "!imported:test.local", EventID: fmt.Sprintf("$s%d", i),
			Type: "m.room.topic", Sender: "@alice:test.local", StateKey: strPtr(""),
			Content:  json.RawMessage(fmt.Sprintf(`{"topic":"тема %d"}`, i)),
			OriginTS: now + int64(i), Depth: int64(i), Stream: int64Ptr(int64(100 + i)),
		})
	}

	path := writeRecords(t, dump)

	im := newImporter(db, "test.local", 4)

	if err := im.importFile(context.Background(), path, 0); err != nil {
		t.Fatalf("importFile() error: %v", err)
	}

	if len(im.errors) != 0 {
		t.Fatalf("ошибки импорта: %+v", im.errors)
	}

	// Повторный импорт не создаёт дублей.
	if err := newImporter(db, "test.local", 4).importFile(context.Background(), path, 0); err != nil {
		t.Fatalf("повторный importFile() error: %v", err)
	}

	roomStore := store.NewRoomStore(db)
	ctx := context.Background()

	var eventsCount int

	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM events WHERE room_id = $1", "!imported:test.local").Scan(&eventsCount); err != nil {
		t.Fatalf("count events: %v", err)
	}

	if eventsCount != 12 {
		t.Fatalf("событий %d, ожидали 12 (4 базовых + 8 топиков, повторный импорт не дублирует)", eventsCount)
	}

	// История через слой store: порядок по stream_ordering сохранён.
	messages, err := roomStore.RoomEvents(ctx, "!imported:test.local", 1000, 10, true)
	if err != nil {
		t.Fatalf("RoomEvents() error: %v", err)
	}

	var bodies []string

	for i := range messages {
		if messages[i].Type != "m.room.message" {
			continue
		}

		var content struct {
			Body string `json:"body"`
		}

		if err := json.Unmarshal(messages[i].Content, &content); err != nil {
			t.Fatalf("unmarshal content: %v", err)
		}

		bodies = append(bodies, content.Body)
	}

	if len(bodies) != 2 || bodies[0] != "второе" || bodies[1] != "первое" {
		t.Fatalf("история = %v, ожидали [второе первое]", bodies)
	}

	// Состояние и членство материализованы импортом.
	membership, err := roomStore.Membership(ctx, "!imported:test.local", "@alice:test.local")
	if err != nil {
		t.Fatalf("Membership() error: %v", err)
	}

	if membership != "join" {
		t.Fatalf("membership = %q", membership)
	}

	var stateEventID string

	if err := db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member'",
		"!imported:test.local").Scan(&stateEventID); err != nil {
		t.Fatalf("select state: %v", err)
	}

	if stateEventID != "$e2" {
		t.Fatalf("state event = %q, ожидали $e2", stateEventID)
	}

	// Алиас и state: 10 state-событий (create, member, 8 топиков) + 2 сообщения.
	var stateCount int

	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM room_state WHERE room_id = $1", "!imported:test.local").Scan(&stateCount); err != nil {
		t.Fatalf("count state: %v", err)
	}

	if stateCount != 3 { // m.room.create, m.room.member, m.room.topic (последний топик побеждает)
		t.Fatalf("state-строк %d, ожидали 3 (create, member, topic)", stateCount)
	}

	var topicID string

	if err := db.Pool().QueryRow(ctx,
		"SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.topic'",
		"!imported:test.local").Scan(&topicID); err != nil {
		t.Fatalf("select topic: %v", err)
	}

	if topicID != "$s12" {
		t.Fatalf("topic state = %q, ожидали $s12 (последнее по stream)", topicID)
	}

	var aliasRoom string

	if err := db.Pool().QueryRow(ctx,
		"SELECT room_id FROM room_aliases WHERE room_alias = '#imported:test.local'").Scan(&aliasRoom); err != nil {
		t.Fatalf("alias не импортирован: %v", err)
	}

	// Отчёт импорта пишется и не содержит ошибок.
	reportPath := filepath.Join(t.TempDir(), "report.json")

	if err := writeReport(reportPath, importReport{Counts: im.counts, Errors: im.errors}); err != nil {
		t.Fatalf("writeReport() error: %v", err)
	}

	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}

	if !strings.Contains(string(reportData), `"Events": 12`) && !strings.Contains(string(reportData), `"events": 12`) {
		t.Fatalf("отчёт не содержит счётчиков: %s", reportData)
	}

	// Позиция потока учитывает импортированные stream_ordering.
	position, err := store.NewSyncStore(db).StreamPosition(ctx)
	if err != nil {
		t.Fatalf("StreamPosition() error: %v", err)
	}

	if position < 104 {
		t.Fatalf("stream position %d, ожидали ≥104", position)
	}

	// Sanity: пул жив и типы событий читаются (используем pgxpool напрямую).
	var pool interface{ Ping(context.Context) error } = db.Pool()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	_ = pgxpool.Pool{} // импорт используется в других интеграционных тестах пакета
	_ = core.Event{}
}

// writeRecords пишет записи дампа в JSONL-файл.
func writeRecords(t *testing.T, records []dumpRecord) string {
	t.Helper()

	lines := make([]string, 0, len(records))

	for i := range records {
		data, err := json.Marshal(records[i])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		lines = append(lines, string(data))
	}

	return writeDump(t, lines...)
}

// TestImportExitCodeAndSequence - путь из README (весь дамп через
// syngo-import) подтягивает sequence stream_ordering, иначе новое сообщение
// получало stream_ordering меньше импортированных и не попадало в
// инкрементальный /sync; битая строка не прерывает импорт, но код возврата
// ненулевой.
func TestImportExitCodeAndSequence(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	db, err := store.Open(context.Background(), dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	ctx := context.Background()

	path := writeDump(t,
		`{"kind":"user","user_id":"@alice:test.local","localpart":"alice"}`,
		`{"kind":"room","room_id":"!r:test.local","room_version":"10","creator":"@alice:test.local"}`,
		`{"kind":"event","room_id":"!r:test.local","event_id":"$c","type":"m.room.create","sender":"@alice:test.local","state_key":"","content":{"creator":"@alice:test.local"},"depth":1,"origin_server_ts":1,"stream_ordering":500}`,
		`{"kind":"event",`, // битая строка посреди дампа
		`{"kind":"event","room_id":"!r:test.local","event_id":"$m","type":"m.room.message","sender":"@alice:test.local","content":{"body":"x"},"depth":2,"origin_server_ts":2,"stream_ordering":900}`,
		`{"kind":"room_alias","alias":"#r:test.local","room_id":"!r:test.local"}`,
	)

	oldArgs, oldFlags := os.Args, flag.CommandLine

	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	flag.CommandLine = flag.NewFlagSet("syngo-import", flag.ContinueOnError)
	os.Args = []string{"syngo-import", "-dsn", dsn, "-file", path, "-report", filepath.Join(t.TempDir(), "r.json")}

	if err := run(); err == nil {
		t.Fatal("импорт с битой строкой должен завершаться ошибкой (ненулевой код возврата)")
	}

	var events, aliases int

	if err := db.Pool().QueryRow(ctx, `SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM room_aliases)`).
		Scan(&events, &aliases); err != nil {
		t.Fatalf("count: %v", err)
	}

	if events != 2 || aliases != 1 {
		t.Fatalf("после битой строки импорт должен продолжиться: events=%d aliases=%d", events, aliases)
	}

	// Новое локальное сообщение идет после импортированных и видно в инкрементальном /sync.
	ev, err := db.AppendEvent(ctx, core.Event{
		EventID: "$new", RoomID: "!r:test.local", Type: "m.room.message", Sender: "@alice:test.local",
		Content: []byte(`{"body":"после импорта"}`), Depth: 3, OriginServerTS: 3, Origin: "test.local",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	if ev.StreamOrdering <= 900 {
		t.Fatalf("stream_ordering нового события %d <= 900: sequence не подтянута", ev.StreamOrdering)
	}

	since, err := db.RoomEventsSince(ctx, "!r:test.local", 900, 10)
	if err != nil {
		t.Fatalf("since: %v", err)
	}

	if len(since) != 1 || since[0].EventID != "$new" {
		t.Fatalf("инкрементальная выборка после позиции импорта: %+v", since)
	}
}

// TestImportEventIDMismatch - event_id, не совпавший с reference hash
// исходного PDU, делает код возврата syngo-import ненулевым (как у
// syngo-import-events). Раньше расхождение только считалось в отчете событий, и
// неверные правила event_id версии комнаты (например, v12) проходили молча.
func TestImportEventIDMismatch(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	room := "!" + strings.Repeat("A", 43)
	pdu := `{"type":"m.room.create","sender":"@alice:test.local","state_key":"","content":{"room_version":"12"},` +
		`"auth_events":[],"prev_events":[],"depth":1,"origin_server_ts":1,"hashes":{"sha256":"x"}}`

	path := writeDump(t,
		`{"kind":"user","user_id":"@alice:test.local","localpart":"alice"}`,
		`{"kind":"room","room_id":"`+room+`","room_version":"12","creator":"@alice:test.local"}`,
		`{"kind":"event","room_id":"`+room+`","event_id":"$`+room[1:]+`","type":"m.room.create","sender":"@alice:test.local",`+
			`"state_key":"","content":{"room_version":"12"},"depth":1,"origin_server_ts":1,"stream_ordering":1,"json":`+strconv.Quote(pdu)+`}`,
		`{"kind":"manifest","events":1,"min_stream":1,"max_stream":1}`,
	)

	reportPath := filepath.Join(t.TempDir(), "r.json")
	if err := runCmd(t, "-dsn", dsn, "-file", path, "-report", reportPath); err == nil {
		t.Fatal("event_id не совпадает с PDU, а код возврата нулевой")
	}

	raw, readErr := os.ReadFile(reportPath) //nolint:gosec // отчет теста
	if readErr != nil || !strings.Contains(string(raw), "reference hash") {
		t.Fatalf("отчет без расхождения event_id: %s, %v", raw, readErr)
	}
}

// TestImportUnsupportedRoomVersion - комната версии без наших правил
// (будущая v13, нестабильная org.matrix.hydra.11) не переносится молча: dry-run и
// импорт завершаются ошибкой с версией и room_id, ее события - ошибками строк.
// Раньше такая комната импортировалась, и чтение ее версии (не число) ломалось.
func TestImportUnsupportedRoomVersion(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	path := writeDump(t,
		`{"kind":"user","user_id":"@alice:test.local","localpart":"alice"}`,
		`{"kind":"room","room_id":"!ok:test.local","room_version":"12","creator":"@alice:test.local"}`,
		`{"kind":"room","room_id":"!hydra:test.local","room_version":"org.matrix.hydra.11","creator":"@alice:test.local"}`,
		`{"kind":"event","room_id":"!hydra:test.local","event_id":"$h","type":"m.room.message","sender":"@alice:test.local","content":{"body":"x"},"depth":1,"origin_server_ts":1,"stream_ordering":1}`,
	)

	if err := runCmd(t, "-dsn", dsn, "-file", path, "-dry-run"); err == nil || !strings.Contains(err.Error(), "org.matrix.hydra.11") {
		t.Fatalf("dry-run: %v", err)
	}

	reportPath := filepath.Join(t.TempDir(), "r.json")
	if err := runCmd(t, "-dsn", dsn, "-file", path, "-report", reportPath); err == nil {
		t.Fatal("комната неподдерживаемой версии, а код возврата нулевой")
	}

	raw, err := os.ReadFile(reportPath) //nolint:gosec // отчет теста
	if err != nil || !strings.Contains(string(raw), `версия \"org.matrix.hydra.11\" не поддерживается`) ||
		!strings.Contains(string(raw), `"room_versions"`) {
		t.Fatalf("отчет: %s, %v", raw, err)
	}
}

// runCmd запускает syngo-import как оператор.
func runCmd(t *testing.T, args ...string) error {
	t.Helper()

	oldArgs, oldFlags := os.Args, flag.CommandLine

	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	flag.CommandLine = flag.NewFlagSet("syngo-import", flag.ContinueOnError)
	os.Args = append([]string{"syngo-import"}, args...)

	return run()
}

// TestImportMediaQuarantineAndURLCache - метаданные медиа через
// syngo-import - карантин переносится блокировкой, превью ссылок пропускаются.
func TestImportMediaQuarantineAndURLCache(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	db, err := store.Open(context.Background(), dsn, 4, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	path := writeDump(t,
		`{"kind":"media","media_id":"plainmedia1","media_type":"text/plain","media_length":3,"created_at_ms":1}`,
		`{"kind":"media","media_id":"quarantine1","media_type":"image/png","media_length":3,"created_at_ms":1,"quarantined_by":"@admin:test.local"}`,
		`{"kind":"media","media_id":"urlpreview1","media_type":"image/png","media_length":3,"created_at_ms":1,"url_cache":"http://example.org/"}`,
	)

	if err := runCmd(t, "-dsn", dsn, "-file", path, "-server-name", "test.local",
		"-report", filepath.Join(t.TempDir(), "r.json")); err != nil {
		t.Fatalf("импорт: %v", err)
	}

	rows, err := db.Pool().Query(context.Background(),
		`SELECT media_id || ':' || blocked::text || ':' || coalesce(quarantined_by, '') FROM media ORDER BY media_id`)
	if err != nil {
		t.Fatalf("media: %v", err)
	}

	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("media: %v", err)
	}

	want := []string{"plainmedia1:false:", "quarantine1:true:@admin:test.local"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("media = %v, ожидали %v", got, want)
	}
}

// strPtr возвращает указатель на строку.
func strPtr(value string) *string { return &value }

// int64Ptr возвращает указатель на int64.
func int64Ptr(value int64) *int64 { return &value }
