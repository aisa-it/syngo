package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// promSample - строка текстового формата Prometheus.
type promSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// promSet - снимок /metrics: ключ серии (имя + отсортированные метки) -> отсчет.
type promSet map[string]promSample

// seriesKey - каноническое имя серии.
func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	var b strings.Builder

	b.WriteString(name)
	b.WriteByte('{')

	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, "%s=%q", k, labels[k])
	}

	b.WriteByte('}')

	return b.String()
}

// parsePrometheus разбирает текстовый формат (exposition 0.0.4). Комментарии и
// битые строки пропускаются: метрики чужого сервера (Synapse) не должны ронять отчет.
func parsePrometheus(r io.Reader) (promSet, error) {
	out := promSet{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}

		s, ok := parsePromLine(line)
		if !ok {
			continue
		}

		out[seriesKey(s.Name, s.Labels)] = s
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}

	return out, nil
}

// parsePromLine - `name{a="b"} value [timestamp]`.
func parsePromLine(line string) (promSample, bool) {
	s := promSample{Labels: map[string]string{}}

	i := strings.IndexAny(line, "{ \t")
	if i < 0 {
		return s, false
	}

	s.Name = line[:i]
	rest := line[i:]

	if strings.HasPrefix(rest, "{") {
		end, ok := parsePromLabels(rest[1:], s.Labels)
		if !ok {
			return s, false
		}

		rest = rest[1+end:]
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 || s.Name == "" {
		return s, false
	}

	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, false
	}

	s.Value = v

	return s, true
}

// parsePromLabels разбирает `a="b",c="d"}` и возвращает позицию после "}".
func parsePromLabels(s string, into map[string]string) (int, bool) {
	i := 0

	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}

		if i < len(s) && s[i] == '}' {
			return i + 1, true
		}

		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 || i+eq+1 >= len(s) || s[i+eq+1] != '"' {
			return 0, false
		}

		name := strings.TrimSpace(s[i : i+eq])
		i += eq + 2

		var val strings.Builder

		closed := false

		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i+1])
				}

				i += 2

				continue
			}

			i++

			if c == '"' {
				closed = true

				break
			}

			val.WriteByte(c)
		}

		if !closed {
			return 0, false
		}

		into[name] = val.String()
	}

	return 0, false
}

// sum - сумма значений метрики name по сериям, у которых совпадают метки match.
func (p promSet) sum(name string, match map[string]string) (float64, bool) {
	total, found := 0.0, false

	for _, s := range p {
		if s.Name != name || !labelsMatch(s.Labels, match) {
			continue
		}

		total += s.Value
		found = true
	}

	return total, found
}

func labelsMatch(labels, match map[string]string) bool {
	for k, v := range match {
		if labels[k] != v {
			return false
		}
	}

	return true
}

// delta - прирост счетчика name (сумма по сериям) от start до end; сброс счетчика
// (рестарт сервера) - значение end.
func delta(start, end promSet, name string) (float64, bool) {
	e, ok := end.sum(name, nil)
	if !ok {
		return 0, false
	}

	s, _ := start.sum(name, nil)
	if e < s {
		return e, true
	}

	return e - s, true
}

// groupedDelta - прирост счетчика name по значениям меток by (ключ - значения через пробел).
func groupedDelta(start, end promSet, name string, by ...string) map[string]float64 {
	out := map[string]float64{}

	for key, s := range end {
		if s.Name != name {
			continue
		}

		prev := start[key].Value

		d := s.Value - prev
		if d < 0 {
			d = s.Value
		}

		if d == 0 {
			continue
		}

		parts := make([]string, len(by))
		for i, l := range by {
			parts[i] = s.Labels[l]
		}

		out[strings.Join(parts, " ")] += d
	}

	return out
}

// histBucket - корзина гистограммы Prometheus: верхняя граница и накопленный счет.
type histBucket struct {
	le    float64
	count float64
}

// histogramQuantiles - квантиль q (0..1) гистограммы name за интервал (разность
// снимков) по группам меток by. Интерполяция внутри корзины - как в PromQL
// histogram_quantile. Группы без замеров пропускаются.
func histogramQuantiles(start, end promSet, name string, q float64, by ...string) map[string]float64 {
	groups := map[string]map[float64]float64{}

	for key, s := range end {
		if s.Name != name+"_bucket" {
			continue
		}

		le, err := strconv.ParseFloat(s.Labels["le"], 64)
		if err != nil {
			continue
		}

		d := s.Value - start[key].Value
		if d < 0 {
			d = s.Value
		}

		parts := make([]string, len(by))
		for i, l := range by {
			parts[i] = s.Labels[l]
		}

		g := strings.Join(parts, " ")
		if groups[g] == nil {
			groups[g] = map[float64]float64{}
		}

		groups[g][le] += d
	}

	out := map[string]float64{}

	for g, byLE := range groups {
		buckets := make([]histBucket, 0, len(byLE))
		for le, c := range byLE {
			buckets = append(buckets, histBucket{le: le, count: c})
		}

		sort.Slice(buckets, func(i, j int) bool { return buckets[i].le < buckets[j].le })

		if v, ok := bucketQuantile(q, buckets); ok {
			out[g] = v
		}
	}

	return out
}

// bucketQuantile - квантиль по накопленным корзинам (последняя - +Inf).
func bucketQuantile(q float64, buckets []histBucket) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}

	total := buckets[len(buckets)-1].count
	if total <= 0 {
		return 0, false
	}

	rank := q * total
	prevLE, prevCount := 0.0, 0.0

	for _, b := range buckets {
		if b.count >= rank {
			if math.IsInf(b.le, 1) {
				// Выше последней конечной границы значение неизвестно: берем ее.
				return prevLE, true
			}

			if b.count == prevCount {
				return b.le, true
			}

			return prevLE + (b.le-prevLE)*(rank-prevCount)/(b.count-prevCount), true
		}

		prevLE, prevCount = b.le, b.count
	}

	return prevLE, true
}
