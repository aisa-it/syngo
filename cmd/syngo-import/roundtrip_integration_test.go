//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/exportdata"
	"github.com/aisa-it/syngo/internal/importmedia"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/synapseexport"
	"github.com/aisa-it/syngo/internal/testdb"
)

const fixtureDir = "../../internal/importevents/testdata"

// loadSynapseFixture грузит фикстуру живого Synapse 1.165 (события и медиа) в
// схему synapse_fixture тестовой базы и возвращает пул, видящий ее как схему Synapse.
func loadSynapseFixture(t *testing.T, db *store.DB, dsn string) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	for _, name := range []string{"synapse-1.165.sql", "synapse-1.165-media.sql", "synapse-1.165-userdata-schema.sql"} {
		script, err := os.ReadFile(filepath.Join(fixtureDir, name)) //nolint:gosec // файл фикстуры
		if err != nil {
			t.Fatalf("фикстура: %v", err)
		}

		if _, err := db.Pool().Exec(ctx, string(script)); err != nil {
			t.Fatalf("загрузка %s: %v", name, err)
		}
	}

	t.Cleanup(func() {
		_, _ = db.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS synapse_fixture CASCADE")
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}

	src, err := pgxpool.New(ctx, dsn+sep+"search_path=synapse_fixture")
	if err != nil {
		t.Fatalf("пул Synapse: %v", err)
	}

	t.Cleanup(src.Close)

	return src
}

// runExportSQL исполняет tools/synapse-export/export.sql против схемы Synapse так
// же, как psql -At: каждая строка результата - строка JSONL (раньше
// export.sql против схемы Synapse не исполнялся нигде).
func runExportSQL(t *testing.T, src *pgxpool.Pool) []string {
	t.Helper()

	script, err := os.ReadFile("../../tools/synapse-export/export.sql")
	if err != nil {
		t.Fatalf("export.sql: %v", err)
	}

	var clean strings.Builder

	for _, line := range strings.Split(string(script), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			clean.WriteString(line + "\n")
		}
	}

	var out []string

	for _, stmt := range strings.Split(clean.String(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}

		rows, err := src.Query(context.Background(), stmt)
		if err != nil {
			t.Fatalf("export.sql против схемы Synapse: %v\n%s", err, stmt)
		}

		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("export.sql: %v", err)
		}

		out = append(out, lines...)
	}

	return out
}

// byKind оставляет строки дампа заданных kind.
func byKind(lines []string, kinds ...string) []string {
	re := regexp.MustCompile(`"kind":"(` + strings.Join(kinds, "|") + `)"`)

	var out []string

	for _, line := range lines {
		if re.MatchString(line) {
			out = append(out, line)
		}
	}

	return out
}

// importMediaLines переносит байты и метаданные медиа (как syngo-import-media).
func importMediaLines(t *testing.T, db *store.DB, lines []string, sourceDir, objectDir string) importmedia.Report {
	t.Helper()

	var records []importmedia.Record

	for _, line := range byKind(lines, "media") {
		var rec importmedia.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("строка медиа: %v", err)
		}

		records = append(records, rec)
	}

	objects, err := media.NewLocalStore(objectDir)
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}

	importer := importmedia.New(store.NewMediaStore(db), objects, sourceDir, "c4.test")
	importer.SetWorkers(2)

	report, err := importer.Import(context.Background(), records)
	if err != nil {
		t.Fatalf("импорт медиа: %v", err)
	}

	return report
}

// verifyAgainstSynapse - syngo-verify-migration.
func verifyAgainstSynapse(t *testing.T, src *pgxpool.Pool, db *store.DB, stage string) {
	t.Helper()

	report, err := synapseexport.Verify(context.Background(), src, db.Pool(), synapseexport.VerifyOptions{ServerName: "c4.test"})
	if err != nil {
		t.Fatalf("%s: сверка: %v", stage, err)
	}

	if !report.OK() || !report.MediaChecked || report.Media != 4 || report.Users == 0 || report.Aliases == 0 {
		t.Fatalf("%s: сверка с Synapse:\n%s", stage, report.Text())
	}
}

// snapshot - то, что сверка с Synapse не видит, но обратная выгрузка обязана
// сохранить: исходные PDU, оригиналы удаленных событий, sha256 медиа, отметки прочтения.
func snapshot(t *testing.T, db *store.DB) string {
	t.Helper()

	var out string

	if err := db.Pool().QueryRow(context.Background(), `
		SELECT concat_ws(' | ',
			(SELECT string_agg(event_id || '=' || md5(coalesce(pdu_json, '')), ',' ORDER BY event_id) FROM events),
			(SELECT string_agg(event_id || '=' || md5(coalesce(pdu_json, '')), ',' ORDER BY event_id) FROM excluded_events),
			(SELECT string_agg(event_id || '=' || md5(coalesce(original_content::text, '')), ',' ORDER BY event_id) FROM redactions),
			(SELECT string_agg(media_id || '=' || encode(sha256, 'hex') || ':' || blocked, ',' ORDER BY media_id) FROM media),
			(SELECT string_agg(concat_ws(':', room_id, user_id, receipt_type, thread_id, event_id, ts, event_stream), ','
			        ORDER BY room_id, user_id, receipt_type, thread_id) FROM receipts),
			(SELECT count(*)::text FROM redactions WHERE original_content IS NOT NULL))`).Scan(&out); err != nil {
		t.Fatalf("снимок: %v", err)
	}

	return out
}

// TestRoundTripSynapseFixture - путь отката и выхода. Фикстура живого
// Synapse переносится по runbook (export.sql, syngo-synapse-export, syngo-import,
// импорт медиа), сверка с Synapse проходит; затем syngo-export выгружает базу,
// схема стирается, дамп импортируется теми же импортерами - и сверка с тем же
// Synapse снова проходит, исходные PDU, оригиналы удаленных событий, карантин и
// sha256 медиа, отметки прочтения совпадают. Раньше выгрузка теряла PDU,
// отклоненные события, оригиналы redaction, алиасы и event_id членства (обратный
// импорт - ошибка FK), отметки прочтения.
func TestRoundTripSynapseFixture(t *testing.T) {
	dsn := testdb.DSN(t)

	migrateSchema(t, dsn)

	db, err := store.Open(context.Background(), dsn, 8, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(db.Close)

	ctx := context.Background()
	src := loadSynapseFixture(t, db, dsn)
	addReceiptsToSynapse(t, db)

	// Шаг 3.2 runbook: export.sql и syngo-synapse-export.
	all := runExportSQL(t, src)

	var events bytes.Buffer
	if _, err := synapseexport.ExportEvents(ctx, src, &events, synapseexport.Options{BatchSize: 7}); err != nil {
		t.Fatalf("выгрузка событий: %v", err)
	}

	dump := append(byKind(all, "user", "room", "room_alias"), strings.Split(strings.TrimSpace(events.String()), "\n")...)
	dump = append(dump, byKind(all, "membership", "receipt")...)

	if err := runCmd(t, "-dsn", dsn, "-file", writeDump(t, dump...), "-server-name", "c4.test",
		"-report", filepath.Join(t.TempDir(), "r.json")); err != nil {
		t.Fatalf("импорт из Synapse: %v", err)
	}

	objects := t.TempDir()

	mediaReport := importMediaLines(t, db, all, filepath.Join(fixtureDir, "synapse-media"), objects)
	if mediaReport.HasProblems() || mediaReport.Copied != 4 || mediaReport.URLCache != 2 || mediaReport.Quarantined != 1 {
		t.Fatalf("медиа из Synapse:\n%s", mediaReport.ReportFile())
	}

	verifyAgainstSynapse(t, src, db, "импорт из Synapse")

	var originals int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM redactions WHERE original_content IS NOT NULL`).Scan(&originals); err != nil || originals == 0 {
		t.Fatalf("импорт не сохранил оригиналы удаленных событий: %d, %v", originals, err)
	}

	before := snapshot(t, db)

	// Обратная выгрузка (exit path / откат).
	var out bytes.Buffer

	mediaOut := t.TempDir()

	exportReport, err := exportdata.New(db.Pool(), exportdata.Options{
		BatchSize: 5, MediaSourceDir: objects, MediaOutDir: mediaOut,
	}).Run(ctx, &out)
	if err != nil {
		t.Fatalf("syngo-export: %v", err)
	}

	for _, want := range []string{`"kind":"state"`, `"kind":"room_alias"`, `"kind":"manifest"`, `"rejected_reason":`, `"json":`, `"kind":"receipt"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("в выгрузке нет %s: %s", want, exportReport.Text())
		}
	}

	exported := filepath.Join(t.TempDir(), "syngo-dump.jsonl")
	if err := os.WriteFile(exported, out.Bytes(), 0o600); err != nil {
		t.Fatalf("дамп: %v", err)
	}

	// «Новая» база: схема стерта и накатана заново, фикстура Synapse осталась.
	resetSchema(t, dsn)

	if err := runCmd(t, "-dsn", dsn, "-file", exported, "-server-name", "c4.test",
		"-report", filepath.Join(t.TempDir(), "r2.json")); err != nil {
		t.Fatalf("импорт выгрузки syngo-export: %v", err)
	}

	mediaReport = importMediaLines(t, db, readLines(t, exported), mediaOut, t.TempDir())
	if mediaReport.HasProblems() || mediaReport.Copied != 4 || mediaReport.Quarantined != 1 {
		t.Fatalf("медиа из выгрузки:\n%s", mediaReport.ReportFile())
	}

	verifyAgainstSynapse(t, src, db, "после обратной выгрузки")

	if after := snapshot(t, db); after != before {
		t.Fatalf("обратная выгрузка изменила данные:\nдо:    %s\nпосле: %s", before, after)
	}
}

// resetSchema стирает схему synapse-go и накатывает ее заново.
func resetSchema(t *testing.T, dsn string) {
	t.Helper()

	migrator, err := migrate.New("file://../../migrations", "pgx5://"+stripScheme(dsn))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	defer func() { _, _ = migrator.Close() }()

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("down: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	f, err := os.Open(path) //nolint:gosec // файл теста
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	defer f.Close()

	var lines []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	return lines
}
