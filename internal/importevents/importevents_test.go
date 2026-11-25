package importevents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/importevents"
)

// fakeWriter — писатель в памяти.
type fakeWriter struct {
	rooms   map[string]struct{}
	events  map[string]importevents.Event
	batches int
}

func newFakeWriter(rooms ...string) *fakeWriter {
	m := map[string]struct{}{}
	for _, r := range rooms {
		m[r] = struct{}{}
	}

	return &fakeWriter{rooms: m, events: map[string]importevents.Event{}}
}

func (w *fakeWriter) ExistingRooms(context.Context) (map[string]struct{}, error) { return w.rooms, nil }

func (w *fakeWriter) InsertEvents(_ context.Context, events []importevents.Event) (int64, error) {
	w.batches++

	var inserted int64

	for i := range events {
		ev := events[i]
		if _, ok := w.events[ev.EventID]; ok {
			continue
		}

		w.events[ev.EventID] = ev
		inserted++
	}

	return inserted, nil
}

func record(t *testing.T, kind, room, eventID, typ string, stream int64) string {
	t.Helper()

	rec := map[string]any{
		"kind": kind, "room_id": room, "event_id": eventID, "type": typ,
		"sender": "@alice:test.local", "origin_server_ts": int64(1_700_000_000_000),
		"depth": 1, "stream_ordering": stream,
		"content": json.RawMessage(`{"body":"hi"}`),
	}

	if typ == "m.room.member" {
		rec["state_key"] = "@alice:test.local"
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(data)
}

func TestImportCountsAndBatches(t *testing.T) {
	t.Parallel()

	writer := newFakeWriter("!room:test.local")
	importer := importevents.New(writer, importevents.Options{BatchSize: 2, ProgressEvery: 0})

	dump := strings.Join([]string{
		record(t, "event", "!room:test.local", "$e1", "m.room.message", 1),
		record(t, "event", "!room:test.local", "$e2", "m.room.member", 2),
		record(t, "event", "!room:test.local", "$e3", "m.room.message", 3),
		record(t, "event", "!room:test.local", "$e1", "m.room.message", 1), // дубль
		record(t, "event", "!missing:test.local", "$e4", "m.room.message", 4),
		"{битая строка",
		`{"kind":"user","user_id":"@x:test.local"}`,
	}, "\n")

	report, err := importer.Import(context.Background(), strings.NewReader(dump))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	// Read считает все разобранные события, включая отброшенные из-за неизвестной комнаты.
	if report.Read != 5 {
		t.Fatalf("read = %d, ожидали 5", report.Read)
	}

	if report.Inserted != 3 {
		t.Fatalf("inserted = %d, ожидали 3", report.Inserted)
	}

	if report.Skipped != 1 {
		t.Fatalf("skipped = %d, ожидали 1 (дубль)", report.Skipped)
	}

	if report.Bad != 2 {
		t.Fatalf("bad = %d, ожидали 2 (чужая комната + битая строка)", report.Bad)
	}

	if len(writer.events) != 3 {
		t.Fatalf("в хранилище %d событий, ожидали 3", len(writer.events))
	}

	// ByType считает разобранные события (включая дубли, которые отсекает БД).
	if report.ByType["m.room.message"] != 3 || report.ByType["m.room.member"] != 1 {
		t.Fatalf("счётчики по типам: %+v", report.ByType)
	}

	if writer.batches != 2 {
		t.Fatalf("пачек %d, ожидали 2 (3 события при batch=2)", writer.batches)
	}

	// origin берётся из домена отправителя
	if got := writer.events["$e1"].Origin; got != "test.local" {
		t.Fatalf("origin = %q", got)
	}

	if got := writer.events["$e1"].StreamOrdering; got != 1 {
		t.Fatalf("stream_ordering = %d, ожидали 1", got)
	}

	if !bytes.Contains([]byte(report.Text()), []byte("type m.room.message: 3")) {
		t.Fatal("в отчёте нет счётчика типов")
	}
}

func TestImportProgressAndRSS(t *testing.T) {
	t.Parallel()

	writer := newFakeWriter("!r:test.local")

	var progress bytes.Buffer

	importer := importevents.New(writer, importevents.Options{
		BatchSize: 1, ProgressEvery: 2, ProgressWriter: &progress,
	})

	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, record(t, "event", "!r:test.local", fmt.Sprintf("$e%d", i), "m.room.message", int64(i)))
	}

	report, err := importer.Import(context.Background(), strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Inserted != 5 {
		t.Fatalf("inserted = %d", report.Inserted)
	}

	if !strings.Contains(progress.String(), "read=2") || !strings.Contains(progress.String(), "read=4") {
		t.Fatalf("прогресс не напечатан: %q", progress.String())
	}

	if report.Duration() < 0 || report.RSSBytes < 0 {
		t.Fatalf("отчёт с отрицательными значениями: %+v", report)
	}

	if report.FinishedAt.Before(report.StartedAt) {
		t.Fatalf("время: %v < %v", report.FinishedAt, report.StartedAt)
	}

	_ = time.Second
}

// TestImportV1EventRefs - в комнатах v1/v2 prev_events и auth_events -
// пары [event_id, {sha256}]; раньше Unmarshal в []string падал и все события
// таких комнат уходили в bad.
func TestImportV1EventRefs(t *testing.T) {
	t.Parallel()

	writer := newFakeWriter("!old:test.local")
	importer := importevents.New(writer, importevents.Options{ProgressEvery: -1})

	dump := `{"kind":"event","room_id":"!old:test.local","event_id":"$1:test.local","type":"m.room.message",` +
		`"sender":"@alice:test.local","content":{"body":"v1"},"depth":3,"stream_ordering":3,` +
		`"prev_events":[["$0:test.local",{"sha256":"abc"}]],"auth_events":[["$a:test.local",{}],"$b:test.local"]}`

	report, err := importer.Import(context.Background(), strings.NewReader(dump))
	if err != nil || report.Bad != 0 || report.Inserted != 1 {
		t.Fatalf("импорт v1: %v\n%s", err, report.Text())
	}

	ev := writer.events["$1:test.local"]
	if fmt.Sprint(ev.PrevEvents) != "[$0:test.local]" || fmt.Sprint(ev.AuthEvents) != "[$a:test.local $b:test.local]" {
		t.Fatalf("ссылки: prev=%v auth=%v", ev.PrevEvents, ev.AuthEvents)
	}
}

// TestImportPDUJSON - тело события берется из исходного PDU (поле json
// дампа syngo-synapse-export), \u0000 в content заменяется, а не роняет пачку.
func TestImportPDUJSON(t *testing.T) {
	t.Parallel()

	writer := newFakeWriter("!r:test.local")
	importer := importevents.New(writer, importevents.Options{ProgressEvery: -1})

	pdu := `{"content":{"body":"a\u0000b","n":12345678901234567890},"prev_events":[["$p",{}]],"auth_events":["$c"],"type":"m.room.message"}`

	line, err := json.Marshal(map[string]any{
		"kind": "event", "room_id": "!r:test.local", "event_id": "$e", "type": "m.room.message",
		"sender": "@alice:test.local", "depth": 2, "stream_ordering": -4, "json": pdu,
	})
	if err != nil {
		t.Fatal(err)
	}

	manifest := `{"kind":"manifest","events":1,"min_stream":-4,"max_stream":-4}`

	report, err := importer.Import(context.Background(), strings.NewReader(string(line)+"\n"+manifest))
	if err != nil || report.Inserted != 1 {
		t.Fatalf("импорт: %v\n%s", err, report.Text())
	}

	ev := writer.events["$e"]

	if string(ev.Content) != `{"body":"a`+"�"+`b","n":12345678901234567890}` {
		t.Fatalf("content = %s", ev.Content)
	}

	if fmt.Sprint(ev.PrevEvents, ev.AuthEvents) != "[$p] [$c]" {
		t.Fatalf("ссылки из PDU: %v %v", ev.PrevEvents, ev.AuthEvents)
	}

	if ev.StreamOrdering != -4 {
		t.Fatalf("stream_ordering backfill = %d, ожидали -4", ev.StreamOrdering)
	}
}

// TestImportManifest - оборванная выгрузка (событий меньше, чем в
// манифесте, или манифеста нет) - ошибка, а не тихий частичный импорт.
func TestImportManifest(t *testing.T) {
	t.Parallel()

	event := `{"kind":"event","room_id":"!r:test.local","event_id":"$e","type":"m.room.message","sender":"@a:test.local","stream_ordering":1,"json":"{\"content\":{}}"}`

	for name, dump := range map[string]string{
		"мало событий":    event + "\n" + `{"kind":"manifest","events":2}`,
		"нет манифеста":   event,
		"пустой манифест": `{"kind":"manifest","events":1}`,
	} {
		importer := importevents.New(newFakeWriter("!r:test.local"), importevents.Options{ProgressEvery: -1})

		if _, err := importer.Import(context.Background(), strings.NewReader(dump)); !errors.Is(err, importevents.ErrIncompleteDump) {
			t.Fatalf("%s: err = %v, ожидали ErrIncompleteDump", name, err)
		}
	}

	importer := importevents.New(newFakeWriter("!r:test.local"), importevents.Options{ProgressEvery: -1})

	report, err := importer.Import(context.Background(), strings.NewReader(event+"\n"+`{"kind":"manifest","events":1}`))
	if err != nil || report.Manifest == nil || report.Manifest.Events != 1 {
		t.Fatalf("полный дамп: %v %+v", err, report.Manifest)
	}
}

// TestImportResumeFromCheckpoint - чекпойнт пишется после каждой
// пачки, повторный запуск с ResumeAfterLine не пишет уже записанные строки.
func TestImportResumeFromCheckpoint(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, record(t, "event", "!r:test.local", fmt.Sprintf("$e%d", i), "m.room.message", int64(i)))
	}

	dump := strings.Join(lines, "\n")

	var checkpoints []int64

	first := newFakeWriter("!r:test.local")
	importer := importevents.New(first, importevents.Options{
		BatchSize: 2, ProgressEvery: -1,
		Checkpoint: func(line int64) error { checkpoints = append(checkpoints, line); return nil },
	})

	if _, err := importer.Import(context.Background(), strings.NewReader(dump)); err != nil {
		t.Fatalf("import: %v", err)
	}

	if fmt.Sprint(checkpoints) != "[2 4 5]" {
		t.Fatalf("чекпойнты %v, ожидали [2 4 5]", checkpoints)
	}

	resumed := newFakeWriter("!r:test.local")
	importer = importevents.New(resumed, importevents.Options{BatchSize: 2, ProgressEvery: -1, ResumeAfterLine: 4})

	report, err := importer.Import(context.Background(), strings.NewReader(dump))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if len(resumed.events) != 1 || report.Resumed != 4 || report.Read != 5 {
		t.Fatalf("продолжение: записано %d, %s", len(resumed.events), report.Text())
	}

	if _, ok := resumed.events["$e5"]; !ok {
		t.Fatalf("после чекпойнта должно записаться только $e5: %v", resumed.events)
	}
}

// rowErrorWriter отказывает пачке, в которой есть событие $poison, ошибкой
// данных строки - как PostgreSQL на нарушении ограничения.
type rowErrorWriter struct{ *fakeWriter }

var errRow = errors.New("row: constraint violation")

func (w rowErrorWriter) InsertEvents(ctx context.Context, events []importevents.Event) (int64, error) {
	for i := range events {
		if events[i].EventID == "$poison" {
			return 0, errRow
		}
	}

	return w.fakeWriter.InsertEvents(ctx, events)
}

func (rowErrorWriter) IsRowError(err error) bool { return errors.Is(err, errRow) }

// TestImportRowErrorQuarantine - одна строка с ошибкой БД не роняет
// пачку и весь импорт: пачка повторяется по одному событию, плохая строка
// уходит в отчет и карантин.
func TestImportRowErrorQuarantine(t *testing.T) {
	t.Parallel()

	writer := rowErrorWriter{newFakeWriter("!r:test.local")}

	var quarantine bytes.Buffer

	importer := importevents.New(writer, importevents.Options{BatchSize: 10, ProgressEvery: -1, BadLines: &quarantine})

	dump := strings.Join([]string{
		record(t, "event", "!r:test.local", "$ok1", "m.room.message", 1),
		record(t, "event", "!r:test.local", "$poison", "m.room.message", 2),
		record(t, "event", "!r:test.local", "$ok2", "m.room.message", 3),
	}, "\n")

	report, err := importer.Import(context.Background(), strings.NewReader(dump))
	if err != nil {
		t.Fatalf("ошибка строки не должна ронять импорт: %v", err)
	}

	if report.Inserted != 2 || report.Bad != 1 || len(writer.events) != 2 {
		t.Fatalf("отчет: %s", report.Text())
	}

	if !strings.Contains(quarantine.String(), `"$poison"`) || strings.Count(quarantine.String(), "\n") != 1 {
		t.Fatalf("карантин: %q", quarantine.String())
	}
}

func TestSanitizeContent(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		`{"body":"ok"}`:                `{"body":"ok"}`,
		`{"body":"a\u0000"}`:           `{"body":"a` + "�" + `"}`,
		`{"k\u0000":["\ud800",1.50]}`:  `{"k` + "�" + `":["` + "�" + `",1.50]}`,
		`{"body":"\\u0000 не символ"}`: `{"body":"\\u0000 не символ"}`,
	} {
		got, err := importevents.SanitizeContent([]byte(in))
		if err != nil || string(got) != want {
			t.Fatalf("SanitizeContent(%s) = %s, %v; ожидали %s", in, got, err, want)
		}
	}
}

// versionedWriter - fakeWriter с версиями комнат и записью состояния.
type versionedWriter struct {
	*fakeWriter

	state map[string][]importevents.StateRow
}

func (w versionedWriter) RoomVersions(context.Context) (map[string]string, error) {
	return map[string]string{"!r:test.local": "10"}, nil
}

func (w versionedWriter) ReplaceRoomState(_ context.Context, roomID string, rows []importevents.StateRow) ([]importevents.StateRow, error) {
	w.state[roomID] = rows

	return nil, nil
}

// TestImportPDUFieldsAndExclusion - из исходного PDU берутся подписи,
// hashes, origin и redacts; отклоненные, soft-failed и outlier помечаются вне
// timeline; event_id проверяется по reference hash; строки kind=state
// заменяют состояние комнаты и сверяются с манифестом.
func TestImportPDUFieldsAndExclusion(t *testing.T) {
	t.Parallel()

	pdu := map[string]any{
		"type": "m.room.redaction", "room_id": "!r:test.local", "sender": "@alice:test.local",
		"content": map[string]any{"reason": "x"}, "redacts": "$target", "depth": 5,
		"origin_server_ts": 1, "prev_events": []any{"$p"}, "auth_events": []any{"$a"},
		"hashes": map[string]any{"sha256": "h"}, "signatures": map[string]any{"test.local": map[string]any{"ed25519:k": "s"}},
		"origin": "test.local",
	}

	goodID, err := core.EventID(pdu, 10)
	if err != nil {
		t.Fatal(err)
	}

	pduJSON, err := json.Marshal(pdu)
	if err != nil {
		t.Fatal(err)
	}

	line := func(id string, extra map[string]any) string {
		rec := map[string]any{
			"kind": "event", "room_id": "!r:test.local", "event_id": id, "type": "m.room.redaction",
			"sender": "@alice:test.local", "depth": 5, "stream_ordering": 7, "json": string(pduJSON),
		}
		for k, v := range extra {
			rec[k] = v
		}

		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	writer := versionedWriter{newFakeWriter("!r:test.local"), map[string][]importevents.StateRow{}}
	importer := importevents.New(writer, importevents.Options{ProgressEvery: -1})

	dump := strings.Join([]string{
		line(goodID, nil),
		line("$wrong", map[string]any{"rejected_reason": "auth_error"}),
		line("$soft", map[string]any{"soft_failed": true}),
		line("$out", map[string]any{"outlier": true}),
		`{"kind":"state","room_id":"!r:test.local","type":"m.room.create","state_key":"","event_id":"` + goodID + `"}`,
		`{"kind":"manifest","events":4,"state_rows":1}`,
	}, "\n")

	report, err := importer.Import(context.Background(), strings.NewReader(dump))
	if err != nil {
		t.Fatalf("импорт: %v\n%s", err, report.Text())
	}

	ev := writer.events[goodID]
	if ev.Redacts != "$target" || ev.Origin != "test.local" || string(ev.Hashes) != `{"sha256":"h"}` ||
		!strings.Contains(string(ev.Signatures), "ed25519:k") || ev.PDUJSON != string(pduJSON) || ev.Excluded != "" {
		t.Fatalf("поля PDU: %+v", ev)
	}

	for id, want := range map[string]string{
		"$wrong": importevents.ExcludedRejected, "$soft": importevents.ExcludedSoftFailed, "$out": importevents.ExcludedOutlier,
	} {
		if got := writer.events[id].Excluded; got != want {
			t.Fatalf("%s: Excluded=%q, ожидали %q", id, got, want)
		}
	}

	// $wrong, $soft, $out - тот же PDU под чужим event_id.
	if report.IDMismatch != 3 || report.Excluded[importevents.ExcludedRejected] != 1 {
		t.Fatalf("отчет: %s", report.Text())
	}

	if rows := writer.state["!r:test.local"]; len(rows) != 1 || rows[0].EventID != goodID || report.StateRooms != 1 {
		t.Fatalf("состояние: %+v, %s", rows, report.Text())
	}

	// Манифест сверяет и строки состояния.
	broken := strings.Replace(dump, `"state_rows":1`, `"state_rows":2`, 1)

	importer = importevents.New(versionedWriter{newFakeWriter("!r:test.local"), map[string][]importevents.StateRow{}},
		importevents.Options{ProgressEvery: -1})

	if _, err := importer.Import(context.Background(), strings.NewReader(broken)); !errors.Is(err, importevents.ErrIncompleteDump) {
		t.Fatalf("неполное состояние: err = %v", err)
	}
}

// TestImportStateCheckpoint - чекпойнт не обгоняет записанное состояние: строка
// начала следующей комнаты не считается записанной, пока не записана комната.
func TestImportStateCheckpoint(t *testing.T) {
	t.Parallel()

	writer := versionedWriter{newFakeWriter("!r:test.local", "!s:test.local"), map[string][]importevents.StateRow{}}

	var checkpoints []int64

	importer := importevents.New(writer, importevents.Options{
		BatchSize: 10, ProgressEvery: -1,
		Checkpoint: func(line int64) error {
			// К моменту чекпойнта все комнаты со строками до line записаны.
			if line >= 4 && len(writer.state["!r:test.local"]) != 2 {
				t.Errorf("чекпойнт %d до записи состояния !r", line)
			}

			if line >= 5 && len(writer.state["!s:test.local"]) != 1 {
				t.Errorf("чекпойнт %d до записи состояния !s", line)
			}

			checkpoints = append(checkpoints, line)

			return nil
		},
	})

	state := func(room, typ string) string {
		return `{"kind":"state","room_id":"` + room + `","type":"` + typ + `","state_key":"","event_id":"$e1"}`
	}

	dump := strings.Join([]string{
		record(t, "event", "!r:test.local", "$e1", "m.room.message", 1),
		record(t, "event", "!s:test.local", "$e2", "m.room.message", 2),
		state("!r:test.local", "m.room.create"),
		state("!r:test.local", "m.room.name"),
		state("!s:test.local", "m.room.create"),
	}, "\n")

	if _, err := importer.Import(context.Background(), strings.NewReader(dump)); err != nil {
		t.Fatalf("import: %v", err)
	}

	for i := 1; i < len(checkpoints); i++ {
		if checkpoints[i] < checkpoints[i-1] {
			t.Fatalf("чекпойнты идут назад: %v", checkpoints)
		}
	}
}

// receiptWriter - fakeWriter с позициями отметок прочтения.
type receiptWriter struct {
	*fakeWriter

	calls *int
	err   error
}

func (w receiptWriter) ApplyImportedReceipts(context.Context) (int64, error) {
	*w.calls++

	return 3, w.err
}

// TestImportAppliesReceipts - после событий импорт проставляет позиции
// отметкам прочтения, импортированным раньше них (данные пользователей идут до
// событий), - иначе разбор событий не видел отметок; сбой - ошибка импорта.
func TestImportAppliesReceipts(t *testing.T) {
	t.Parallel()

	calls := 0
	writer := receiptWriter{fakeWriter: newFakeWriter("!r:test.local"), calls: &calls}

	report, err := importevents.New(writer, importevents.Options{BatchSize: 10, ProgressEvery: -1}).
		Import(context.Background(), strings.NewReader(record(t, "event", "!r:test.local", "$e1", "m.room.message", 1)))
	if err != nil || calls != 1 || report.Receipts != 3 || !strings.Contains(report.Text(), "receipts: positioned=3") {
		t.Fatalf("отметки: calls=%d err=%v\n%s", calls, err, report.Text())
	}

	writer.err = errors.New("db down")

	if _, err := importevents.New(writer, importevents.Options{BatchSize: 10, ProgressEvery: -1}).
		Import(context.Background(), strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "отметки прочтения") {
		t.Fatalf("сбой отметок не вернул ошибку: %v", err)
	}
}
