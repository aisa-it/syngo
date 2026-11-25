package api

import (
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestFilterTimelineAboveUpper — события с позицией выше зафиксированной верхней
// границы не отдаются: next_batch их не покрывает, и на следующем /sync клиент
// получил бы их повторно.
func TestFilterTimelineAboveUpper(t *testing.T) {
	t.Parallel()

	data := core.SyncData{Timeline: map[string][]core.Event{
		"!r:test": {
			{EventID: "$a", StreamOrdering: 5},
			{EventID: "$b", StreamOrdering: 9},
		},
		"!new:test": {
			{EventID: "$c", StreamOrdering: 20},
		},
	}}

	filterTimelineAboveUpper(data, 6)

	if got := len(data.Timeline["!r:test"]); got != 1 {
		t.Fatalf("в !r:test осталось %d событий, ожидали 1", got)
	}

	if _, ok := data.Timeline["!new:test"]; ok {
		t.Fatal("комната, где все события новее upper, должна исчезнуть из ответа")
	}
}
