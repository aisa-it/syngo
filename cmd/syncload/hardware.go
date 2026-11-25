package main

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/benchenv"
	"github.com/aisa-it/syngo/internal/store"
)

// detectHardware - на чем снят замер. Цифры бенча без железа не сравнимы: README
// приводил p99 без записи хоста, и на другом хосте они не воспроизвелись.
// perf-ratchet сравнивает время только на том же железе.
// Формат общий с cmd/loadgen (internal/benchenv).
func detectHardware(ctx context.Context, db *store.DB) benchenv.Hardware {
	var version string
	if err := db.Pool().QueryRow(ctx, `SHOW server_version`).Scan(&version); err == nil {
		version = "PostgreSQL " + version
	}

	return benchenv.DetectHardware(version)
}

// rssKB - RSS процесса в килобайтах (для отчета и perf-ratchet).
func rssKB() int64 { return benchenv.SelfRSSKB() }

// rss - RSS процесса для вывода.
func rss() string {
	if kb := rssKB(); kb > 0 {
		return fmt.Sprintf("%d kB", kb)
	}

	return "unknown"
}
