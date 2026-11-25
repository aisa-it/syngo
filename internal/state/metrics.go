package state

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics — метрики разрешения состояния.
//
// Экспортируются в /metrics (текстовый формат Prometheus) и логируются по комнатам
// вызывающей стороной. Счётчики атомарные: Resolve может вызываться конкурентно.
type Metrics struct {
	resolutions atomic.Int64
	conflicts   atomic.Int64
	changed     atomic.Int64
	durationNs  atomic.Int64
	byRoom      sync.Map // room_id -> *roomStats
}

type roomStats struct {
	resolutions atomic.Int64
	conflicts   atomic.Int64
	changed     atomic.Int64
}

// DefaultMetrics — общие метрики процесса.
var DefaultMetrics = &Metrics{}

// Observe записывает результат одного разрешения.
func (m *Metrics) Observe(roomID string, duration time.Duration, conflicted, changed int) {
	m.resolutions.Add(1)
	m.conflicts.Add(int64(conflicted))
	m.changed.Add(int64(changed))
	m.durationNs.Add(duration.Nanoseconds())

	entry, _ := m.byRoom.LoadOrStore(roomID, &roomStats{})

	stats, ok := entry.(*roomStats)
	if !ok {
		return
	}

	stats.resolutions.Add(1)
	stats.conflicts.Add(int64(conflicted))
	stats.changed.Add(int64(changed))
}

// ConflictsByRoom — накопленные конфликты по комнатам (для логов и тестов).
func (m *Metrics) ConflictsByRoom() map[string]int64 {
	out := map[string]int64{}

	m.byRoom.Range(func(key, value any) bool {
		roomID, ok := key.(string)
		if !ok {
			return true
		}

		stats, ok := value.(*roomStats)
		if !ok {
			return true
		}

		out[roomID] = stats.conflicts.Load()

		return true
	})

	return out
}

// WritePrometheus пишет метрики в текстовом формате Prometheus.
func (m *Metrics) WritePrometheus(w io.Writer) {
	resolutions := m.resolutions.Load()
	conflicts := m.conflicts.Load()
	changed := m.changed.Load()

	fmt.Fprintf(w, "# HELP syngo_state_resolution_duration_seconds_total Суммарное время разрешения состояния.\n")
	fmt.Fprintf(w, "# TYPE syngo_state_resolution_duration_seconds_total counter\n")
	fmt.Fprintf(w, "syngo_state_resolution_duration_seconds_total %.6f\n", float64(m.durationNs.Load())/1e9)

	fmt.Fprintf(w, "# HELP syngo_state_resolution_conflicts_total Сумма размеров full conflicted set.\n")
	fmt.Fprintf(w, "# TYPE syngo_state_resolution_conflicts_total counter\n")
	fmt.Fprintf(w, "syngo_state_resolution_conflicts_total %d\n", conflicts)

	fmt.Fprintf(w, "# HELP syngo_state_changed_total Изменённые ключи состояния по итогам разрешения.\n")
	fmt.Fprintf(w, "# TYPE syngo_state_changed_total counter\n")
	fmt.Fprintf(w, "syngo_state_changed_total %d\n", changed)

	fmt.Fprintf(w, "# HELP syngo_state_resolution_total Число вызовов разрешения состояния.\n")
	fmt.Fprintf(w, "# TYPE syngo_state_resolution_total counter\n")
	fmt.Fprintf(w, "syngo_state_resolution_total %d\n", resolutions)
}
