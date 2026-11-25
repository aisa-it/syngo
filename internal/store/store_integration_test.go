//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// newMigrator поднимает мигратор на тестовой БД. Схема всегда накатывается с нуля
// (Down → Up), чтобы тест проверял и up, и down.
func newMigrator(t *testing.T) *migrate.Migrate {
	t.Helper()

	dsn := testdb.DSN(t)

	m, err := migrate.New("file://../../migrations", "pgx5://"+stripScheme(dsn))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	return m
}

func stripScheme(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(dsn) > len(prefix) && dsn[:len(prefix)] == prefix {
			return dsn[len(prefix):]
		}
	}

	return dsn
}

func openDB(t *testing.T) *store.DB {
	t.Helper()

	dsn := testdb.DSN(t)

	db, err := store.OpenWithOptions(context.Background(), store.Options{DSN: dsn, MaxConns: 4, Tracer: queryCounter{}})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	return db
}

// TestMigrationsUpDown — миграции накатываются и откатываются
// на чистой БД без ошибок.
func TestMigrationsUpDown(t *testing.T) {
	m := newMigrator(t)

	t.Cleanup(func() {
		_ = m.Down()
		if _, err := m.Close(); err != nil {
			t.Logf("migrate close: %v", err)
		}
	})

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	latest, dirty, err := m.Version()
	if err != nil || latest == 0 || dirty {
		t.Fatalf("version after up: v=%d dirty=%t err=%v", latest, dirty, err)
	}

	// Откат одной версии должен оставить предыдущую (на пустой схеме — ErrNilVersion).
	// Предыдущая - не обязательно latest-1: в нумерации бывают пропуски (номера
	// заняты параллельными ветками).
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 1: %v", err)
	}

	if v, _, verErr := m.Version(); latest > 1 && (verErr != nil || v >= latest || v == 0) {
		t.Fatalf("после down 1 ожидали версию ниже %d, получили v=%d err=%v", latest, v, verErr)
	}

	// Полный откат — схема пуста.
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down all: %v", err)
	}

	if _, _, verErr := m.Version(); !errors.Is(verErr, migrate.ErrNilVersion) {
		t.Fatalf("после down ожидали пустую схему, получили err=%v", verErr)
	}
}

// TestSchemaCoversP0 — схема покрывает все блоки P0 из
// docs/api-scope.md (AUTH, ROOMS, EVENTS, SYNC, MEDIA). Проверяем наличие таблиц и
// ключевых индексов, а не их вид: DDL вправе меняться в следующих миграциях.
func TestSchemaCoversP0(t *testing.T) {
	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	ctx := context.Background()
	db := openDB(t)

	for _, name := range []string{
		"users", "devices", "access_tokens", // AUTH
		"rooms", "room_aliases", "room_state", "room_memberships", // ROOMS
		"events", "transactions", // EVENTS
		"media", // MEDIA (P0, последний блок)
	} {
		var regclass *string

		if err := db.Pool().QueryRow(ctx, "SELECT to_regclass($1)::text", "public."+name).
			Scan(&regclass); err != nil {
			t.Fatalf("probe table %s: %v", name, err)
		}

		if regclass == nil {
			t.Fatalf("таблица %s отсутствует в схеме", name)
		}
	}

	for _, idx := range []string{
		"events_room_stream_idx", "events_room_state_idx", "room_state_pkey",
		"transactions_pkey", "media_server_media_id_key",
	} {
		var found int

		if err := db.Pool().QueryRow(ctx,
			"SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1", idx).
			Scan(&found); err != nil {
			t.Fatalf("probe index %s: %v", idx, err)
		}

		if found == 0 {
			t.Fatalf("индекс %s отсутствует в схеме", idx)
		}
	}
}

// TestEventAppendAndStreamOrder — вставка события и выборка
// по комнате в stream-порядке.
func TestEventAppendAndStreamOrder(t *testing.T) {
	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	ctx := context.Background()
	db := openDB(t)
	roomID := "!room:test.local"

	var roomCount int

	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM rooms WHERE room_id = $1", roomID).Scan(&roomCount); err != nil {
		t.Fatalf("probe rooms: %v", err)
	}

	// Комнату создаем напрямую: createRoom проверяется отдельно, здесь - слой хранения.
	if _, err := db.Pool().Exec(ctx,
		"INSERT INTO rooms (room_id, creator) VALUES ($1, $2)", roomID, "@alice:test.local"); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	appender, ok := any(db).(store.EventStore)
	if !ok {
		t.Fatal("store.DB должен реализовывать store.EventStore")
	}

	var previous string

	for i, body := range []string{"первое", "второе", "третье"} {
		content, err := json.Marshal(map[string]string{"body": body})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		ev, err := appender.AppendEvent(ctx, core.Event{
			EventID:        "$event" + body + ":test.local",
			RoomID:         roomID,
			Type:           "m.room.message",
			Sender:         "@alice:test.local",
			Content:        content,
			PrevEvents:     prev(previous),
			AuthEvents:     nil,
			Depth:          int64(i + 1),
			OriginServerTS: 1_700_000_000_000 + int64(i),
			Origin:         "test.local",
		})
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}

		if ev.StreamOrdering == 0 {
			t.Fatalf("stream_ordering не назначен для %s", ev.EventID)
		}

		if i > 0 && ev.StreamOrdering <= 0 {
			t.Fatalf("stream_ordering должен расти: %d", ev.StreamOrdering)
		}

		previous = ev.EventID
	}

	events, err := appender.RoomEventsSince(ctx, roomID, 0, 100)
	if err != nil {
		t.Fatalf("room events since: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("ожидали 3 события, получили %d", len(events))
	}

	for i := 1; i < len(events); i++ {
		if events[i].StreamOrdering <= events[i-1].StreamOrdering {
			t.Fatalf("нарушен stream-порядок: %d <= %d", events[i].StreamOrdering, events[i-1].StreamOrdering)
		}
	}

	// since-курсор: после второго события остаётся только третье.
	tail, err := appender.RoomEventsSince(ctx, roomID, events[1].StreamOrdering, 100)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}

	if len(tail) != 1 || tail[0].EventID != events[2].EventID {
		t.Fatalf("since-курсор вернул %d событий, ожидали последнее", len(tail))
	}

	// materialized state: upsert обновляет event_id при конфликте.
	stateEvent := events[0]

	if err := appender.UpsertRoomState(ctx, core.StateRef{
		RoomID: roomID, Type: "m.room.topic", StateKey: "", EventID: stateEvent.EventID,
	}); err != nil {
		t.Fatalf("upsert state: %v", err)
	}

	if err := appender.UpsertRoomState(ctx, core.StateRef{
		RoomID: roomID, Type: "m.room.topic", StateKey: "", EventID: events[2].EventID,
	}); err != nil {
		t.Fatalf("upsert state (second): %v", err)
	}

	state, err := appender.RoomState(ctx, roomID)
	if err != nil {
		t.Fatalf("room state: %v", err)
	}

	if len(state) != 1 || state[0].EventID != events[2].EventID {
		t.Fatalf("materialized state не обновился: %+v", state)
	}
}

func prev(eventID string) []string {
	if eventID == "" {
		return nil
	}

	return []string{eventID}
}

// TestRedactEventIntegration — redaction скрывает content и в БД, и в выдаче.
// В events лежит вырезанный content, оригинал - в redactions.original_content
// (пока не истек redaction.retention_period).
func TestRedactEventIntegration(t *testing.T) {
	m := newMigrator(t)

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db := openDB(t)
	db.SetRedactionRetention(time.Hour)

	rs := store.NewRoomStore(db)
	ctx := context.Background()
	roomID := "!redact-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":test.local"
	eventID := "$redact-target-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	if _, err := db.Pool().Exec(ctx, `INSERT INTO rooms (room_id, room_version, creator) VALUES ($1,'10','@alice:test.local')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		VALUES ($1,$2,'m.room.message','@alice:test.local','{"body":"secret","msgtype":"m.text"}',1,1,'test.local')`,
		eventID, roomID); err != nil {
		t.Fatalf("event: %v", err)
	}

	redaction := core.Event{
		EventID: "$redaction:test.local", RoomID: roomID, Type: "m.room.redaction", Sender: "@alice:test.local",
		Content: json.RawMessage(`{"redacts":"` + eventID + `"}`), Depth: 2, OriginServerTS: 2, Origin: "test.local",
	}
	if _, _, err := rs.AppendEvent(ctx, redaction, nil); err != nil {
		t.Fatalf("redact: %v", err)
	}

	ev, err := rs.EventByID(ctx, roomID, eventID)
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}

	var content map[string]any
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		t.Fatalf("content: %v", err)
	}

	if _, ok := content["body"]; ok {
		t.Fatalf("body не вырезан при чтении после redaction: %s", ev.Content)
	}

	// Оригинал - отдельно от events, его видит только оператор.
	var stored []byte
	if err := db.Pool().QueryRow(ctx, `SELECT original_content FROM redactions WHERE event_id=$1`,
		redaction.EventID).Scan(&stored); err != nil {
		t.Fatalf("исходный content: %v", err)
	}

	var original map[string]any
	if err := json.Unmarshal(stored, &original); err != nil {
		t.Fatalf("original content: %v", err)
	}

	if original["body"] != "secret" {
		t.Fatalf("оригинал не сохранён: %s", stored)
	}

	var redactedBy string
	if err := db.Pool().QueryRow(ctx, `SELECT coalesce(redacted_by,'') FROM events WHERE event_id=$1`, eventID).Scan(&redactedBy); err != nil {
		t.Fatalf("redacted_by: %v", err)
	}

	if redactedBy != "$redaction:test.local" {
		t.Fatalf("redacted_by = %q", redactedBy)
	}
}

// TestDeviceListChangesOnMembership — смена членства фиксирует device-лист
// участника; изменения видны только тем, с кем есть общая комната.
func TestDeviceListChangesOnMembership(t *testing.T) {
	m := newMigrator(t)

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() { _ = m.Down(); _, _ = m.Close() })

	db := openDB(t)
	ctx := context.Background()
	rs := store.NewRoomStore(db)

	roomID := "!devlists-" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":test.local"

	if _, err := db.Pool().Exec(ctx, `INSERT INTO rooms (room_id, room_version, creator) VALUES ($1,'10','@alice:test.local')`, roomID); err != nil {
		t.Fatalf("room: %v", err)
	}

	join := core.Event{
		EventID: "$join-" + strconv.FormatInt(time.Now().UnixNano(), 10), RoomID: roomID,
		Type: "m.room.member", Sender: "@bob:test.local",
		StateKey: strPtrStore("@bob:test.local"), Content: []byte(`{"membership":"join"}`),
		Depth: 1, OriginServerTS: 1, Origin: "test.local",
	}

	written, _, err := rs.AppendEvent(ctx, join, nil)
	if err != nil {
		t.Fatalf("append join: %v", err)
	}

	upper := written.StreamOrdering + 100

	bob, err := db.DeviceListChangesFor(ctx, "@bob:test.local", written.StreamOrdering-1, upper)
	if err != nil {
		t.Fatalf("changes for bob: %v", err)
	}

	if !slices.Contains(bob, "@bob:test.local") {
		t.Fatalf("bob не видит свое изменение: %v", bob)
	}

	// alice не в комнате - изменение bob ей не показывается.
	alice, err := db.DeviceListChangesFor(ctx, "@alice:test.local", 0, upper)
	if err != nil {
		t.Fatalf("changes for alice: %v", err)
	}

	if slices.Contains(alice, "@bob:test.local") {
		t.Fatalf("bob попал в изменения alice без общей комнаты: %v", alice)
	}
}

func strPtrStore(s string) *string { return &s }
