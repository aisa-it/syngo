// Command syngo-recompute-state материализует room_state из уже импортированных events.
//
// Идемпотентно: повторный прогон даёт тот же результат. Нужен после импорта истории
// старого формата без строк kind=state (или для баз, импортированных до появления
// материализации). Это эвристика "последнее state-событие по stream_ordering": для
// дампа syngo-synapse-export состояние приходит из current_state_events Synapse, и
// пересчет перезапишет его, расходясь на форках.
//
//	syngo-recompute-state -dsn "$PSQL_DSN" -batch 100 -report state-report.txt
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aisa-it/syngo/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-recompute-state: ", err)
	}
}

func run() error {
	var (
		dsn        = flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN PostgreSQL")
		batch      = flag.Int("batch", 100, "сколько комнат обрабатывать за раз")
		reportPath = flag.String("report", "", "куда записать отчёт (по умолчанию stdout)")
	)

	flag.Parse()

	if *dsn == "" {
		return errors.New("нужен -dsn или SYNGOD_DATABASE_DSN")
	}

	db, err := store.Open(context.Background(), *dsn, 8, 0)
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}

	defer db.Close()

	report, err := db.RecomputeRoomState(context.Background(), *batch)
	if err != nil {
		return err
	}

	text := fmt.Sprintf("room_state: rooms=%d state_rows=%d membership_join=%d mismatches=%d\n",
		report.Rooms, report.StateRows, report.MembershipRows, len(report.Mismatches))

	for _, m := range report.Mismatches {
		text += "mismatch: " + m + "\n"
	}

	fmt.Print(text)

	if *reportPath != "" {
		//nolint:gosec // путь к отчёту задаёт оператор
		if err := os.WriteFile(*reportPath, []byte(text), 0o600); err != nil {
			return fmt.Errorf("записать отчёт: %w", err)
		}
	}

	if report.HasMismatches() {
		return fmt.Errorf("расхождения room_state и room_memberships: %d", len(report.Mismatches))
	}

	return nil
}
