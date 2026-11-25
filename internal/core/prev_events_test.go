package core

import (
	"reflect"
	"testing"
)

func extremityIDs(events []Event) []string {
	ids := make([]string, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].EventID)
	}

	return ids
}

// TestForwardExtremities - prev_events нового события - только события, на которые
// никто не ссылается, а не все последние события комнаты.
func TestForwardExtremities(t *testing.T) {
	t.Parallel()

	// Линейная история (новые первыми): extremity одна - последнее событие.
	linear := []Event{
		{EventID: "$c", PrevEvents: []string{"$b", "$a"}},
		{EventID: "$b", PrevEvents: []string{"$a"}},
		{EventID: "$a"},
	}
	if got := extremityIDs(forwardExtremities(linear, 10)); !reflect.DeepEqual(got, []string{"$c"}) {
		t.Fatalf("линейная история: %v", got)
	}

	// Развилка: два события ссылаются на одно - обе ветки остаются.
	fork := []Event{
		{EventID: "$remote", PrevEvents: []string{"$a"}},
		{EventID: "$local", PrevEvents: []string{"$a"}},
		{EventID: "$a"},
	}
	if got := extremityIDs(forwardExtremities(fork, 10)); !reflect.DeepEqual(got, []string{"$remote", "$local"}) {
		t.Fatalf("развилка: %v", got)
	}

	// Предел.
	many := []Event{{EventID: "$1"}, {EventID: "$2"}, {EventID: "$3"}}
	if got := forwardExtremities(many, 2); len(got) != 2 {
		t.Fatalf("предел: %d", len(got))
	}
}
