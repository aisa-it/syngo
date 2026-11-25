// Package migrations встраивает SQL-миграции в бинари: syngod сверяет
// по ним версию схемы при старте, migrate применяет их без каталога на диске
// (в distroless-образе каталога migrations нет).
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// FS - файлы миграций формата golang-migrate (NNNN_name.up.sql / .down.sql).
//
//go:embed *.sql
var FS embed.FS

// Latest возвращает номер последней up-миграции из fsys: это версия схемы,
// которую ожидает текущий бинарь.
func Latest(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("migrations: read dir: %w", err)
	}

	var latest uint

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return 0, fmt.Errorf("migrations: unexpected file name %q", name)
		}

		v, err := strconv.ParseUint(prefix, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("migrations: bad version in %q: %w", name, err)
		}

		latest = max(latest, uint(v))
	}

	if latest == 0 {
		return 0, fmt.Errorf("migrations: no *.up.sql files")
	}

	return latest, nil
}
