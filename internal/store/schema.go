package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrSchemaMismatch - схема БД не совпадает с той, что ожидает бинарь.
var ErrSchemaMismatch = errors.New("store: database schema mismatch")

// SchemaVersion читает версию схемы из таблицы golang-migrate. found=false -
// миграции не применялись (таблицы schema_migrations нет или она пуста).
func (db *DB) SchemaVersion(ctx context.Context) (version uint, dirty, found bool, err error) {
	var v int64

	err = db.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty)

	var pgErr *pgconn.PgError

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, false, nil
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table
		return 0, false, false, nil
	case err != nil:
		return 0, false, false, fmt.Errorf("store: read schema version: %w", err)
	}

	if v < 0 {
		return 0, dirty, true, nil
	}

	return uint(v), dirty, true, nil
}

// CheckSchema сверяет версию схемы с ожидаемой (последняя встроенная миграция)
// и отказывает на непримененной, недокатанной (dirty) или чужой схеме:
// иначе новый бинарь на старой схеме отвечает 500 на части запросов.
func (db *DB) CheckSchema(ctx context.Context, want uint) error {
	version, dirty, found, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	return compareSchema(version, dirty, found, want)
}

func compareSchema(version uint, dirty, found bool, want uint) error {
	switch {
	case !found:
		return fmt.Errorf("%w: migrations are not applied (expected version %d); "+
			"run `syngo-migrate -up` before starting syngod", ErrSchemaMismatch, want)
	case dirty:
		return fmt.Errorf("%w: version %d is dirty (a migration failed halfway); "+
			"fix the schema manually, then `syngo-migrate -force %d` and `syngo-migrate -up`",
			ErrSchemaMismatch, version, version-1)
	case version < want:
		return fmt.Errorf("%w: database is at version %d, this build expects %d; "+
			"run `syngo-migrate -up`", ErrSchemaMismatch, version, want)
	case version > want:
		return fmt.Errorf("%w: database is at version %d, newer than this build expects (%d); "+
			"deploy a matching syngod build or roll the schema back explicitly", ErrSchemaMismatch, version, want)
	}

	return nil
}
