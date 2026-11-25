// Command syngo-import-media переносит медиа из Synapse в наш медиа-репозиторий.
//
//	# 1. экспорт метаданных на стороне Synapse (kind=media), см. tools/synapse-export/export.sql
//	# 2. импорт файлов + метаданных
//	syngo-import-media -dsn "$PSQL_DSN" -dump media.jsonl \
//	    -media-store /var/lib/matrix-synapse/media_store -object-dir var/media \
//	    -server-name matrix.example.org -report media-import.txt
//
// Команда идемпотентна: повторный прогон не дублирует записи и не копирует байты, уже
// лежащие в хранилище с тем же размером и sha256 (статус skipped), - поэтому медиа
// можно перенести заранее, а в окне простоя докопировать разницу, и так же продолжить
// прогон после обрыва. Превью ссылок (url_cache) пропускаются, карантин
// (quarantined_by) переносится и соблюдается, sha256 сверяется с БД Synapse; код
// возврата ненулевой при missing, mismatch, hash_mismatch и failed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aisa-it/syngo/internal/importmedia"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-import-media: ", err)
	}
}

func run() error {
	var (
		dsn        = flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN PostgreSQL")
		dump       = flag.String("dump", "", "JSONL-дамп Synapse с записями kind=media")
		mediaStore = flag.String("media-store", "", "каталог media_store Synapse (содержит local_content/)")
		objectDir  = flag.String("object-dir", "", "наш каталог объектного хранилища (media.directory)")
		serverName = flag.String("server-name", "", "имя нашего сервера (для content_uri)")
		reportPath = flag.String("report", "", "куда записать отчёт (по умолчанию только stdout)")
		limit      = flag.Int("limit", 0, "обработать не больше N записей (0 — все)")
		workers    = flag.Int("workers", 4, "сколько файлов копировать параллельно")
		rehash     = flag.Bool("rehash", false, "перед пропуском уже перенесенного перечитать объект и сверить sha256")
	)

	flag.Parse()

	switch {
	case *dsn == "":
		return errors.New("нужен -dsn или SYNGOD_DATABASE_DSN")
	case *dump == "":
		return errors.New("нужен -dump с JSONL-дампом метаданных медиа")
	case *mediaStore == "":
		return errors.New("нужен -media-store: каталог media_store Synapse")
	case *objectDir == "":
		return errors.New("нужен -object-dir: наш каталог объектного хранилища")
	case *serverName == "":
		return errors.New("нужен -server-name (имя нашего сервера)")
	}

	records, err := readRecords(*dump, *limit)
	if err != nil {
		return err
	}

	db, err := store.Open(context.Background(), *dsn, 8, 0)
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}

	defer db.Close()

	objects, err := media.NewLocalStore(*objectDir)
	if err != nil {
		return fmt.Errorf("объектное хранилище: %w", err)
	}

	importer := importmedia.New(store.NewMediaStore(db), objects, *mediaStore, *serverName)
	importer.SetWorkers(*workers)
	importer.SetRehash(*rehash)

	report, err := importer.Import(context.Background(), records)
	if err != nil {
		return fmt.Errorf("импорт: %w", err)
	}

	text := report.ReportFile()
	fmt.Print(text)

	if *reportPath != "" {
		//nolint:gosec // путь к отчёту задаёт оператор миграции флагом -report
		if err := os.WriteFile(*reportPath, []byte(text), 0o600); err != nil {
			return fmt.Errorf("записать отчёт: %w", err)
		}
	}

	if report.HasProblems() {
		return fmt.Errorf("импорт завершён с замечаниями: missing=%d mismatch=%d hash_mismatch=%d failed=%d",
			report.Missing, report.Mismatched, report.HashMismatch, report.FailedCount)
	}

	return nil
}

// readRecords читает JSONL и оставляет только kind=media.
func readRecords(path string, limit int) ([]importmedia.Record, error) {
	file, err := os.Open(path) //nolint:gosec // путь задаёт оператор миграции
	if err != nil {
		return nil, fmt.Errorf("открыть дамп: %w", err)
	}

	defer file.Close()

	var records []importmedia.Record

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	line := 0

	for scanner.Scan() {
		line++

		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "--") {
			continue
		}

		var rec struct {
			Kind string `json:"kind"`
			importmedia.Record
		}

		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("строка %d: %w", line, err)
		}

		if rec.Kind != "media" {
			continue
		}

		if rec.MediaID == "" {
			return nil, fmt.Errorf("строка %d: пустой media_id", line)
		}

		records = append(records, rec.Record)

		if limit > 0 && len(records) >= limit {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("чтение дампа: %w", err)
	}

	return records, nil
}
