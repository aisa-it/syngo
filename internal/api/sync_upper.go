package api

import "github.com/aisa-it/syngo/internal/core"

// filterTimelineAboveUpper выкидывает события новее зафиксированной верхней
// границы /sync и убирает комнаты, где не осталось ни одного события.
// next_batch = upper, поэтому отдавать более новые события нельзя — иначе на
// следующем /sync они придут повторно.
func filterTimelineAboveUpper(data core.SyncData, upper int64) {
	if upper <= 0 {
		return
	}

	for roomID, events := range data.Timeline {
		kept := make([]core.Event, 0, len(events))

		for i := range events {
			if events[i].StreamOrdering > upper {
				continue
			}

			kept = append(kept, events[i])
		}

		if len(kept) == 0 {
			delete(data.Timeline, roomID)

			continue
		}

		data.Timeline[roomID] = kept
	}
}
