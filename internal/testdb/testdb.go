// Package testdb - общий вход интеграционных тестов в PostgreSQL.
// Тесты делают migrate Down() (удаление всех таблиц), поэтому DSN из
// SYNGOD_TEST_DSN принимается, только если имя базы содержит "test".
package testdb

import (
	"os"
	"testing"

	"github.com/aisa-it/syngo/internal/dbguard"
)

// EnvDSN - переменная с DSN тестовой базы.
const EnvDSN = "SYNGOD_TEST_DSN"

// DSN возвращает DSN тестовой базы: пропускает тест без SYNGOD_TEST_DSN и
// останавливает его, если база не тестовая.
func DSN(tb testing.TB) string {
	tb.Helper()

	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		tb.Skip(EnvDSN + " не задан - интеграционные тесты пропущены")
	}

	if err := dbguard.RequireTestDatabase(dsn); err != nil {
		tb.Fatalf("%s: %v - отказ, чтобы не стереть рабочую базу", EnvDSN, err)
	}

	return dsn
}
