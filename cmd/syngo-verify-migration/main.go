// Command syngo-verify-migration сверяет импорт с БД Synapse: число событий и
// границы stream_ordering, число событий по
// комнатам, множества event_id и раздел событий (timeline или отклоненные),
// текущее состояние комнат с current_state_events, удаленные события,
// текущее членство, sequence stream_ordering, пользователей и алиасы, медиа
// (размер, карантин, sha256; превью ссылок не переносятся и не сверяются).
//
//	syngo-verify-migration -synapse-dsn "$SYNAPSE_DSN" -dsn "$PSQL_DSN" -report verify.txt
//
// Код возврата 1, если есть расхождения: отчет перечисляет их с примерами.
// Запускать после импорта членства и медиа (или с -skip-media) и до cutover
// (после него у нас появятся новые события).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aisa-it/syngo/internal/synapseexport"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-verify-migration: ", err)
	}
}

func run() error {
	var (
		srcDSN     = flag.String("synapse-dsn", os.Getenv("SYNAPSE_DSN"), "DSN БД Synapse")
		dstDSN     = flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN БД synapse-go")
		reportPath = flag.String("report", "", "куда записать отчет (кроме stdout)")
		maxDiffs   = flag.Int("max-diffs", 20, "примеров расхождений на проверку")
		serverName = flag.String("server-name", "", "сверять членство только пользователей этого сервера (как syngo-import -server-name)")
		skipMedia  = flag.Bool("skip-media", false, "не сверять медиа (еще не импортировано)")
	)

	flag.Parse()

	if *srcDSN == "" || *dstDSN == "" {
		return errors.New("нужны -synapse-dsn и -dsn")
	}

	ctx := context.Background()

	src, err := pgxpool.New(ctx, *srcDSN)
	if err != nil {
		return fmt.Errorf("подключение к Synapse: %w", err)
	}

	defer src.Close()

	dst, err := pgxpool.New(ctx, *dstDSN)
	if err != nil {
		return fmt.Errorf("подключение к synapse-go: %w", err)
	}

	defer dst.Close()

	report, err := synapseexport.Verify(ctx, src, dst, synapseexport.VerifyOptions{
		MaxDiffs: *maxDiffs, ServerName: *serverName, SkipMedia: *skipMedia,
	})
	if err != nil {
		return err
	}

	text := report.Text()
	fmt.Print(text)

	if *reportPath != "" {
		//nolint:gosec // путь к отчету задает оператор миграции флагом -report
		if err := os.WriteFile(*reportPath, []byte(text), 0o600); err != nil {
			return fmt.Errorf("записать отчет: %w", err)
		}
	}

	if !report.OK() {
		return fmt.Errorf("расхождений: %d", len(report.Problems))
	}

	return nil
}
