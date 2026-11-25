package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestAuditDetailsClientIP - в details аудита admin попадает IP клиента из
// контекста запроса; без него поля нет, явное ip не перетирается.
func TestAuditDetailsClientIP(t *testing.T) {
	t.Parallel()

	decode := func(ctx context.Context, details map[string]any) map[string]any {
		t.Helper()

		raw, err := auditDetails(ctx, details)
		if err != nil {
			t.Fatalf("auditDetails: %v", err)
		}

		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}

		return out
	}

	ctx := core.WithClientIP(context.Background(), "203.0.113.10")

	if got := decode(ctx, map[string]any{"device_id": "D"}); got["ip"] != "203.0.113.10" || got["device_id"] != "D" {
		t.Fatalf("details = %v, ожидали ip клиента", got)
	}

	if got := decode(context.Background(), nil); got["ip"] != nil {
		t.Fatalf("без IP в контексте details = %v", got)
	}

	if got := decode(ctx, map[string]any{"ip": "explicit"}); got["ip"] != "explicit" {
		t.Fatalf("явное ip перетерто: %v", got)
	}
}
