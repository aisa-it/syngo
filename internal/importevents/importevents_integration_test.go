//go:build integration

package importevents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/importevents"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// storeWriter - общий писатель в PostgreSQL (importevents.StoreWriter).
func storeWriter(db *store.DB) importevents.StoreWriter { return importevents.NewStoreWriter(db) }

func freshDB(t *testing.T) *store.DB {
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

	db, err := store.Open(context.Background(), dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	return db
}

// coreEvent собирает событие для проверки серверной вставки (nextval).
func coreEvent(roomID, eventID string) core.Event {
	return core.Event{
		EventID:        eventID,
		RoomID:         roomID,
		Type:           "m.room.message",
		Sender:         "@alice:test.local",
		Content:        []byte(`{"body":"local"}`),
		Depth:          1,
		OriginServerTS: 1_700_000_100_000,
		Origin:         "test.local",
	}
}

// newPipe — поток для генерации дампа без загрузки в память.
func newPipe() (*io.PipeReader, *io.PipeWriter) {
	return io.Pipe()
}

// eventJSON собирает строку дампа для события.
func eventJSON(t *testing.T, roomID, eventID, typ string, stream int64, stateKey *string) string {
	t.Helper()

	rec := map[string]any{
		"kind": "event", "room_id": roomID, "event_id": eventID, "type": typ,
		"sender": "@alice:test.local", "origin_server_ts": int64(1_700_000_000_000 + stream),
		"depth": stream, "stream_ordering": stream,
		"content": json.RawMessage(`{"body":"event ` + eventID + `"}`),
	}

	if stateKey != nil {
		rec["state_key"] = *stateKey
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(data)
}

// TestImportHistoryTwoRooms — история двух комнат импортируется с сохранением порядка,
// повторный импорт идемпотентен, store.RoomEventsSince отдаёт исходный порядок.
func TestImportHistoryTwoRooms(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()

	for _, roomID := range []string{"!one:test.local", "!two:test.local"} {
		if _, err := db.Pool().Exec(ctx,
			"INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')", roomID); err != nil {
			t.Fatalf("insert room: %v", err)
		}
	}

	member := ""
	lines := []string{
		eventJSON(t, "!one:test.local", "$one1", "m.room.create", 1, &member),
		eventJSON(t, "!one:test.local", "$one2", "m.room.message", 2, nil),
		eventJSON(t, "!two:test.local", "$two1", "m.room.create", 3, &member),
		eventJSON(t, "!one:test.local", "$one3", "m.room.message", 4, nil),
		eventJSON(t, "!two:test.local", "$two2", "m.room.message", 5, nil),
	}

	dump := strings.Join(lines, "\n")

	importer := importevents.New(storeWriter(db), importevents.Options{BatchSize: 2, ProgressEvery: 0})

	report, err := importer.Import(ctx, strings.NewReader(dump))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Inserted != 5 || report.Bad != 0 {
		t.Fatalf("отчёт: %+v", report)
	}

	events, err := db.RoomEventsSince(ctx, "!one:test.local", 0, 100)
	if err != nil {
		t.Fatalf("room events: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("в комнате !one %d событий, ожидали 3", len(events))
	}

	wantOrder := []string{"$one1", "$one2", "$one3"}

	for i := range events {
		if events[i].EventID != wantOrder[i] {
			t.Fatalf("порядок нарушен: got %s, want %s", events[i].EventID, wantOrder[i])
		}

		if events[i].StreamOrdering <= 0 {
			t.Fatalf("stream_ordering не сохранён: %+v", events[i])
		}
	}

	// Повторный импорт: ничего не вставляем, всё считается skipped.
	report2, err := importer.Import(ctx, strings.NewReader(dump))
	if err != nil {
		t.Fatalf("повторный импорт: %v", err)
	}

	if report2.Inserted != 0 || report2.Skipped != 5 {
		t.Fatalf("повторный импорт: %+v", report2)
	}

	var total int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM events").Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}

	if total != 5 {
		t.Fatalf("всего событий %d, ожидали 5", total)
	}
}

// TestImport100kPerformance — приёмочный замер: 100k событий, время и RSS в отчёт.
func TestImport100kPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}

	db := freshDB(t)
	ctx := context.Background()

	const roomID = "!perf:test.local"

	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	const total = 100_000

	// Дамп генерируем потоком, чтобы тест сам не съел память.
	pr, pw := newPipe()

	go func() {
		defer pw.Close()

		for i := int64(1); i <= total; i++ {
			if _, err := fmt.Fprintln(pw, eventJSON(t, roomID, fmt.Sprintf("$perf%d", i), "m.room.message", i, nil)); err != nil {
				return
			}
		}
	}()

	importer := importevents.New(storeWriter(db), importevents.Options{BatchSize: 1000, ProgressEvery: 0})

	start := time.Now()

	report, err := importer.Import(ctx, pr)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	elapsed := time.Since(start)

	if report.Inserted != total {
		t.Fatalf("вставлено %d, ожидали %d", report.Inserted, total)
	}

	t.Logf("100k events: %s (%.0f events/s), RSS %.1f MiB", elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds(), float64(report.RSSBytes)/(1<<20))

	if elapsed > 2*time.Minute {
		t.Fatalf("100k событий импортировались за %s — дольше окна 2 минуты", elapsed)
	}

	if report.RSSBytes > 512<<20 {
		t.Fatalf("RSS %d байт — память не должна расти линейно с дампом", report.RSSBytes)
	}
}

// TestImportAdvancesSequence - после импорта новый локальный /send получает
// stream_ordering больше импортированных, а повторный импорт не откатывает счётчик.
func TestImportAdvancesSequence(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()

	const roomID = "!seq:test.local"

	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	var lines []string
	for i := int64(1); i <= 100; i++ {
		lines = append(lines, eventJSON(t, roomID, fmt.Sprintf("$seq%d", i), "m.room.message", i, nil))
	}

	dump := strings.Join(lines, "\n")
	importer := importevents.New(storeWriter(db), importevents.Options{BatchSize: 25, ProgressEvery: -1})

	if _, err := importer.Import(ctx, strings.NewReader(dump)); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Новое событие через серверный путь (nextval) должно идти после импортированных.
	ev, err := db.AppendEvent(ctx, coreEvent(roomID, "$local1"))
	if err != nil {
		t.Fatalf("append local event: %v", err)
	}

	if ev.StreamOrdering <= 100 {
		t.Fatalf("stream_ordering нового события = %d, ожидали > 100 (sequence не подтянулась)", ev.StreamOrdering)
	}

	// Повторный импорт не должен откатить счётчик назад.
	before := ev.StreamOrdering

	if _, err := importer.Import(ctx, strings.NewReader(dump)); err != nil {
		t.Fatalf("повторный импорт: %v", err)
	}

	ev2, err := db.AppendEvent(ctx, coreEvent(roomID, "$local2"))
	if err != nil {
		t.Fatalf("append second local event: %v", err)
	}

	if ev2.StreamOrdering <= before {
		t.Fatalf("после повторного импорта stream_ordering %d <= %d", ev2.StreamOrdering, before)
	}
}

// TestImportMissingStreamUsesNextval - событие без stream_ordering получает nextval,
// а не 0 (иначе оно оказалось бы «до» всей истории).
func TestImportMissingStreamUsesNextval(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()

	const roomID = "!nostream:test.local"

	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1, '@alice:test.local')", roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	dump := `{"kind":"event","room_id":"!nostream:test.local","event_id":"$nosteream1","type":"m.room.message","sender":"@alice:test.local","content":{"body":"x"},"depth":1,"origin_server_ts":1}`

	importer := importevents.New(storeWriter(db), importevents.Options{BatchSize: 10, ProgressEvery: -1})

	report, err := importer.Import(ctx, strings.NewReader(dump))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Inserted != 1 {
		t.Fatalf("inserted = %d", report.Inserted)
	}

	events, err := db.RoomEventsSince(ctx, roomID, 0, 10)
	if err != nil {
		t.Fatalf("room events: %v", err)
	}

	if len(events) != 1 || events[0].StreamOrdering <= 0 {
		t.Fatalf("stream_ordering должен быть назначен через nextval: %+v", events)
	}
}
