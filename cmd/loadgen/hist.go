package main

import (
	"math"
	"time"
)

// histPrecision - относительная ширина корзины гистограммы: перцентиль известен
// с точностью ±1%. Хранить все замеры нельзя: на профиле L за 30 минут их
// десятки миллионов.
const histPrecision = 0.01

// histBuckets - число корзин: от 1 мкс до ~1,5 часа при шаге 1%.
const histBuckets = 2300

// histLogBase - ln(1 + histPrecision), шаг корзин в логарифмической шкале.
var histLogBase = math.Log1p(histPrecision)

// Histogram - лог-линейная гистограмма латентности (мкс). Не потокобезопасна:
// синхронизацию держит владелец (recorder).
type Histogram struct {
	counts [histBuckets]int64
	n      int64
	sumUS  float64
	minUS  int64
	maxUS  int64
}

// bucketOf - корзина для значения в микросекундах (0 и 1 мкс - в нулевой).
func bucketOf(us int64) int {
	if us <= 1 {
		return 0
	}

	idx := int(math.Log(float64(us)) / histLogBase)
	if idx >= histBuckets {
		return histBuckets - 1
	}

	return idx
}

// bucketValue - представитель корзины: середина [base^i, base^(i+1)); нулевая
// корзина (0 и 1 мкс) - 0.
func bucketValue(idx int) float64 {
	if idx == 0 {
		return 0
	}

	lo := math.Exp(float64(idx) * histLogBase)
	hi := math.Exp(float64(idx+1) * histLogBase)

	return (lo + hi) / 2
}

// Observe добавляет замер; отрицательный приводится к нулю.
func (h *Histogram) Observe(d time.Duration) {
	us := max(d.Microseconds(), 0)

	h.counts[bucketOf(us)]++

	if h.n == 0 || us < h.minUS {
		h.minUS = us
	}

	if us > h.maxUS {
		h.maxUS = us
	}

	h.n++
	h.sumUS += float64(us)
}

// Count - число замеров.
func (h *Histogram) Count() int64 { return h.n }

// Merge добавляет замеры другой гистограммы.
func (h *Histogram) Merge(other *Histogram) {
	if other.n == 0 {
		return
	}

	for i, c := range &other.counts {
		h.counts[i] += c
	}

	if h.n == 0 || other.minUS < h.minUS {
		h.minUS = other.minUS
	}

	h.maxUS = max(h.maxUS, other.maxUS)
	h.n += other.n
	h.sumUS += other.sumUS
}

// QuantileMS - перцентиль p (0..100) в миллисекундах; пусто - 0. Значение
// ограничено фактическими min и max: на краях корзина шире реального разброса.
func (h *Histogram) QuantileMS(p float64) float64 {
	if h.n == 0 {
		return 0
	}

	// Ранг по nearest-rank: p99 из 100 замеров - 99-й по порядку.
	rank := int64(math.Ceil(p / 100 * float64(h.n)))
	rank = min(max(rank, 1), h.n)

	var seen int64

	for i, c := range &h.counts {
		seen += c
		if seen >= rank {
			// Последняя корзина - переполнение шкалы: известен только max.
			if i == histBuckets-1 {
				return float64(h.maxUS) / 1000
			}

			v := math.Min(math.Max(bucketValue(i), float64(h.minUS)), float64(h.maxUS))

			return v / 1000
		}
	}

	return float64(h.maxUS) / 1000
}

// MeanMS - среднее, мс.
func (h *Histogram) MeanMS() float64 {
	if h.n == 0 {
		return 0
	}

	return h.sumUS / float64(h.n) / 1000
}

// MaxMS - максимум, мс.
func (h *Histogram) MaxMS() float64 { return float64(h.maxUS) / 1000 }

// latencySummary - перцентили для отчета.
type latencySummary struct {
	Count  int64   `json:"count"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
	MeanMS float64 `json:"mean_ms"`
}

// Summary - перцентили гистограммы (значения округлены до микросекунд).
func (h *Histogram) Summary() latencySummary {
	return latencySummary{
		Count:  h.n,
		P50MS:  round3(h.QuantileMS(50)),
		P95MS:  round3(h.QuantileMS(95)),
		P99MS:  round3(h.QuantileMS(99)),
		MaxMS:  round3(h.MaxMS()),
		MeanMS: round3(h.MeanMS()),
	}
}

// round3 - три знака после запятой (микросекунды в миллисекундах).
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
