//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/store"
	"github.com/aisa-it/syngo/migrations"
)

// TestReadySchema - readiness проверяет не только доступность БД, но
// и версию схемы; текст ошибки - только на служебном листенере.
func TestReadySchema(t *testing.T) {
	srv := newIntegrationServer(t)

	want, err := migrations.Latest(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}

	ready := func(h http.Handler) (int, readyResponse) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", http.NoBody))

		var body readyResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &body)

		return rec.Code, body
	}

	srv.SetSchemaVersion(want)

	if code, body := ready(srv.Handler()); code != http.StatusOK || body.Checks["schema"] != "ok" {
		t.Fatalf("актуальная схема: %d %+v", code, body)
	}

	// Бинарь ждет более новую схему, чем в БД (миграция не применена).
	srv.SetSchemaVersion(want + 1)

	if code, body := ready(srv.Handler()); code != http.StatusServiceUnavailable || body.Checks["schema"] != "fail" {
		t.Fatalf("отставшая схема на публичном: %d %+v", code, body)
	}

	if code, body := ready(srv.OpsHandler()); code != http.StatusServiceUnavailable ||
		body.Checks["schema"] == "fail" || body.Checks["schema"] == "" {
		t.Fatalf("отставшая схема на служебном: %d %+v", code, body)
	}
}

// TestMetricsStorage - пул соединений, размер базы и готовность
// числом видны в /metrics.
func TestMetricsStorage(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Media.Directory = t.TempDir()

	rec := httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	body := rec.Body.String()

	for _, want := range []string{
		"syngo_ready 1",
		`syngo_ready_check{check="database"} 1`,
		`syngo_db_pool_connections{state="idle"}`,
		"syngo_db_pool_max_connections ",
		"syngo_db_pool_empty_acquires_total ",
		"syngo_db_size_bytes ",
		"syngo_media_disk_free_bytes ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("в /metrics нет %q\n%s", want, body)
		}
	}
}

// TestMonitoringReferencesExportedMetrics - алерты и дашборд
// ссылаются только на метрики, которые syngod (или backup.sh) реально отдает:
// переименование метрики без правки алертов молча выключает алерт.
func TestMonitoringReferencesExportedMetrics(t *testing.T) {
	srv := newIntegrationServer(t)
	srv.cfg.Media.Directory = t.TempDir()
	srv.SetOutboxStats(func(context.Context) (store.OutboxStats, error) { return store.OutboxStats{}, nil })
	srv.SetInstanceLock(func() bool { return true })
	srv.syncDuration.observe(time.Millisecond)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_matrix/client/versions", http.NoBody))

	rec := httptest.NewRecorder()
	srv.OpsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))

	exported := map[string]bool{}

	for line := range strings.Lines(rec.Body.String()) {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			fields := strings.Fields(name)
			exported[fields[0]] = true

			if fields[1] == "histogram" {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					exported[fields[0]+suffix] = true
				}
			}
		}
	}

	backupScript, err := os.ReadFile("../../deploy/backup.sh")
	if err != nil {
		t.Fatal(err)
	}

	metricRe := regexp.MustCompile(`\b(?:syngo|go|process)_[a-z0-9_]+`)

	for _, file := range []string{"../../deploy/monitoring/alerts.yml", "../../deploy/monitoring/grafana-dashboard.json"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		for _, name := range metricRe.FindAllString(string(data), -1) {
			if strings.HasPrefix(name, "syngo_backup_") {
				if !strings.Contains(string(backupScript), name) {
					t.Errorf("%s: %s не пишет deploy/backup.sh", file, name)
				}

				continue
			}

			if !exported[name] {
				t.Errorf("%s: метрики %s нет в /metrics", file, name)
			}
		}
	}
}
