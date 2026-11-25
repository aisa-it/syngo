package importmedia_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/importmedia"
)

// TestImportWorkersKeepOrder - копирование в несколько потоков
// (окно простоя), строки отчета - в порядке дампа, все медиа перенесены.
func TestImportWorkersKeepOrder(t *testing.T) {
	t.Parallel()

	importer, repo, source := newImporter(t)
	importer.SetWorkers(4)

	records := make([]importmedia.Record, 0, 40)

	for i := range 40 {
		id := fmt.Sprintf("worker%04d", i)
		payload := []byte("payload " + id)
		writeSynapseFile(t, source, id, payload)
		records = append(records, importmedia.Record{MediaID: id, MediaLen: int64(len(payload))})
	}

	report, err := importer.Import(context.Background(), records)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if report.Copied != 40 || report.HasProblems() || len(repo.items) != 40 {
		t.Fatalf("отчёт: copied=%d problems=%t items=%d", report.Copied, report.HasProblems(), len(repo.items))
	}

	for i := range report.Outcomes {
		if want := records[i].MediaID; report.Outcomes[i].MediaID != want {
			t.Fatalf("строка %d отчета: %s, ожидали %s", i, report.Outcomes[i].MediaID, want)
		}
	}
}
