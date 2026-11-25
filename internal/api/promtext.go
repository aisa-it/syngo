package api

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Гистограммы и счетчики с метками в текстовом формате Prometheus.
// Квантили в процессе (прежний syngo_sync_duration_seconds{quantile}) нельзя
// агрегировать между инстансами и окнами; гистограмма считается в Prometheus
// через histogram_quantile(sum by (le) (rate(..._bucket[5m]))).

// latencyBuckets - границы корзин длительности запросов, секунды.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// histogram - одна гистограмма (без меток).
type histogram struct {
	bounds []float64
	counts []uint64 // по корзинам, не накопительно; последняя - +Inf
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	idx := sort.SearchFloat64s(h.bounds, v)
	h.counts[idx]++
	h.sum += v
	h.count++
}

// metricVec - семейство счетчиков или гистограмм с метками.
type metricVec struct {
	name, help string
	labels     []string
	bounds     []float64 // nil - счетчик

	mu       sync.Mutex
	counters map[string]uint64
	hists    map[string]*histogram
}

func newCounterVec(name, help string, labels ...string) *metricVec {
	return &metricVec{name: name, help: help, labels: labels, counters: map[string]uint64{}}
}

func newHistogramVec(name, help string, bounds []float64, labels ...string) *metricVec {
	return &metricVec{name: name, help: help, labels: labels, bounds: bounds, hists: map[string]*histogram{}}
}

// key - метки в виде `a="x",b="y"` (значения экранированы).
func (v *metricVec) key(values []string) string {
	var b strings.Builder

	for i, name := range v.labels {
		if i > 0 {
			b.WriteByte(',')
		}

		value := ""
		if i < len(values) {
			value = values[i]
		}

		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(labelEscaper.Replace(value))
		b.WriteByte('"')
	}

	return b.String()
}

// labelEscaper - экранирование значения метки по формату Prometheus.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// inc увеличивает счетчик с метками.
func (v *metricVec) inc(values ...string) {
	k := v.key(values)

	v.mu.Lock()
	v.counters[k]++
	v.mu.Unlock()
}

// observe добавляет измерение в гистограмму с метками.
func (v *metricVec) observe(d time.Duration, values ...string) {
	k := v.key(values)

	v.mu.Lock()

	h, ok := v.hists[k]
	if !ok {
		h = newHistogram(v.bounds)
		v.hists[k] = h
	}

	h.observe(d.Seconds())
	v.mu.Unlock()
}

// snapshot - копия гистограммы по меткам (тесты).
func (v *metricVec) snapshot(values ...string) (count uint64, sum float64) {
	k := v.key(values)

	v.mu.Lock()
	defer v.mu.Unlock()

	if h, ok := v.hists[k]; ok {
		return h.count, h.sum
	}

	return v.counters[k], 0
}

// write выводит семейство в текстовом формате, ряды по алфавиту меток.
func (v *metricVec) write(w io.Writer) {
	v.mu.Lock()
	defer v.mu.Unlock()

	kind := "counter"
	if v.bounds != nil {
		kind = "histogram"
	}

	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.name, v.help, v.name, kind)

	if v.bounds == nil {
		for _, k := range sortedKeys(v.counters) {
			fmt.Fprintf(w, "%s%s %d\n", v.name, braces(k), v.counters[k])
		}

		return
	}

	for _, k := range sortedKeys(v.hists) {
		h := v.hists[k]
		prefix := k
		if prefix != "" {
			prefix += ","
		}

		var cumulative uint64

		for i, bound := range h.bounds {
			cumulative += h.counts[i]
			fmt.Fprintf(w, "%s_bucket{%sle=\"%g\"} %d\n", v.name, prefix, bound, cumulative)
		}

		cumulative += h.counts[len(h.bounds)]
		fmt.Fprintf(w, "%s_bucket{%sle=\"+Inf\"} %d\n", v.name, prefix, cumulative)
		fmt.Fprintf(w, "%s_sum%s %.6f\n", v.name, braces(k), h.sum)
		fmt.Fprintf(w, "%s_count%s %d\n", v.name, braces(k), h.count)
	}
}

func braces(labels string) string {
	if labels == "" {
		return ""
	}

	return "{" + labels + "}"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
