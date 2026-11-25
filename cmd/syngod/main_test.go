package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

func devConfig(t *testing.T) config.Config {
	t.Helper()

	cfg := config.Default()
	cfg.Env = "dev"
	cfg.Matrix.ServerName = "localhost"
	cfg.Media.Directory = t.TempDir()

	return cfg
}

// TestCheckStartupMedia - недоступный на запись каталог медиа - отказ
// старта, а не 500 на каждом upload.
func TestCheckStartupMedia(t *testing.T) {
	t.Parallel()

	cfg := devConfig(t)
	if err := checkStartup(context.Background(), cfg, nil); err != nil {
		t.Fatalf("dev без БД с доступным каталогом: %v", err)
	}

	// Файл вместо каталога: MkdirAll не создаст каталог - то же, что том без прав.
	blocker := filepath.Join(t.TempDir(), "media")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg.Media.Directory = filepath.Join(blocker, "sub")

	err := checkStartup(context.Background(), cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "media.directory") {
		t.Fatalf("checkStartup() = %v, ожидали ошибку про media.directory", err)
	}
}

// TestCheckStartupRequiresDatabaseInProd - без БД стартует только dev/test.
func TestCheckStartupRequiresDatabaseInProd(t *testing.T) {
	t.Parallel()

	cfg := devConfig(t)
	cfg.Env = "prod"

	if err := checkStartup(context.Background(), cfg, nil); err == nil {
		t.Fatal("prod без БД должен отказывать в старте")
	}
}

// TestLoggerRedactsSecrets - логгер процесса маскирует секреты.
func TestLoggerRedactsSecrets(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"json", "text"} {
		var out strings.Builder

		log := newLogger(config.Log{Level: "debug", Format: format}, &out)
		log.Info("probe", "authorization", "Bearer TOPSECRET1", "password", "TOPSECRET2",
			"uri", "/sync?access_token=TOPSECRET3", "user_id", "@alice:test.local")

		got := out.String()
		if strings.Contains(got, "TOPSECRET") {
			t.Fatalf("%s: секрет в логе: %s", format, got)
		}

		if !strings.Contains(got, "@alice:test.local") {
			t.Fatalf("%s: обычное поле потеряно: %s", format, got)
		}
	}
}
