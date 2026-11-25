// Command syngo-synapse-export выгружает все события из БД Synapse в JSONL для
// syngo-import-events.
//
//	syngo-synapse-export -synapse-dsn "$SYNAPSE_DSN" -out events.jsonl
//
// Выгрузка идет одним снимком (REPEATABLE READ READ ONLY) по всем
// stream_ordering, включая отрицательные (backfill) и NULL; в конце - строка
// kind=manifest с числом событий, без нее syngo-import-events дамп не примет.
// Файл пишется во временный <out>.partial и переименовывается только после
// успешного конца: оборванная выгрузка не выглядит готовой. Любая ошибка -
// ненулевой код возврата.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/synapseexport"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-synapse-export: ", err)
	}
}

func run() error {
	var (
		dsn   = flag.String("synapse-dsn", os.Getenv("SYNAPSE_DSN"), "DSN БД Synapse (лучше реплика, read-only роль)")
		out   = flag.String("out", "-", "куда писать JSONL ('-' - stdout)")
		batch = flag.Int("batch", 10000, "строк на запрос")
	)

	flag.Parse()

	if *dsn == "" {
		return errors.New("нужен -synapse-dsn или SYNAPSE_DSN")
	}

	ctx := context.Background()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("подключение к Synapse: %w", err)
	}

	defer func() { _ = conn.Close(ctx) }()

	var (
		w      io.Writer = os.Stdout
		file   *os.File
		tmpOut = *out + ".partial"
	)

	if *out != "-" {
		//nolint:gosec // путь задает оператор миграции флагом -out
		file, err = os.OpenFile(tmpOut, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("открыть %s: %w", tmpOut, err)
		}

		defer func() { _ = file.Close() }()

		w = file
	}

	manifest, err := synapseexport.ExportSnapshot(ctx, conn, w, synapseexport.Options{
		BatchSize: *batch,
		Progress:  os.Stderr,
	})
	if err != nil {
		return err
	}

	if file != nil {
		if err := file.Sync(); err != nil {
			return fmt.Errorf("записать %s: %w", tmpOut, err)
		}

		if err := file.Close(); err != nil {
			return fmt.Errorf("закрыть %s: %w", tmpOut, err)
		}

		if err := os.Rename(tmpOut, *out); err != nil {
			return fmt.Errorf("переименовать в %s: %w", *out, err)
		}
	}

	fmt.Fprintf(os.Stderr, "выгружено событий: %d, stream_ordering [%d..%d], без stream: %d, комнат: %d\n",
		manifest.Events, manifest.MinStream, manifest.MaxStream, manifest.NullStream, manifest.Rooms)

	return nil
}
