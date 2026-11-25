// Command syngo-import-events переносит историю событий из дампа Synapse.
//
// Стриминг: дамп читается построчно (stdin или файл), события пишутся пачками через
// multi-row INSERT ... ON CONFLICT DO NOTHING. Память ограничена размером пачки.
//
//	# выгрузка из Synapse: все события, с манифестом в конце
//	syngo-synapse-export -synapse-dsn "$SYNAPSE_DSN" -out events.jsonl
//
//	# импорт (комнаты уже должны быть импортированы: syngo-import)
//	syngo-import-events -dsn "$PSQL_DSN" -file events.jsonl -batch 1000 -report events-import.txt \
//	    -checkpoint events.checkpoint -bad-file events-bad.jsonl
//
// Код возврата ненулевой, если хоть одна строка не импортирована, дамп неполный
// (манифест не совпал), event_id не совпал с reference hash PDU или запись
// упала. С -checkpoint повторный запуск продолжает с последней записанной пачки.
//
// Отклоненные, soft-failed и outlier-события пишутся в excluded_events (вне
// timeline и состояния), redaction применяются в конце, состояние комнат
// берется из строк kind=state (current_state_events Synapse).
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
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/importevents"
	"github.com/aisa-it/syngo/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-import-events: ", err)
	}
}

func run() error {
	var (
		dsn        = flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN PostgreSQL")
		file       = flag.String("file", "-", "JSONL-дамп событий ('-' — stdin)")
		batch      = flag.Int("batch", 1000, "размер пачки вставки")
		progress   = flag.Int("progress", 50000, "печатать прогресс каждые N событий (0 — выключить)")
		noState    = flag.Bool("no-recompute-state", false, "не материализовать room_state после импорта")
		reportPath = flag.String("report", "", "куда записать отчёт (по умолчанию stdout)")
		checkpoint = flag.String("checkpoint", "", "файл чекпойнта: продолжить с записанной строки и обновлять после каждой пачки")
		badFile    = flag.String("bad-file", "", "куда копировать строки, которые не удалось импортировать")
		retention  = flag.Duration("redaction-retention", 7*24*time.Hour,
			"сколько хранить оригинал удаленных событий (redaction.retention_period syngod; 0 - не хранить)")
	)

	flag.Parse()

	if *dsn == "" {
		return errors.New("нужен -dsn или SYNGOD_DATABASE_DSN")
	}

	reader, closeFn, err := openDump(*file)
	if err != nil {
		return err
	}

	defer closeFn()

	db, err := store.Open(context.Background(), *dsn, 8, 0)
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}

	defer db.Close()

	// Оригинал цели redaction - в redactions.original_content с received_at
	// события redaction; стирает его syngod по redaction.retention_period.
	db.SetRedactionRetention(*retention)

	opts := importevents.Options{
		BatchSize:      *batch,
		ProgressEvery:  *progress,
		ProgressWriter: os.Stderr,
	}

	if *checkpoint != "" {
		resume, err := readCheckpoint(*checkpoint)
		if err != nil {
			return err
		}

		if resume > 0 {
			fmt.Fprintf(os.Stderr, "продолжение после строки %d (чекпойнт %s)\n", resume, *checkpoint)
		}

		opts.ResumeAfterLine = resume
		opts.Checkpoint = func(line int64) error { return writeCheckpoint(*checkpoint, line) }
	}

	if *badFile != "" {
		//nolint:gosec // путь задаёт оператор миграции флагом -bad-file
		bad, err := os.OpenFile(*badFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("открыть -bad-file: %w", err)
		}

		defer func() { _ = bad.Close() }()

		opts.BadLines = bad
	}

	importer := importevents.New(importevents.NewStoreWriter(db), opts)

	report, importErr := importer.Import(context.Background(), reader)

	text := report.Text()
	fmt.Print(text)

	if *reportPath != "" {
		//nolint:gosec // путь к отчёту задаёт оператор миграции флагом -report
		if err := os.WriteFile(*reportPath, []byte(text), 0o600); err != nil {
			return fmt.Errorf("записать отчёт: %w", err)
		}
	}

	if importErr != nil {
		return importErr
	}

	// Материализация room_state эвристикой - только для дампов без kind=state
	// (старый формат): состояние из current_state_events Synapse точнее.
	if report.StateRooms > 0 {
		fmt.Printf("room_state: из current_state_events источника, комнат %d\n", report.StateRooms)
	} else if !*noState {
		state, err := db.RecomputeRoomState(context.Background(), 100)
		if err != nil {
			return fmt.Errorf("материализация room_state: %w", err)
		}

		fmt.Printf("room_state: rooms=%d state_rows=%d membership_join=%d mismatches=%d\n",
			state.Rooms, state.StateRows, state.MembershipRows, len(state.Mismatches))

		for _, m := range state.Mismatches {
			fmt.Println("state mismatch:", m)
		}
	}

	if report.Bad > 0 || report.IDMismatch > 0 {
		return fmt.Errorf("импорт завершен с ошибками: bad=%d, event_id mismatch=%d (см. отчет)", report.Bad, report.IDMismatch)
	}

	return nil
}

// readCheckpoint читает номер строки из файла чекпойнта (нет файла - 0).
func readCheckpoint(path string) (int64, error) {
	data, err := os.ReadFile(path) //nolint:gosec // путь задаёт оператор миграции
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("чекпойнт: %w", err)
	}

	line, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || line < 0 {
		return 0, fmt.Errorf("чекпойнт %s: не число строки: %q", path, data)
	}

	return line, nil
}

// writeCheckpoint атомарно записывает номер строки (temp + rename).
func writeCheckpoint(path string, line int64) error {
	tmp := path + ".tmp"

	//nolint:gosec // путь задаёт оператор миграции
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(line, 10)+"\n"), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

// openDump открывает файл дампа или stdin.
func openDump(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}

	f, err := os.Open(path) //nolint:gosec // путь задаёт оператор миграции
	if err != nil {
		return nil, nil, fmt.Errorf("открыть дамп: %w", err)
	}

	return f, func() { _ = f.Close() }, nil
}
