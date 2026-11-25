// Package dbguard защищает боевую БД от разрушительных команд:
// интеграционные тесты, откат миграций и генераторы нагрузки работают только с
// базой, имя которой явно помечено как тестовое.
package dbguard

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DatabaseName извлекает имя базы из DSN (URL или key=value).
func DatabaseName(dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("dbguard: parse dsn: %w", err)
	}

	return cfg.Database, nil
}

// IsTestDatabase сообщает, что имя базы содержит одну из меток (без учета регистра).
func IsTestDatabase(dsn string, marks ...string) (isTest bool, name string, err error) {
	name, err = DatabaseName(dsn)
	if err != nil {
		return false, "", err
	}

	lower := strings.ToLower(name)
	for _, mark := range marks {
		if strings.Contains(lower, mark) {
			return true, name, nil
		}
	}

	return false, name, nil
}

// RequireTestDatabase возвращает ошибку, если имя базы не содержит "test":
// так разрушительное действие не сработает на боевой БД по ошибке в DSN.
func RequireTestDatabase(dsn string) error {
	ok, name, err := IsTestDatabase(dsn, "test")
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("dbguard: database %q is not a test database (name must contain \"test\")", name)
	}

	return nil
}
