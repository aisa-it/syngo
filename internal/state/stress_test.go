package state_test

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/aisa-it/syngo/internal/state"
)

// TestStress100Forks - 100 форков (набор состояния на каждый)
// разрешаются детерминированно на 10 перестановках, повторное разрешение
// материализованного состояния не дает изменений.
func TestStress100Forks(t *testing.T) {
	t.Parallel()

	base := roomState("public")
	g := newGraph(base...)
	sets := make([]state.StateMap, 0, 100)

	// 100 веток: topic и join/leave bob в каждой.
	for i := range 100 {
		branch := strconv.Itoa(i)
		membership := "join"

		if i%3 == 0 {
			membership = "leave"
		}

		g.add(
			state.Event{
				ID: "$topic-" + branch, Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
				Content: []byte(`{"topic":"` + branch + `"}`), Auth: []string{"$create", "$pl", "$alice"},
				Prev: []string{"$jr"}, Depth: int64(10 + i), OriginTS: int64(1000 + i),
			},
			member("$member-"+branch, rBob, rBob, membership, int64(2000+i), "$create", "$pl", "$jr"),
		)

		sets = append(sets, g.set(append(ids(base), "$topic-"+branch, "$member-"+branch)...))
	}

	first := resolve(t, g, sets...)
	if first[keyTopic] == "" || first[keyBob] == "" {
		t.Fatalf("состояние не определено: %v", first)
	}

	rnd := rand.New(rand.NewSource(41))

	for i := range 10 {
		rnd.Shuffle(len(sets), func(a, b int) { sets[a], sets[b] = sets[b], sets[a] })

		got := resolve(t, g, sets...)
		if got[keyTopic] != first[keyTopic] || got[keyBob] != first[keyBob] {
			t.Fatalf("перестановка %d: %v, ожидали %v", i, got, first)
		}
	}

	// Идемпотентность: материализованное состояние не меняется при повторном разрешении.
	resolver := state.New().WithCurrent(first)

	if _, err := resolver.Resolve([]state.StateMap{state.StateMap(first), state.StateMap(first)}, g); err != nil {
		t.Fatalf("repeat resolve: %v", err)
	}

	if resolver.Stats.Changed != 0 {
		t.Fatalf("Stats.Changed = %d при повторном разрешении, ожидали 0", resolver.Stats.Changed)
	}
}

// TestMetricsExport — метрики разрешения отдаются в текстовом формате Prometheus.
func TestMetricsExport(t *testing.T) {
	t.Parallel()

	resolver := state.New().WithRoom("!metrics:test.local")

	base := roomState("public")
	g := newGraph(base...).add(
		state.Event{
			ID: "$mt1", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{}`), Auth: []string{"$create", "$pl", "$alice"}, OriginTS: 50,
		},
		state.Event{
			ID: "$mt2", Type: "m.room.topic", StateKey: ptr(""), Sender: rAlice,
			Content: []byte(`{}`), Auth: []string{"$create", "$pl", "$alice"}, OriginTS: 60,
		},
	)

	sets := []state.StateMap{g.set(append(ids(base), "$mt1")...), g.set(append(ids(base), "$mt2")...)}

	if _, err := resolver.Resolve(sets, g); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var buf testWriter

	state.DefaultMetrics.WritePrometheus(&buf)

	for _, want := range []string{
		"syngo_state_resolution_duration_seconds_total",
		"syngo_state_resolution_conflicts_total",
		"syngo_state_changed_total",
		"syngo_state_resolution_total",
	} {
		if !contains(buf.data, want) {
			t.Fatalf("метрика %s отсутствует в экспорте", want)
		}
	}

	if got := state.DefaultMetrics.ConflictsByRoom()["!metrics:test.local"]; got == 0 {
		t.Fatal("конфликты по комнате не учтены")
	}
}

// testWriter — минимальный io.Writer для проверки экспорта.
type testWriter struct{ data string }

func (w *testWriter) Write(p []byte) (int, error) {
	w.data += string(p)

	return len(p), nil
}

// contains — проверка подстроки (без лишних импортов).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
