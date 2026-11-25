package testdb_test

import (
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/testdb"
)

// recorder перехватывает Fatalf/Skip, чтобы проверить решение DSN без
// остановки самого теста.
type recorder struct {
	testing.TB

	fatal, skipped string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }

func (r *recorder) Skip(args ...any) { r.skipped = fmt.Sprint(args...) }

// TestDSNRefusesNonTestDatabase - интеграционные тесты не работают с
// базой без "test" в имени (раньше Down() выполнялся на любом SYNGOD_TEST_DSN).
func TestDSNRefusesNonTestDatabase(t *testing.T) {
	t.Setenv(testdb.EnvDSN, "postgres://syngo:syngo@127.0.0.1:5432/syngo?sslmode=disable")

	r := &recorder{TB: t}
	testdb.DSN(r)

	if r.fatal == "" {
		t.Fatal("DSN() приняла боевую базу syngo")
	}

	t.Setenv(testdb.EnvDSN, "postgres://syngo:syngo@127.0.0.1:5432/syngo_test?sslmode=disable")

	r = &recorder{TB: t}
	if got := testdb.DSN(r); r.fatal != "" || got == "" {
		t.Fatalf("DSN() отвергла тестовую базу: %s", r.fatal)
	}

	t.Setenv(testdb.EnvDSN, "")

	r = &recorder{TB: t}
	testdb.DSN(r)

	if r.skipped == "" {
		t.Fatal("без SYNGOD_TEST_DSN тест должен пропускаться")
	}
}
