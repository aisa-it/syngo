package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// isNoRows — общий признак «нет строки» для новых репозиториев.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
