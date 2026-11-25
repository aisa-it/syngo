package main

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// endpointStats - замеры одного эндпоинта.
type endpointStats struct {
	hist     Histogram
	statuses map[int]int64
}

// recorder копит латентность и коды ответов по эндпоинтам. Пишет только при
// включенном замере: разогрев и подготовка в итог не попадают.
type recorder struct {
	measuring atomic.Bool

	mu        sync.Mutex
	endpoints map[string]*endpointStats
	counters  map[string]int64
}

func newRecorder() *recorder {
	return &recorder{endpoints: map[string]*endpointStats{}, counters: map[string]int64{}}
}

// observe - один ответ (status 0 - сетевая ошибка). Латентность ошибок тоже
// пишется: таймаут под нагрузкой - тоже латентность.
func (r *recorder) observe(endpoint string, d time.Duration, status int) {
	if !r.measuring.Load() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.endpoints[endpoint]
	if st == nil {
		st = &endpointStats{statuses: map[int]int64{}}
		r.endpoints[endpoint] = st
	}

	st.hist.Observe(d)
	st.statuses[status]++
}

// count увеличивает именованный счетчик (dropped, limited_timeline, ...).
func (r *recorder) count(name string, delta int64) {
	if !r.measuring.Load() {
		return
	}

	r.mu.Lock()
	r.counters[name] += delta
	r.mu.Unlock()
}

// endpointReport - эндпоинт в отчете.
type endpointReport struct {
	latencySummary

	RPS float64 `json:"rps"`
	// Errors - ответы не 2xx и сетевые ошибки (код "net").
	Errors      int64            `json:"errors"`
	RateLimited int64            `json:"rate_limited"`
	Statuses    map[string]int64 `json:"statuses"`
}

// isError - ответ считается ошибкой: не 2xx или сеть.
func isError(status int) bool { return status < 200 || status > 299 }

// statusKey - код в отчете ("200", "429", "net").
func statusKey(status int) string {
	if status == 0 {
		return "net"
	}

	return strconv.Itoa(status)
}

// snapshot - сводка по эндпоинтам за замер длительностью window.
func (r *recorder) snapshot(window time.Duration) (eps map[string]endpointReport, counters map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	eps = make(map[string]endpointReport, len(r.endpoints))

	for name, st := range r.endpoints {
		rep := endpointReport{latencySummary: st.hist.Summary(), Statuses: map[string]int64{}}
		if window > 0 {
			rep.RPS = round3(float64(st.hist.Count()) / window.Seconds())
		}

		for status, n := range st.statuses {
			rep.Statuses[statusKey(status)] += n

			if isError(status) {
				rep.Errors += n
			}

			if status == 429 {
				rep.RateLimited += n
			}
		}

		eps[name] = rep
	}

	counters = make(map[string]int64, len(r.counters))
	for k, v := range r.counters {
		counters[k] = v
	}

	return eps, counters
}

// totals - сумма запросов и ошибок по всем эндпоинтам.
type totals struct {
	Requests    int64   `json:"requests"`
	RPS         float64 `json:"rps"`
	Errors      int64   `json:"errors"`
	RateLimited int64   `json:"rate_limited"`
	// ErrorRate - доля ошибок без 429 (лимит - ожидаемое поведение под нагрузкой).
	ErrorRate float64 `json:"error_rate"`
}

func sumEndpoints(eps map[string]endpointReport, window time.Duration) totals {
	var t totals

	for _, ep := range eps {
		t.Requests += ep.Count
		t.Errors += ep.Errors
		t.RateLimited += ep.RateLimited
	}

	if window > 0 {
		t.RPS = round3(float64(t.Requests) / window.Seconds())
	}

	if t.Requests > 0 {
		t.ErrorRate = float64(t.Errors-t.RateLimited) / float64(t.Requests)
	}

	return t
}

// sortedEndpoints - имена эндпоинтов по убыванию числа запросов.
func sortedEndpoints(eps map[string]endpointReport) []string {
	names := make([]string, 0, len(eps))
	for name := range eps {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		if eps[names[i]].Count != eps[names[j]].Count {
			return eps[names[i]].Count > eps[names[j]].Count
		}

		return names[i] < names[j]
	})

	return names
}
