//go:build integration

package importevents_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/importevents"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/synapseexport"
	"github.com/aisa-it/syngo/internal/testdb"
)

// synapseFixture загружает фикстуру живого Synapse (testdata/synapse-1.165.sql)
// в схему synapse_fixture той же тестовой базы и возвращает пул, который видит
// ее таблицы как свои (search_path), - так экспортер работает со схемой Synapse
// без изменений в запросах.
func synapseFixture(t *testing.T, db *store.DB) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	script, err := os.ReadFile("testdata/synapse-1.165.sql")
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, string(script)); err != nil {
		t.Fatalf("загрузка фикстуры Synapse: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS synapse_fixture CASCADE")
	})

	dsn := testdb.DSN(t)

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	src, err := pgxpool.New(ctx, dsn+sep+"search_path=synapse_fixture")
	if err != nil {
		t.Fatalf("пул Synapse: %v", err)
	}

	t.Cleanup(src.Close)

	// Комнаты, пользователей и алиасы переносит syngo-import (export.sql) - здесь
	// теми же запросами: сверка проверяет и их.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO rooms (room_id, room_version, creator, is_public)
		SELECT room_id, coalesce(room_version, '10'), coalesce(creator, ''), coalesce(is_public, false)
		FROM synapse_fixture.rooms;
		INSERT INTO users (user_id, localpart, password_hash, deactivated)
		SELECT name, split_part(substr(name, 2), ':', 1), coalesce(password_hash, ''), deactivated <> 0
		FROM synapse_fixture.users;
		INSERT INTO room_aliases (room_alias, room_id, creator)
		SELECT room_alias, room_id, coalesce(creator, '') FROM synapse_fixture.room_aliases`); err != nil {
		t.Fatalf("комнаты фикстуры: %v", err)
	}

	return src
}

// exportAndImport выгружает события фикстуры и импортирует их.
func exportAndImport(t *testing.T, db *store.DB, src *pgxpool.Pool) importevents.Report {
	t.Helper()

	ctx := context.Background()

	var dump bytes.Buffer

	// Маленькая пачка: выгрузка проходит несколько страниц keyset-пагинации.
	if _, err := synapseexport.ExportEvents(ctx, src, &dump, synapseexport.Options{BatchSize: 7}); err != nil {
		t.Fatalf("выгрузка: %v", err)
	}

	// Как syngo-import-events по умолчанию (-redaction-retention 168h).
	db.SetRedactionRetention(7 * 24 * time.Hour)

	report, err := importevents.New(importevents.NewStoreWriter(db), importevents.Options{
		BatchSize: 5, ProgressEvery: -1,
	}).Import(ctx, &dump)
	if err != nil {
		t.Fatalf("импорт: %v\n%s", err, report.Text())
	}

	// Членство переносит syngo-import (export.sql, kind=membership) после
	// событий - здесь тем же запросом.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership, forgotten)
		SELECT DISTINCT ON (rm.room_id, rm.user_id) rm.event_id, rm.room_id, rm.user_id, rm.user_id,
		       rm.membership, rm.forgotten <> 0
		FROM synapse_fixture.room_memberships rm
		JOIN synapse_fixture.current_state_events cse ON cse.room_id = rm.room_id AND cse.event_id = rm.event_id
		ORDER BY rm.room_id, rm.user_id, rm.event_id
		ON CONFLICT (room_id, user_id) DO UPDATE SET event_id = EXCLUDED.event_id, membership = EXCLUDED.membership`); err != nil {
		t.Fatalf("членство: %v", err)
	}

	return report
}

// TestSynapseFixtureRoundTrip - на данных живого Synapse: выгружены все
// события, включая backfill (stream_ordering < 0), комнату v1 (ссылки парами) и
// \u0000 в content; число, границы stream_ordering и event_id совпадают с
// источником; новое сообщение после импорта видно в инкрементальной выборке;
// повторный прогон идемпотентен.
func TestSynapseFixtureRoundTrip(t *testing.T) {
	db := freshDB(t)
	src := synapseFixture(t, db)
	ctx := context.Background()

	var (
		total, minStream, maxStream int64
		nulEvent, backfillEvent     string
		v1Room                      string
		v1Events                    int64
	)

	if err := src.QueryRow(ctx, `SELECT count(*), min(stream_ordering), max(stream_ordering) FROM events`).
		Scan(&total, &minStream, &maxStream); err != nil {
		t.Fatalf("источник: %v", err)
	}

	if err := src.QueryRow(ctx, `
		SELECT (SELECT event_id FROM event_json WHERE strpos(json, '\u0000') > 0),
		       (SELECT event_id FROM events WHERE stream_ordering < 0),
		       (SELECT room_id FROM rooms WHERE room_version = '1'),
		       (SELECT count(*) FROM events e JOIN rooms r USING (room_id) WHERE r.room_version = '1')`).
		Scan(&nulEvent, &backfillEvent, &v1Room, &v1Events); err != nil {
		t.Fatalf("особые события фикстуры: %v", err)
	}

	if minStream >= 0 {
		t.Fatalf("в фикстуре нет backfill: min(stream_ordering)=%d", minStream)
	}

	report := exportAndImport(t, db, src)

	if report.Bad != 0 || report.Inserted != total {
		t.Fatalf("импорт: inserted=%d bad=%d, ожидали %d и 0\n%s", report.Inserted, report.Bad, total, report.Text())
	}

	verify, err := synapseexport.Verify(ctx, src, db.Pool(), synapseexport.VerifyOptions{})
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}

	if !verify.OK() || verify.Target.MaxStream != maxStream || verify.Target.MinStream != minStream {
		t.Fatalf("сверка с Synapse:\n%s", verify.Text())
	}

	// Комната v1: события не ушли в bad, ссылки разобраны из пар [id, hashes].
	var (
		imported int64
		prev     []string
	)

	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*), (SELECT prev_events FROM events WHERE room_id = $1 AND type = 'm.room.message' LIMIT 1)
		FROM events WHERE room_id = $1`, v1Room).Scan(&imported, &prev); err != nil {
		t.Fatalf("комната v1: %v", err)
	}

	if imported != v1Events || len(prev) == 0 || !strings.HasPrefix(prev[0], "$") {
		t.Fatalf("комната v1: импортировано %d из %d, prev_events=%v", imported, v1Events, prev)
	}

	// \u0000 в content: событие на месте, символ заменен (jsonb его не принимает).
	var body string

	if err := db.Pool().QueryRow(ctx, `SELECT content->>'body' FROM events WHERE event_id = $1`, nulEvent).Scan(&body); err != nil {
		t.Fatalf("событие с \\u0000: %v", err)
	}

	if !strings.Contains(body, "\uFFFD") {
		t.Fatalf("content с \\u0000: %q", body)
	}

	// backfill сохранил отрицательный stream_ordering.
	var backfillStream int64

	if err := db.Pool().QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = $1`, backfillEvent).
		Scan(&backfillStream); err != nil || backfillStream != minStream {
		t.Fatalf("backfill: stream=%d err=%v, ожидали %d", backfillStream, err, minStream)
	}

	// Новое сообщение после импорта - после всех импортированных и видно с позиции max.
	var roomID string
	if err := src.QueryRow(ctx, `SELECT room_id FROM rooms WHERE room_version = '10'`).Scan(&roomID); err != nil {
		t.Fatalf("комната v10: %v", err)
	}

	ev, err := db.AppendEvent(ctx, core.Event{
		EventID: "$after-import", RoomID: roomID, Type: "m.room.message", Sender: "@alice:c4.test",
		Content: []byte(`{"body":"после переезда"}`), Depth: 100, OriginServerTS: 1, Origin: "c4.test",
	})
	if err != nil {
		t.Fatalf("новое сообщение: %v", err)
	}

	since, err := db.RoomEventsSince(ctx, roomID, maxStream, 10)
	if err != nil || len(since) != 1 || since[0].EventID != ev.EventID {
		t.Fatalf("инкрементальная выборка после импорта: %+v, %v", since, err)
	}

	if _, err := db.Pool().Exec(ctx, `DELETE FROM events WHERE event_id = $1`, ev.EventID); err != nil {
		t.Fatalf("удалить новое сообщение: %v", err)
	}

	// Повторный прогон: ничего не вставлено, сверка по-прежнему чистая.
	again := exportAndImport(t, db, src)
	if again.Inserted != 0 || again.Skipped != total || again.Bad != 0 {
		t.Fatalf("повторный импорт: %s", again.Text())
	}

	verify, err = synapseexport.Verify(ctx, src, db.Pool(), synapseexport.VerifyOptions{})
	if err != nil || !verify.OK() {
		t.Fatalf("сверка после повтора: %v\n%s", err, verify.Text())
	}
}

// TestVerifyReportsMismatch - сверка идет с Synapse, а не с дампом,
// и называет расхождения: потерянное событие видно по числу, комнате и event_id.
func TestVerifyReportsMismatch(t *testing.T) {
	db := freshDB(t)
	src := synapseFixture(t, db)
	ctx := context.Background()

	exportAndImport(t, db, src)

	var lost, room string

	if err := db.Pool().QueryRow(ctx, `
		DELETE FROM events WHERE event_id = (
			SELECT event_id FROM events WHERE type = 'm.room.message' ORDER BY stream_ordering DESC LIMIT 1)
		RETURNING event_id, room_id`).Scan(&lost, &room); err != nil {
		t.Fatalf("удалить событие: %v", err)
	}

	verify, err := synapseexport.Verify(ctx, src, db.Pool(), synapseexport.VerifyOptions{})
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}

	text := verify.Text()

	if verify.OK() || verify.MissingEvents != 1 || verify.RoomCountDiffs != 1 ||
		!strings.Contains(text, lost) || !strings.Contains(text, room) {
		t.Fatalf("сверка должна найти потерю %s в %s:\n%s", lost, room, text)
	}
}

// TestSynapseFixtureLossless - на данных живого Synapse: исходный PDU
// (подписи, hashes) сохранен и event_id сходится с его reference hash;
// отклоненное, soft-failed и outlier-событие не попадают в timeline и состояние;
// redaction применены (content и PDU цели вырезаны, оригинал - в redactions,
// поиск удаленный текст не находит); состояние комнат - как current_state_events
// (форк темы, а не последнее по stream_ordering); event_auth заполнена; сверка
// чистая, повторный прогон ничего не меняет.
func TestSynapseFixtureLossless(t *testing.T) {
	db := freshDB(t)
	src := synapseFixture(t, db)
	ctx := context.Background()

	report := exportAndImport(t, db, src)

	if report.IDMismatch != 0 || report.Bad != 0 {
		t.Fatalf("event_id по reference hash или строки: %s", report.Text())
	}

	if report.Excluded[importevents.ExcludedRejected] != 1 || report.Excluded[importevents.ExcludedSoftFailed] != 1 ||
		report.Excluded[importevents.ExcludedOutlier] != 1 || report.StateRooms != 3 || report.Redacted != 4 {
		t.Fatalf("отчет импорта: %s", report.Text())
	}

	verify, err := synapseexport.Verify(ctx, src, db.Pool(), synapseexport.VerifyOptions{ServerName: "c4.test"})
	if err != nil || !verify.OK() || verify.StateRows == 0 || verify.Redacted != 4 || verify.Memberships == 0 {
		t.Fatalf("сверка: %v\n%s", err, verify.Text())
	}

	// Отклоненные, soft-failed, outlier - в excluded_events, не в timeline.
	rows, err := src.Query(ctx, `
		SELECT e.event_id, e.room_id,
		       CASE WHEN e.rejection_reason IS NOT NULL THEN 'rejected'
		            WHEN j.internal_metadata LIKE '%soft_failed%' THEN 'soft_failed' ELSE 'outlier' END
		FROM events e JOIN event_json j USING (event_id)
		WHERE e.rejection_reason IS NOT NULL OR e.outlier OR j.internal_metadata LIKE '%soft_failed%'`)
	if err != nil {
		t.Fatalf("особые события: %v", err)
	}

	type special struct{ id, room, reason string }

	var specials []special

	for rows.Next() {
		var sp special
		if err := rows.Scan(&sp.id, &sp.room, &sp.reason); err != nil {
			t.Fatalf("scan: %v", err)
		}

		specials = append(specials, sp)
	}

	rows.Close()

	if len(specials) != 3 {
		t.Fatalf("в фикстуре %d особых событий, ожидали 3", len(specials))
	}

	rooms := store.NewRoomStore(db)

	for _, sp := range specials {
		var reason string
		if err := db.Pool().QueryRow(ctx, `SELECT reason FROM excluded_events WHERE event_id = $1`, sp.id).Scan(&reason); err != nil || reason != sp.reason {
			t.Fatalf("%s: в excluded_events reason=%q err=%v, ожидали %s", sp.id, reason, err, sp.reason)
		}

		timeline, err := rooms.RoomEvents(ctx, sp.room, 1<<40, 1000, true)
		if err != nil {
			t.Fatalf("timeline: %v", err)
		}

		for i := range timeline {
			if timeline[i].EventID == sp.id {
				t.Fatalf("%s (%s) виден в /messages", sp.id, sp.reason)
			}
		}
	}

	// Состояние - как current_state_events: тема из форка, а не последняя по stream.
	var roomID, wantTopic, gotTopic, latestTopic string

	if err := src.QueryRow(ctx, `
		SELECT room_id, event_id FROM current_state_events WHERE type = 'm.room.topic'
		  AND room_id = (SELECT room_id FROM rooms WHERE room_version = '10')`).Scan(&roomID, &wantTopic); err != nil {
		t.Fatalf("тема источника: %v", err)
	}

	if err := db.Pool().QueryRow(ctx, `
		SELECT (SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.topic'),
		       (SELECT event_id FROM events WHERE room_id = $1 AND type = 'm.room.topic' ORDER BY stream_ordering DESC LIMIT 1)`,
		roomID).Scan(&gotTopic, &latestTopic); err != nil {
		t.Fatalf("тема: %v", err)
	}

	if gotTopic != wantTopic || gotTopic == latestTopic {
		t.Fatalf("room_state тема %s, в Synapse %s (последняя по stream %s)", gotTopic, wantTopic, latestTopic)
	}

	// redaction: цель помечена, content и PDU вырезаны, оригинал в redactions,
	// поиск не находит удаленный текст.
	var (
		redactedBy, content, pdu, original string
		found                              int
	)

	if err := db.Pool().QueryRow(ctx, `
		SELECT t.redacted_by, t.content::text, coalesce(t.pdu_json, ''), coalesce(r.original_content->>'body', ''),
		       (SELECT count(*) FROM events WHERE body_tsv @@ plainto_tsquery('simple', 'секрет'))
		FROM events t JOIN redactions r ON r.event_id = t.redacted_by
		WHERE t.sender = '@bob:c4.test' AND t.room_id = $1 AND t.redacted_by = r.event_id
		  AND r.event_id IN (SELECT event_id FROM events WHERE sender = '@bob:c4.test' AND type = 'm.room.redaction')`,
		roomID).Scan(&redactedBy, &content, &pdu, &original, &found); err != nil {
		t.Fatalf("удаленное сообщение: %v", err)
	}

	if content != "{}" || strings.Contains(pdu, "u0441") || !strings.Contains(pdu, `"content":{}`) ||
		original != "секрет боба" || found != 0 {
		t.Fatalf("redaction: content=%s pdu=%s original=%q найдено поиском=%d", content, pdu, original, found)
	}

	// Исходный PDU, подписи, hashes, event_auth.
	var (
		withPDU, withSignatures, authRows int
		authEvents                        []string
	)

	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE pdu_json IS NOT NULL),
		       count(*) FILTER (WHERE signatures ? 'c4.test' AND hashes ? 'sha256'),
		       (SELECT count(*) FROM event_auth WHERE event_id = $1),
		       (SELECT auth_events FROM events WHERE event_id = $1)
		FROM events`, redactedBy).Scan(&withPDU, &withSignatures, &authRows, &authEvents); err != nil {
		t.Fatalf("PDU: %v", err)
	}

	if withPDU != 46 || withSignatures != 46 || authRows == 0 || authRows != len(authEvents) {
		t.Fatalf("PDU у %d, подписи у %d из 46; event_auth %d из %d", withPDU, withSignatures, authRows, len(authEvents))
	}

	// Повторный прогон идемпотентен: ничего не вставлено и не удалено заново.
	again := exportAndImport(t, db, src)
	if again.Inserted != 0 || again.Redacted != 0 || again.Bad != 0 {
		t.Fatalf("повторный импорт: %s", again.Text())
	}

	verify, err = synapseexport.Verify(ctx, src, db.Pool(), synapseexport.VerifyOptions{ServerName: "c4.test"})
	if err != nil || !verify.OK() {
		t.Fatalf("сверка после повтора: %v\n%s", err, verify.Text())
	}
}
