// Command migrate накатывает и откатывает SQL-миграции через golang-migrate.
//
//	migrate -dsn postgres://... -up
//	migrate -dsn postgres://... -down 1 [-yes]   (откат на N шагов)
//	migrate -dsn postgres://... -version
//	migrate -dsn postgres://... -force N         (починка dirty-состояния)
//
// Миграции встроены в бинарь: каталог на диске не нужен, -path
// переопределяет набор (отладка новых миграций).
//
// Откат удаляет таблицы с данными, поэтому:
//   - -down без числа шагов - ошибка (раньше откатывались все миграции);
//   - на базе без "test" в имени -down требует -yes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // драйвер БД
	_ "github.com/golang-migrate/migrate/v4/source/file"     // источник: каталог с SQL
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/aisa-it/syngo/internal/dbguard"
	"github.com/aisa-it/syngo/migrations"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Fatalf("migrate: %v", err)
	}
}

// options - разобранные флаги команды.
type options struct {
	dsn       string
	path      string
	up        bool
	down      bool
	downSteps int
	yes       bool
	version   bool
	force     int
}

func parseArgs(args []string, output io.Writer) (options, error) {
	var opts options

	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&opts.dsn, "dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "PostgreSQL DSN (по умолчанию $SYNGOD_DATABASE_DSN)")
	fs.StringVar(&opts.path, "path", "", "каталог с миграциями (по умолчанию - встроенные в бинарь)")
	fs.BoolVar(&opts.up, "up", false, "применить все миграции")
	fs.BoolVar(&opts.down, "down", false, "откатить N миграций: -down N (число обязательно)")
	fs.BoolVar(&opts.yes, "yes", false, "подтвердить -down на базе без \"test\" в имени")
	fs.BoolVar(&opts.version, "version", false, "показать текущую версию схемы")
	fs.IntVar(&opts.force, "force", -1, "принудительно выставить версию (для починки dirty-состояния)")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}

	if opts.dsn == "" {
		return options{}, errors.New("dsn is required (флаг -dsn или SYNGOD_DATABASE_DSN)")
	}

	if opts.down {
		if fs.NArg() == 0 {
			return options{}, errors.New("-down требует число шагов: -down N (откат всех миграций одной командой запрещен)")
		}

		n, err := strconv.Atoi(fs.Arg(0))
		if err != nil || n < 1 {
			return options{}, fmt.Errorf("-down: число шагов должно быть целым >= 1, получено %q", fs.Arg(0))
		}

		opts.downSteps = n

		// flag останавливается на N: флаги после него (-down 1 -yes) разбираем отдельно.
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return options{}, err
		}
	}

	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("лишние аргументы: %v", fs.Args())
	}

	if opts.down {
		if err := confirmDown(opts.dsn, opts.yes); err != nil {
			return options{}, err
		}
	}

	if opts.force < 0 && !opts.up && !opts.down && !opts.version {
		fs.Usage()

		return options{}, errors.New("нужен один из флагов: -up, -down N, -version, -force N")
	}

	return opts, nil
}

// confirmDown разрешает откат без -yes только на тестовой базе.
func confirmDown(dsn string, yes bool) error {
	isTest, name, err := dbguard.IsTestDatabase(dsn, "test")
	if err != nil {
		return err
	}

	if !isTest && !yes {
		return fmt.Errorf("-down удаляет таблицы с данными: база %q не тестовая, подтвердите флагом -yes "+
			"(и сделайте бэкап: make backup)", name)
	}

	return nil
}

func newMigrate(opts options) (*migrate.Migrate, error) {
	dbURL := "pgx5://" + stripScheme(opts.dsn)

	if opts.path != "" {
		return migrate.New("file://"+opts.path, dbURL)
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, err
	}

	return migrate.NewWithSourceInstance("iofs", src, dbURL)
}

func run(args []string, stdout io.Writer) error {
	opts, err := parseArgs(args, os.Stderr)
	if err != nil {
		return err
	}

	m, err := newMigrate(opts)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			log.Printf("migrate: close: %v %v", srcErr, dbErr)
		}
	}()

	switch {
	case opts.force >= 0:
		if err := m.Force(opts.force); err != nil {
			return fmt.Errorf("force %d: %w", opts.force, err)
		}
	case opts.up:
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("up: %w", err)
		}
	case opts.down:
		if err := m.Steps(-opts.downSteps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("down %d: %w", opts.downSteps, err)
		}
	}

	v, dirty, err := m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			_, _ = fmt.Fprintln(stdout, "версия: нет (схема не накатана)")

			return nil
		}

		return fmt.Errorf("version: %w", err)
	}

	_, _ = fmt.Fprintf(stdout, "версия: %d, dirty: %t\n", v, dirty)

	return nil
}

// stripScheme убирает postgres:// или postgresql:// — драйвер pgx5 ждёт DSN без схемы.
func stripScheme(dsn string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(dsn) > len(prefix) && dsn[:len(prefix)] == prefix {
			return dsn[len(prefix):]
		}
	}

	return dsn
}
