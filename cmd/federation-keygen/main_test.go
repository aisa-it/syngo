package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeygenRefusesOverwrite - существующий ключ не перезаписывается
// без -force (раньше перезаписывался молча, сервер терял идентичность).
func TestKeygenRefusesOverwrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(path, []byte("ed25519 a_1 OLDSEED\n"), 0o644); err != nil { //nolint:gosec // проверяем исправление прав
		t.Fatal(err)
	}

	err := run([]string{"-out", path}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("run() = %v, ожидали отказ без -force", err)
	}

	if data, _ := os.ReadFile(path); string(data) != "ed25519 a_1 OLDSEED\n" { //nolint:gosec // путь из t.TempDir
		t.Fatalf("ключ изменен без -force: %q", data)
	}

	if err := run([]string{"-out", path, "-force"}, io.Discard); err != nil {
		t.Fatalf("run(-force) = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("права ключа %o, ожидали 600 и на существовавшем файле", perm)
	}
}

// TestKeygenStdoutOnlyExplicit - seed печатается только по -out -, без -out - ошибка.
func TestKeygenStdoutOnlyExplicit(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := run(nil, &out); err == nil {
		t.Fatal("без -out ожидали ошибку")
	}

	if strings.Contains(out.String(), "ed25519 ") {
		t.Fatalf("seed напечатан без явного -out -: %q", out.String())
	}

	out.Reset()

	if err := run([]string{"-out", "-"}, &out); err != nil || !strings.Contains(out.String(), "ed25519 a_1 ") {
		t.Fatalf("run(-out -) = %v, вывод %q", err, out.String())
	}
}

// TestKeygenEmptyPlaceholder - пустой файл-заглушка (bind-mount стенда) не мешает генерации.
func TestKeygenEmptyPlaceholder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "a.key")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"-out", path}, io.Discard); err != nil {
		t.Fatalf("run() на пустом файле = %v", err)
	}
}
