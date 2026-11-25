package state

// Внутренние функции для тестов пакета state_test.

// AuthDifference - auth difference наборов состояний.
func AuthDifference(sets []StateMap, events map[string]Event) map[string]struct{} {
	return authDifference(sets, events)
}

// ConflictedSubgraph - conflicted subgraph v2.1.
func ConflictedSubgraph(conflicted map[string]struct{}, events map[string]Event) map[string]struct{} {
	return conflictedSubgraph(conflicted, events)
}

// SenderPowerLevel - power level отправителя по auth_events события.
func SenderPowerLevel(ev Event, events map[string]Event) int {
	return newResolution(events).senderPowerLevel(ev)
}

// PowerSort - reverse topological power ordering событий ids.
func PowerSort(ids []string, events map[string]Event) []string {
	full := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		full[id] = struct{}{}
	}

	return newResolution(events).reverseTopologicalPowerSort(ids, full)
}
