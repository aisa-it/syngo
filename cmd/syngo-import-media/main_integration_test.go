//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/internal/testdb"
)

// freshDB накатывает схему с нуля на тестовую базу.
func freshDB(t *testing.T) (*store.DB, string) {
	t.Helper()

	dsn := testdb.DSN(t)

	migrator, err := migrate.New("file://../../migrations",
		"pgx5://"+strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://"))
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	db, err := store.Open(context.Background(), dsn, 4, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(db.Close)

	return db, dsn
}

// synapseFile кладет файл в раскладку media_store Synapse.
func synapseFile(t *testing.T, root, mediaID string, payload []byte) string {
	t.Helper()

	path := filepath.Join(root, "local_content", mediaID[0:2], mediaID[2:4], mediaID[4:])
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

// runImport запускает команду как оператор и возвращает ошибку и текст отчета.
func runImport(t *testing.T, dsn, dump, mediaStore, objectDir string) (string, error) {
	t.Helper()

	oldArgs, oldFlags := os.Args, flag.CommandLine

	t.Cleanup(func() { os.Args, flag.CommandLine = oldArgs, oldFlags })

	report := filepath.Join(t.TempDir(), "media-report.txt")
	flag.CommandLine = flag.NewFlagSet("syngo-import-media", flag.ContinueOnError)
	os.Args = []string{
		"syngo-import-media", "-dsn", dsn, "-dump", dump, "-media-store", mediaStore,
		"-object-dir", objectDir, "-server-name", "test.local", "-report", report,
	}

	err := run()

	text, readErr := os.ReadFile(report) //nolint:gosec // файл теста
	if readErr != nil && err == nil {
		t.Fatalf("отчет: %v", readErr)
	}

	return string(text), err
}

var countsRe = regexp.MustCompile(`copied=(\d+) skipped=(\d+)`)

func counts(t *testing.T, report string) (copied, skipped string) {
	t.Helper()

	m := countsRe.FindStringSubmatch(report)
	if m == nil {
		t.Fatalf("нет итогов в отчете:\n%s", report)
	}

	return m[1], m[2]
}

func writeDump(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "media.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("дамп: %v", err)
	}

	return path
}

// TestImportMediaMigration - на живой БД: карантин переносится и
// соблюдается, url_cache пропускается без ошибки, размер в БД - размер байтов,
// sha256 сверяется с Synapse, повторный запуск ничего не копирует, после обрыва
// докопирует только недостающее.
func TestImportMediaMigration(t *testing.T) {
	db, dsn := freshDB(t)
	ctx := context.Background()

	source := t.TempDir()
	objectDir := t.TempDir()

	okSum := synapseFile(t, source, "plainmedia1", []byte("plain media bytes"))
	synapseFile(t, source, "quarantine1", []byte("bad picture"))

	dump := writeDump(t,
		`{"kind":"user","user_id":"@alice:test.local"}`,
		`{"kind":"media","media_id":"plainmedia1","media_type":"text/plain","media_length":17,"upload_name":"a.txt",`+
			`"user_id":"@alice:test.local","created_at_ms":1700000000000,"sha256":"`+okSum+`"}`,
		`{"kind":"media","media_id":"quarantine1","media_type":"image/png","media_length":11,`+
			`"user_id":"@alice:test.local","created_at_ms":1700000000000,"quarantined_by":"@admin:test.local"}`,
		`{"kind":"media","media_id":"urlpreview1","media_type":"image/png","media_length":64,`+
			`"user_id":null,"created_at_ms":1700000000000,"url_cache":"http://example.org/"}`,
	)

	report, err := runImport(t, dsn, dump, source, objectDir)
	if err != nil {
		t.Fatalf("первый прогон: %v\n%s", err, report)
	}

	if copied, skipped := counts(t, report); copied != "2" || skipped != "0" {
		t.Fatalf("первый прогон: copied=%s skipped=%s\n%s", copied, skipped, report)
	}

	var (
		blocked       bool
		quarantinedBy string
		previews      int
	)

	if err := db.Pool().QueryRow(ctx,
		`SELECT blocked, coalesce(quarantined_by, '') FROM media WHERE media_id = 'quarantine1'`).
		Scan(&blocked, &quarantinedBy); err != nil {
		t.Fatalf("карантин: %v", err)
	}

	if !blocked || quarantinedBy != "@admin:test.local" {
		t.Fatalf("карантин не перенесен: blocked=%t quarantined_by=%q", blocked, quarantinedBy)
	}

	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM media WHERE media_id = 'urlpreview1'`).Scan(&previews); err != nil || previews != 0 {
		t.Fatalf("превью ссылки перенесено: %d, %v", previews, err)
	}

	objects, err := media.NewLocalStore(objectDir)
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}

	svc := media.NewService(store.NewMediaStore(db), objects, "test.local", 1<<20)

	if _, _, err := svc.Download(ctx, "test.local", "quarantine1"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("медиа в карантине скачивается: %v", err)
	}

	meta, body, err := svc.Download(ctx, "test.local", "plainmedia1")
	if err != nil {
		t.Fatalf("скачивание: %v", err)
	}

	data, _ := io.ReadAll(body)
	_ = body.Close()

	if string(data) != "plain media bytes" || meta.SizeBytes != int64(len(data)) {
		t.Fatalf("скачано %q, size_bytes=%d", data, meta.SizeBytes)
	}

	// Повторный запуск: ничего не копирует, объекты не переписываются.
	objectPath := filepath.Join(objectDir, "local_content", "pl", "ai", "nmedia1")

	before, err := os.Stat(objectPath)
	if err != nil {
		t.Fatalf("объект: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	report, err = runImport(t, dsn, dump, source, objectDir)
	if err != nil {
		t.Fatalf("повторный прогон: %v\n%s", err, report)
	}

	if copied, skipped := counts(t, report); copied != "0" || skipped != "2" {
		t.Fatalf("повторный прогон копирует заново: copied=%s skipped=%s\n%s", copied, skipped, report)
	}

	if after, err := os.Stat(objectPath); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("объект переписан повторным прогоном: %v", err)
	}

	// Обрыв: объекта нет - докопируется только он.
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("удалить объект: %v", err)
	}

	report, err = runImport(t, dsn, dump, source, objectDir)
	if err != nil {
		t.Fatalf("прогон после обрыва: %v\n%s", err, report)
	}

	if copied, skipped := counts(t, report); copied != "1" || skipped != "1" {
		t.Fatalf("после обрыва: copied=%s skipped=%s\n%s", copied, skipped, report)
	}
}

// TestImportMediaDiscrepancies - расхождения размера и sha256 с
// Synapse дают ненулевой код и строку problem в отчете; в БД - реальный размер.
func TestImportMediaDiscrepancies(t *testing.T) {
	db, dsn := freshDB(t)
	ctx := context.Background()

	source := t.TempDir()
	objectDir := t.TempDir()

	synapseFile(t, source, "shortfile1", []byte("12345"))
	synapseFile(t, source, "rottenfile", []byte("rotten"))

	wrong := sha256.Sum256([]byte("what synapse hashed"))

	dump := writeDump(t,
		`{"kind":"media","media_id":"shortfile1","media_type":"text/plain","media_length":999,"created_at_ms":1700000000000}`,
		`{"kind":"media","media_id":"rottenfile","media_type":"text/plain","media_length":6,"created_at_ms":1700000000000,`+
			`"sha256":"`+hex.EncodeToString(wrong[:])+`"}`,
	)

	report, err := runImport(t, dsn, dump, source, objectDir)
	if err == nil {
		t.Fatalf("расхождения должны давать ненулевой код:\n%s", report)
	}

	for _, want := range []string{"problem: shortfile1", "problem: rottenfile", "hash_mismatch"} {
		if !strings.Contains(report, want) {
			t.Fatalf("в отчете нет %q:\n%s", want, report)
		}
	}

	var size int64
	if err := db.Pool().QueryRow(ctx, `SELECT size_bytes FROM media WHERE media_id = 'shortfile1'`).Scan(&size); err != nil {
		t.Fatalf("размер: %v", err)
	}

	if size != 5 {
		t.Fatalf("size_bytes = %d: Content-Length скачивания не совпадет с байтами (файл 5)", size)
	}
}
