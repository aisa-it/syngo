//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// syncQueryKey - ключ контекста со счетчиком SQL-запросов одного HTTP-запроса.
type syncQueryKey struct{}

// syncQueryTracer считает запросы, выполненные с контекстом запроса (r.Context()):
// фоновые горутины сервера в счетчик не попадают.
type syncQueryTracer struct{}

func (syncQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if counter, ok := ctx.Value(syncQueryKey{}).(*atomic.Int64); ok {
		counter.Add(1)
	}

	return ctx
}

func (syncQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// syncHTTPQueryBudget - потолок SQL-запросов на один HTTP /sync (ADR-0005 §2:
// число запросов не зависит от числа комнат). Слой данных укладывается в 8
// (internal/store TestSyncLoaderQueryCountIsConstant); остальное - auth, account data,
// to-device, ключи, receipts, typing. Замер на main 20.11.2026 - 16 запросов и в
// initial, и в incremental; рост сверх бюджета - осознанная правка с обоснованием
// в ревью. Затем 12 и 12 - счетчики непрочитанного читаются
// одним запросом из хранимых действий вместо пересчета по правилам (было 5
// запросов: fully_read, позиции receipts, кандидаты, правила, состояние комнат);
// после агрегаций - 12 и 13. Бюджет снижен до 16: с тредами, правками и обрезанным
// timeline (состояние на позиции) initial - 15 (TestSyncAggregationsQueryCountIsConstant).
const syncHTTPQueryBudget = 16

// countedSync выполняет GET /sync и возвращает число SQL-запросов и next_batch.
func countedSync(t *testing.T, server *Server, token, since string) (queries int64, nextBatch string) {
	t.Helper()

	path := "/_matrix/client/v3/sync?timeout=0"
	if since != "" {
		path += "&since=" + since
	}

	var counter atomic.Int64

	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req = req.WithContext(context.WithValue(req.Context(), syncQueryKey{}, &counter))

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]json.RawMessage `json:"join"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.NextBatch == "" {
		t.Fatalf("sync: разбор ответа: %v %.200s", err, rec.Body.String())
	}

	return counter.Load(), resp.NextBatch
}

// TestSyncHTTPQueryCountIsConstant - гейт ADR-0005 на уровне HTTP: число
// SQL-запросов на /sync не растет с числом комнат ни в initial, ни в incremental.
// Тест на слой данных не видит N+1 в обработчике (receipts, unread, ключи по комнатам).
func TestSyncHTTPQueryCountIsConstant(t *testing.T) {
	server := newIntegrationServerWithTracer(t, syncQueryTracer{})
	token := registerHTTP(t, server, "qcount", "QDEV")

	rooms := []string{}
	createRooms := func(n int) {
		t.Helper()

		for range n {
			rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"q"}`, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
			}

			var created struct {
				RoomID string `json:"room_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.RoomID == "" {
				t.Fatalf("createRoom: %v %s", err, rec.Body.String())
			}

			rooms = append(rooms, created.RoomID)
		}
	}

	// sendTo пишет по сообщению в первые n комнат.
	sendTo := func(n int, txn string) {
		t.Helper()

		for i, roomID := range rooms[:n] {
			path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s-%d", roomID, txn, i)
			if rec := do(t, server, http.MethodPut, path, `{"msgtype":"m.text","body":"x"}`, token); rec.Code != http.StatusOK {
				t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
			}
		}
	}

	createRooms(3)
	initialSmall, _ := countedSync(t, server, token, "")

	createRooms(12)
	initialLarge, next := countedSync(t, server, token, "")

	sendTo(3, "small")
	deltaSmall, next := countedSync(t, server, token, next)

	sendTo(15, "large")
	deltaLarge, _ := countedSync(t, server, token, next)

	t.Logf("SQL на /sync: initial 3 комнаты %d, 15 комнат %d; incremental 3 комнаты %d, 15 комнат %d",
		initialSmall, initialLarge, deltaSmall, deltaLarge)

	for name, pair := range map[string][2]int64{
		"initial":     {initialSmall, initialLarge},
		"incremental": {deltaSmall, deltaLarge},
	} {
		small, large := pair[0], pair[1]

		// 0 значит, что трейсер не видит запросы обработчика, и сравнение пустое.
		if small == 0 {
			t.Fatalf("%s: трейсер не посчитал ни одного запроса", name)
		}

		if large != small {
			t.Errorf("%s: SQL-запросов зависит от числа комнат: 3 комнаты → %d, 15 комнат → %d", name, small, large)
		}

		if large > syncHTTPQueryBudget {
			t.Errorf("%s: SQL-запросов %d, бюджет %d", name, large, syncHTTPQueryBudget)
		}
	}
}
