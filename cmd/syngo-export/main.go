// Command syngo-export выгружает данные SYNGO в JSONL-формат импортёра (exit path).
//
//	syngo-export -dsn "$PSQL_DSN" -out syngo-dump.jsonl \
//	    -media-dir var/media -media-out /tmp/synapse-media
//
// Формат строк: kind = user | room | room_alias | event | state | manifest | membership |
// receipt | media - тот же, что у syngo-synapse-export и export.sql, и его читают
// syngo-import / syngo-import-events / syngo-import-media: исходный PDU,
// отклоненные и soft-failed, оригинал удаленных событий (в пределах
// redaction.retention_period), членство с event_id, текущее состояние, отметки
// прочтения, карантин и sha256 медиа.
//
// Для отката после cutover (ADR-0006 §7): события, созданные в synapse-go после
// переключения, - -after-stream <max stream_ordering импорта> (и -media-since <время
// cutover> для медиа); такой дамп - архив для ручного разбора, загрузчика в Synapse нет.
//
// Что теряется при переносе обратно в Synapse: argon2id-хеши паролей (нужен сброс пароля),
// E2EE-ключи устройств, account_data, presence; receipts в дампе есть, но загрузчика в
// Synapse нет (см. README, «Миграция: exit path»).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/exportdata"
	"github.com/aisa-it/syngo/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-export: ", err)
	}
}

func run() error {
	var (
		dsn        = flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN PostgreSQL")
		outPath    = flag.String("out", "-", "куда писать JSONL ('-' — stdout)")
		mediaDir   = flag.String("media-dir", "", "наш каталог объектного хранилища (media.directory)")
		mediaOut   = flag.String("media-out", "", "куда копировать медиа в раскладке Synapse (local_content/...)")
		batch      = flag.Int("batch", 1000, "размер пачки чтения событий")
		reportPath = flag.String("report", "", "куда записать отчёт (по умолчанию stdout, если дамп не в stdout)")
		afterStr   = flag.String("after-stream", "", "только события со stream_ordering больше N (созданные после cutover)")
		mediaSince = flag.String("media-since", "", "только медиа, созданные не раньше (RFC 3339)")
	)

	flag.Parse()

	opts := exportdata.Options{BatchSize: *batch, MediaSourceDir: *mediaDir, MediaOutDir: *mediaOut}

	if *afterStr != "" {
		after, err := strconv.ParseInt(*afterStr, 10, 64)
		if err != nil {
			return fmt.Errorf("-after-stream: %w", err)
		}

		opts.AfterStream, opts.HasAfterStream = after, true
	}

	if *mediaSince != "" {
		since, err := time.Parse(time.RFC3339, *mediaSince)
		if err != nil {
			return fmt.Errorf("-media-since: %w", err)
		}

		opts.MediaSince = since
	}

	if *dsn == "" {
		return errors.New("нужен -dsn или SYNGOD_DATABASE_DSN")
	}

	// Когда дамп идёт в stdout, отчёт печатается после него (поток дампа уже закрыт).
	db, err := store.Open(context.Background(), *dsn, 8, 0)
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}

	defer db.Close()

	writer, closeFn, err := openOutput(*outPath)
	if err != nil {
		return err
	}

	defer closeFn()

	exporter := exportdata.New(db.Pool(), opts)

	report, err := exporter.Run(context.Background(), writer)
	if err != nil {
		return err
	}

	text := report.Text()

	for _, e := range exporter.MediaErrors() {
		text += "media error: " + e + "\n"
	}

	// Дамп может идти в stdout — тогда отчёт пишем в stderr, чтобы не ломать JSONL.
	if *outPath == "-" {
		fmt.Fprint(os.Stderr, text)
	} else {
		fmt.Print(text)
	}

	if *reportPath != "" {
		//nolint:gosec // путь к отчёту задаёт оператор флагом -report
		if err := os.WriteFile(*reportPath, []byte(text), 0o600); err != nil {
			return fmt.Errorf("записать отчёт: %w", err)
		}
	}

	if report.MediaFailed > 0 {
		return fmt.Errorf("медиа скопировано с ошибками: failed=%d (см. отчёт)", report.MediaFailed)
	}

	return nil
}

// openOutput открывает приёмник дампа.
func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}

	f, err := os.Create(path) //nolint:gosec // путь задаёт оператор миграции
	if err != nil {
		return nil, nil, fmt.Errorf("создать дамп: %w", err)
	}

	return f, func() { _ = f.Close() }, nil
}
