package main

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// weighted - действие и его вес.
type weighted struct {
	name   string
	weight float64
}

// picker выбирает действие пропорционально весу.
type picker struct {
	names []string
	cum   []float64
	total float64
}

func newPicker(items []weighted) *picker {
	p := &picker{}

	for _, it := range items {
		if it.weight <= 0 {
			continue
		}

		p.total += it.weight
		p.names = append(p.names, it.name)
		p.cum = append(p.cum, p.total)
	}

	return p
}

// pick - действие для случайного числа u в [0, 1).
func (p *picker) pick(u float64) string {
	if len(p.names) == 0 {
		return ""
	}

	target := u * p.total
	idx := sort.SearchFloat64s(p.cum, target)

	// SearchFloat64s дает первый cum >= target; на границе корзины берем следующую.
	if idx < len(p.cum) && p.cum[idx] == target {
		idx++
	}

	return p.names[min(idx, len(p.names)-1)]
}

// rampFraction - доля целевой нагрузки в момент elapsed от старта: линейно от 0
// до 1 за ramp, дальше 1.
func rampFraction(elapsed, ramp time.Duration) float64 {
	if ramp <= 0 || elapsed >= ramp {
		return 1
	}

	if elapsed <= 0 {
		return 0
	}

	return float64(elapsed) / float64(ramp)
}

// activationOffset - когда подключается i-й из n пользователей: равномерно за ramp
// (первый - сразу). Так initial sync не приходит одной пачкой.
func activationOffset(i, n int, ramp time.Duration) time.Duration {
	if n <= 1 || ramp <= 0 {
		return 0
	}

	return time.Duration(float64(ramp) * float64(i) / float64(n))
}

// fireAt - момент k-го действия (k = 1, 2, ...) в mode rps от старта. Частота
// растет линейно за ramp, поэтому число действий к моменту t - интеграл частоты:
// N(t) = target*t^2/(2*ramp) внутри ramp и target*(t - ramp/2) после; fireAt -
// обратная функция. Расписание от старта, а не от предыдущего действия: опоздание
// одного тика не сдвигает остальные.
func fireAt(k int64, target float64, ramp time.Duration) time.Duration {
	n := float64(k)
	if ramp <= 0 {
		return time.Duration(n / target * float64(time.Second))
	}

	rampSec := ramp.Seconds()
	if inRamp := target * rampSec / 2; n <= inRamp {
		return time.Duration(math.Sqrt(2*n*rampSec/target) * float64(time.Second))
	}

	return time.Duration((n/target + rampSec/2) * float64(time.Second))
}

// thinkTime - пауза между действиями пользователя: экспоненциальное распределение
// со средним mean (поток действий пула - пуассоновский, без синхронных пиков),
// ограниченное сверху 10*mean.
func thinkTime(rng *rand.Rand, mean time.Duration) time.Duration {
	d := time.Duration(rng.ExpFloat64() * float64(mean))

	return min(d, 10*mean)
}

// zipfWeights - популярность n комнат по закону Ципфа с показателем s:
// вес комнаты ранга r - 1/r^s (s = 0 - равномерно).
func zipfWeights(n int, s float64) []float64 {
	out := make([]float64, n)
	for r := range n {
		out[r] = 1 / math.Pow(float64(r+1), s)
	}

	return out
}

// assignRooms - для каждого из users пользователей perUser разных комнат из rooms
// по весам популярности. Создатель комнаты r - пользователь r % users, он в ней
// состоит всегда (кроме этого, выбор случайный, но детерминированный по rng).
func assignRooms(rng *rand.Rand, users, rooms, perUser int, skew float64) [][]int {
	weights := zipfWeights(rooms, skew)
	out := make([][]int, users)

	for u := range users {
		chosen := map[int]bool{}

		for r := u; r < rooms; r += users {
			chosen[r] = true
		}

		// Взвешенный выбор без возвращения: веса выбранных обнуляются.
		w := append([]float64(nil), weights...)
		for r := range chosen {
			w[r] = 0
		}

		for len(chosen) < perUser {
			r := weightedIndex(rng, w)
			if r < 0 {
				break
			}

			chosen[r] = true
			w[r] = 0
		}

		list := make([]int, 0, len(chosen))
		for r := range chosen {
			list = append(list, r)
		}

		sort.Ints(list)
		out[u] = list
	}

	return out
}

// weightedIndex - индекс по весам; все веса нулевые - -1.
func weightedIndex(rng *rand.Rand, w []float64) int {
	total := 0.0
	for _, v := range w {
		total += v
	}

	if total <= 0 {
		return -1
	}

	target := rng.Float64() * total
	for i, v := range w {
		if v <= 0 {
			continue
		}

		target -= v
		if target < 0 {
			return i
		}
	}

	for i := len(w) - 1; i >= 0; i-- {
		if w[i] > 0 {
			return i
		}
	}

	return -1
}
