package migrations_test

import (
	"os"
	"testing"
	"testing/fstest"

	"github.com/aisa-it/syngo/migrations"
)

// TestLatestMatchesDirectory - встроенный набор совпадает с каталогом: последняя
// миграция в бинаре та же, что в репозитории.
func TestLatestMatchesDirectory(t *testing.T) {
	t.Parallel()

	embedded, err := migrations.Latest(migrations.FS)
	if err != nil {
		t.Fatalf("Latest(FS) = %v", err)
	}

	onDisk, err := migrations.Latest(os.DirFS("."))
	if err != nil {
		t.Fatalf("Latest(dir) = %v", err)
	}

	if embedded != onDisk || embedded == 0 {
		t.Fatalf("встроено %d, в каталоге %d", embedded, onDisk)
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"0001_init.up.sql":     {},
		"0001_init.down.sql":   {},
		"0010_more.up.sql":     {},
		"0010_more.down.sql":   {},
		"0002_media.up.sql":    {},
		"README.md":            {},
		"0099_broken.down.sql": {},
	}

	got, err := migrations.Latest(fsys)
	if err != nil || got != 10 {
		t.Fatalf("Latest() = %d, %v; ожидали 10", got, err)
	}

	if _, err := migrations.Latest(fstest.MapFS{"README.md": {}}); err == nil {
		t.Fatal("пустой набор должен давать ошибку")
	}
}
