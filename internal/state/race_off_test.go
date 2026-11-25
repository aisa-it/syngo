//go:build !race

package state_test

// raceSlowdown - без race detector порог времени TestPowerSortScaling (2 с) без поправки.
const raceSlowdown = 1
