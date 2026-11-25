//go:build race

package state_test

// raceSlowdown - во сколько раз race detector замедляет разрешение: порог времени
// TestPowerSortScaling (2 с) задан для обычной сборки.
const raceSlowdown = 10
