package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunScenarioCountsErrors - ошибка сборки /sync считается и не
// попадает в перцентили. Раньше клиент молча выходил на первой ошибке, и при полном
// отказе получался p99=0 и зеленый perf-ratchet.
func TestRunScenarioCountsErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	result := runScenario(context.Background(), func(context.Context) error {
		if calls.Add(1)%2 == 0 {
			return errors.New("отказ")
		}

		return nil
	}, 4, 10)

	if result.samples+result.errors != 40 || result.errors != 20 {
		t.Fatalf("замеров %d, ошибок %d, ожидали 20 и 20", result.samples, result.errors)
	}

	if result.firstErr == nil {
		t.Fatal("первая ошибка не сохранена")
	}

	failed := runScenario(context.Background(), func(context.Context) error { return errors.New("отказ") }, 2, 3)
	if failed.samples != 0 || failed.errors != 6 || failed.p99 != 0 {
		t.Fatalf("полный отказ: %+v", failed)
	}
}

func TestSelectScenarios(t *testing.T) {
	t.Parallel()

	all, err := selectScenarios("all")
	if err != nil || len(all) != 6 {
		t.Fatalf("all = %v, %v", all, err)
	}

	if got, err := selectScenarios("http-delta"); err != nil || len(got) != 1 || got[0] != "http-delta" {
		t.Fatalf("http-delta = %v, %v", got, err)
	}

	if _, err := selectScenarios("nope"); err == nil {
		t.Fatal("неизвестный сценарий принят")
	}
}

// TestHTTPSyncDo - запрос /sync стенда: фильтр lazy_load_members в запросе,
// размер ответа запоминается, ошибка и пустой next_batch - ошибки.
func TestHTTPSyncDo(t *testing.T) {
	t.Parallel()

	var gotQuery string

	body := `{"next_batch":"v1.e5"}`
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery

		if r.URL.Query().Get("since") == "bad" {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		_, _ = w.Write([]byte(body))
	})

	h := &httpSync{handler: handler, token: "t", filter: `{"room":{"state":{"lazy_load_members":true}}}`}

	next, err := h.do(context.Background(), "v1.e1")
	if err != nil || next != "v1.e5" {
		t.Fatalf("do = %q, %v", next, err)
	}

	if !strings.Contains(gotQuery, "filter=") || !strings.Contains(gotQuery, "since=v1.e1") {
		t.Fatalf("запрос без фильтра или since: %s", gotQuery)
	}

	if h.lastBytes.Load() != int64(len(body)) {
		t.Fatalf("размер ответа %d", h.lastBytes.Load())
	}

	if err := h.sync(context.Background(), "bad"); err == nil {
		t.Fatal("ошибка /sync не возвращена")
	}

	if benchUserID("bench.local") != "@bench:bench.local" {
		t.Fatal(benchUserID("bench.local"))
	}
}
