package main

import (
	"io"
	"strings"
	"testing"
)

const (
	prodDSN = "postgres://syngo:pw@127.0.0.1:1/syngo?sslmode=disable"
	testDSN = "postgres://syngo:pw@127.0.0.1:1/syngo_test?sslmode=disable"
)

// TestDownRequiresSteps - -down без числа больше не откатывает все
// миграции, а отказывает до подключения к БД.
func TestDownRequiresSteps(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"-dsn", testDSN, "-down"},
		{"-dsn", testDSN, "-down", "0"},
		{"-dsn", testDSN, "-down", "all"},
	} {
		err := run(args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "-down") {
			t.Errorf("run(%v) = %v, ожидали отказ про число шагов", args, err)
		}
	}
}

// TestDownRequiresConfirmationOnNonTestDB - откат на базе без "test"
// в имени - только с -yes.
func TestDownRequiresConfirmationOnNonTestDB(t *testing.T) {
	t.Parallel()

	err := run([]string{"-dsn", prodDSN, "-down", "1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("run(-down 1 на syngo) = %v, ожидали требование -yes", err)
	}

	for _, args := range [][]string{
		{"-dsn", prodDSN, "-down", "1", "-yes"},
		{"-dsn", prodDSN, "-yes", "-down", "1"},
		{"-dsn", testDSN, "-down", "2"},
	} {
		opts, err := parseArgs(args, io.Discard)
		if err != nil {
			t.Errorf("parseArgs(%v) = %v, ожидали успех", args, err)

			continue
		}

		if !opts.down || opts.downSteps < 1 {
			t.Errorf("parseArgs(%v) = %+v", args, opts)
		}
	}
}

func TestParseArgsRejectsExtra(t *testing.T) {
	t.Parallel()

	if _, err := parseArgs([]string{"-dsn", testDSN, "-up", "extra"}, io.Discard); err == nil {
		t.Fatal("лишний аргумент должен давать ошибку")
	}

	if _, err := parseArgs([]string{"-dsn", testDSN}, io.Discard); err == nil {
		t.Fatal("без действия должна быть ошибка")
	}
}
