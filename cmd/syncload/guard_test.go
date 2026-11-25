package main

import (
	"os"
	"strings"
	"testing"
)

// TestCheckBenchDB - syncload не пишет бенч-данные в базу без метки
// test/bench/perf в имени.
func TestCheckBenchDB(t *testing.T) {
	t.Parallel()

	if err := checkBenchDB("postgres://syngo:pw@db:5432/syngo", false); err == nil || !strings.Contains(err.Error(), "allow-any-db") {
		t.Fatalf("боевая база: %v, ожидали отказ", err)
	}

	for _, dsn := range []string{
		"postgres://syngo:pw@db:5432/syngo_perf?sslmode=disable",
		"postgres://syngo:pw@db:5432/syngo_test",
		"postgres://syngo:pw@db:5432/bench",
	} {
		if err := checkBenchDB(dsn, false); err != nil {
			t.Errorf("%s: %v", dsn, err)
		}
	}

	if err := checkBenchDB("postgres://syngo:pw@db:5432/syngo", true); err != nil {
		t.Fatalf("-allow-any-db: %v", err)
	}
}

// TestSyncloadIgnoresServerDSN - DSN syngod (SYNGOD_DATABASE_DSN) не подхватывается
// по умолчанию: раньше бенч молча писал в базу сервера.
func TestSyncloadIgnoresServerDSN(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(src), `os.Getenv("SYNGOD_DATABASE_DSN")`) {
		t.Fatal("syncload по умолчанию читает SYNGOD_DATABASE_DSN")
	}
}
