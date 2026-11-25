package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHistogramQuantiles - перцентили гистограммы совпадают с точными (nearest
// rank) в пределах шага корзины.
func TestHistogramQuantiles(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))

	var h Histogram

	raw := make([]float64, 0, 20000)

	for range 20000 {
		// Лог-нормальная латентность: медиана ~20 мс, хвост до секунд.
		d := time.Duration(math.Exp(rng.NormFloat64()*1.2+math.Log(20000))) * time.Microsecond
		h.Observe(d)
		raw = append(raw, float64(d.Microseconds())/1000)
	}

	sort.Float64s(raw)

	for _, p := range []float64{50, 90, 95, 99, 99.9} {
		exact := raw[int(math.Ceil(p/100*float64(len(raw))))-1]
		got := h.QuantileMS(p)

		if math.Abs(got-exact)/exact > 0.011 {
			t.Errorf("p%g = %.3f, точно %.3f (ошибка > 1.1%%)", p, got, exact)
		}
	}

	if h.Count() != 20000 || h.MaxMS() != raw[len(raw)-1] {
		t.Fatalf("count %d, max %.3f (точно %.3f)", h.Count(), h.MaxMS(), raw[len(raw)-1])
	}

	mean := 0.0
	for _, v := range raw {
		mean += v
	}

	mean /= float64(len(raw))
	if math.Abs(h.MeanMS()-mean) > 0.01 {
		t.Fatalf("mean %.3f, точно %.3f", h.MeanMS(), mean)
	}
}

func TestHistogramEdges(t *testing.T) {
	t.Parallel()

	var empty Histogram
	if empty.QuantileMS(99) != 0 || empty.MeanMS() != 0 || empty.Summary().Count != 0 {
		t.Fatal("пустая гистограмма")
	}

	var h Histogram

	h.Observe(-time.Second) // отрицательное - ноль (часы, доставка раньше ответа)
	h.Observe(0)
	h.Observe(3 * time.Millisecond)
	h.Observe(5 * time.Hour) // за пределом шкалы - в последнюю корзину

	if h.QuantileMS(25) != 0 || h.QuantileMS(50) != 0 {
		t.Fatalf("p25 %.3f p50 %.3f, ожидали 0", h.QuantileMS(25), h.QuantileMS(50))
	}

	if got := h.QuantileMS(75); math.Abs(got-3) > 0.03 {
		t.Fatalf("p75 = %.3f, ожидали ~3", got)
	}

	if got := h.QuantileMS(100); got != 5*3600*1000 {
		t.Fatalf("p100 = %.3f: ограничен фактическим max", got)
	}

	var other Histogram

	other.Observe(time.Millisecond)
	h.Merge(&other)
	h.Merge(&Histogram{})

	if h.Count() != 5 {
		t.Fatalf("после Merge count %d", h.Count())
	}

	var single Histogram

	single.Merge(&other)

	if s := single.Summary(); s.Count != 1 || s.P50MS != 1 || s.MaxMS != 1 {
		t.Fatalf("summary %+v", s)
	}
}

func TestBuiltinProfilesValid(t *testing.T) {
	t.Parallel()

	for name, p := range builtinProfiles() {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}

		if p.Name != name || len(p.Weights) != len(knownActions) {
			t.Errorf("%s: имя %q, весов %d", name, p.Name, len(p.Weights))
		}
	}

	if _, err := builtinProfile("xl"); err == nil {
		t.Fatal("неизвестный профиль принят")
	}

	m, err := loadProfile("m", "")
	if err != nil || m.Users != 500 || m.Rooms != 200 {
		t.Fatalf("M: %+v, %v", m, err)
	}

	// Частота действий S: 50 пользователей / 5 с = 10/с.
	if s, _ := builtinProfile("S"); s.expectedActionRate() != 10 {
		t.Fatalf("S: %.1f действий/с", s.expectedActionRate())
	}
}

func TestParseProfile(t *testing.T) {
	t.Parallel()

	p, err := parseProfile([]byte(`{"base":"M","users":40,"duration":"90s","warmup":30,"ramp":"10s",
		"weights":{"send":1,"typing":1}}`))
	if err != nil {
		t.Fatal(err)
	}

	if p.Name != "custom" || p.Base != "M" || p.Users != 40 || p.Rooms != 200 ||
		p.Duration.D() != 90*time.Second || p.Warmup.D() != 30*time.Second {
		t.Fatalf("профиль %+v", p)
	}

	// Веса из JSON заменяют встроенные целиком.
	if len(p.Weights) != 2 || p.sortedWeights()[0].name != actSend {
		t.Fatalf("веса %v", p.Weights)
	}

	// Без weights - встроенные веса base (S по умолчанию).
	p, err = parseProfile([]byte(`{"name":"mine","users":10,"rooms":4,"rooms_per_user":2}`))
	if err != nil || p.Name != "mine" || len(p.Weights) != len(knownActions) || p.Base != "S" {
		t.Fatalf("%+v, %v", p, err)
	}

	bad := map[string]string{
		"опечатка в поле":      `{"user":10}`,
		"неизвестное действие": `{"weights":{"fly":1}}`,
		"нулевые веса":         `{"weights":{"send":0}}`,
		"отрицательный вес":    `{"weights":{"send":-1,"typing":1}}`,
		"режим":                `{"mode":"burst"}`,
		"rps без частоты":      `{"mode":"rps","target_rps":0}`,
		"ramp > warmup":        `{"ramp":"1m","warmup":"10s"}`,
		"плохая длительность":  `{"duration":"soon"}`,
		"длительность-объект":  `{"duration":{}}`,
		"мало пользователей":   `{"users":1}`,
		"rooms_per_user":       `{"rooms":2,"rooms_per_user":3}`,
		"skew":                 `{"room_skew":5}`,
		"encrypted":            `{"encrypted_rooms_percent":120}`,
		"think":                `{"think_time":"0s"}`,
		"sync_timeout":         `{"sync_timeout":"10m"}`,
		"media":                `{"media_kb":0}`,
		"base":                 `{"base":"XXL"}`,
		"битый JSON":           `{`,
		"duration 0":           `{"duration":"0s"}`,
		"rooms 0":              `{"rooms":0,"rooms_per_user":0}`,
		"warmup < 0":           `{"warmup":"-1s","ramp":"-2s"}`,
	}
	for name, body := range bad {
		if _, err := parseProfile([]byte(body)); err == nil {
			t.Errorf("%s: профиль %s принят", name, body)
		}
	}

	// Duration в JSON - строкой.
	out, _ := json.Marshal(Duration(1500 * time.Millisecond))
	if string(out) != `"1.5s"` {
		t.Fatalf("Duration JSON %s", out)
	}
}

func TestLoadProfileFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"base":"L","duration":"1m"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := loadProfile("S", path)
	if err != nil || p.Users != 5000 || p.Duration.D() != time.Minute {
		t.Fatalf("%+v %v", p, err)
	}

	if _, err := loadProfile("S", filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("нет файла - ошибка")
	}
}

func TestApplyOverrides(t *testing.T) {
	t.Parallel()

	p, _ := builtinProfile("S")
	applyOverrides(&p, options{
		users: 7, rooms: 3, duration: time.Minute, warmup: 4 * time.Second, think: time.Second,
		rps: 25, seed: 9,
	})

	if p.Users != 7 || p.Rooms != 3 || p.RoomsPerUser != 3 || p.Duration.D() != time.Minute ||
		p.Mode != modeRPS || p.TargetRPS != 25 || p.Seed != 9 || p.ThinkTime.D() != time.Second {
		t.Fatalf("%+v", p)
	}

	// ramp подрезан до warmup.
	if p.Ramp.D() != 4*time.Second {
		t.Fatalf("ramp %s", p.Ramp.D())
	}

	applyOverrides(&p, options{mode: modeUsers, ramp: time.Second, roomsPerUser: 2})

	if p.Mode != modeUsers || p.Ramp.D() != time.Second || p.RoomsPerUser != 2 || p.Validate() != nil {
		t.Fatalf("%+v: %v", p, p.Validate())
	}
}

func TestPicker(t *testing.T) {
	t.Parallel()

	pk := newPicker([]weighted{{"a", 1}, {"zero", 0}, {"b", 3}})

	// Границы: [0, 0.25) - a, [0.25, 1) - b.
	cases := map[float64]string{0: "a", 0.2499: "a", 0.25: "b", 0.9999: "b"}
	for u, want := range cases {
		if got := pk.pick(u); got != want {
			t.Errorf("pick(%g) = %s, ожидали %s", u, got, want)
		}
	}

	rng := rand.New(rand.NewPCG(3, 4))
	counts := map[string]int{}

	for range 40000 {
		counts[pk.pick(rng.Float64())]++
	}

	if share := float64(counts["b"]) / 40000; math.Abs(share-0.75) > 0.01 || counts["zero"] != 0 {
		t.Fatalf("распределение %v", counts)
	}

	if newPicker(nil).pick(0.5) != "" {
		t.Fatal("пустой picker")
	}
}

func TestRampAndSchedule(t *testing.T) {
	t.Parallel()

	ramp := 10 * time.Second

	for _, c := range []struct {
		at   time.Duration
		want float64
	}{{-time.Second, 0}, {0, 0}, {5 * time.Second, 0.5}, {ramp, 1}, {time.Minute, 1}} {
		if got := rampFraction(c.at, ramp); got != c.want {
			t.Errorf("rampFraction(%s) = %g, ожидали %g", c.at, got, c.want)
		}
	}

	if rampFraction(0, 0) != 1 {
		t.Fatal("ramp 0 - сразу полная нагрузка")
	}

	if activationOffset(0, 10, ramp) != 0 || activationOffset(5, 10, ramp) != 5*time.Second ||
		activationOffset(3, 1, ramp) != 0 || activationOffset(3, 10, 0) != 0 {
		t.Fatal("activationOffset")
	}

	// Число действий за 30 с при цели 20/с и ramp 10 с: 20*(30 - 10/2) = 500.
	fires := int64(0)
	for fireAt(fires+1, 20, ramp) <= 30*time.Second {
		fires++
	}

	if fires != 500 {
		t.Fatalf("за 30 с %d действий, ожидали 500", fires)
	}

	// Внутри ramp: к 5 с - 20*25/20 = 25 действий; моменты возрастают.
	if got := fireAt(25, 20, ramp); got != 5*time.Second {
		t.Fatalf("25-е действие в %s", got)
	}

	for k := int64(1); k < 300; k++ {
		if fireAt(k+1, 20, ramp) <= fireAt(k, 20, ramp) {
			t.Fatalf("расписание не возрастает на %d", k)
		}
	}

	// Без ramp - равномерно.
	if d := fireAt(3, 0.5, 0); d != 6*time.Second {
		t.Fatalf("интервал %s", d)
	}
}

func TestThinkTime(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(5, 6))
	mean := time.Second

	var total time.Duration

	for range 20000 {
		d := thinkTime(rng, mean)
		if d < 0 || d > 10*mean {
			t.Fatalf("пауза %s вне [0, 10*mean]", d)
		}

		total += d
	}

	if avg := total / 20000; avg < 950*time.Millisecond || avg > 1050*time.Millisecond {
		t.Fatalf("среднее %s, ожидали ~1s", avg)
	}
}

func TestAssignRooms(t *testing.T) {
	t.Parallel()

	users, rooms, per := 50, 20, 5
	a := assignRooms(rand.New(rand.NewPCG(7, 0)), users, rooms, per, 1)
	b := assignRooms(rand.New(rand.NewPCG(7, 0)), users, rooms, per, 1)

	sizes := make([]int, rooms)

	for u := range users {
		if len(a[u]) != per {
			t.Fatalf("пользователь %d: %d комнат", u, len(a[u]))
		}

		seen := map[int]bool{}

		for i, r := range a[u] {
			if seen[r] || r != b[u][i] {
				t.Fatalf("пользователь %d: повтор или недетерминизм %v / %v", u, a[u], b[u])
			}

			seen[r] = true
			sizes[r]++
		}

		// Создатель (r % users == u) состоит в своей комнате.
		if u < rooms && !seen[u] {
			t.Fatalf("создатель %d не в комнате %d", u, u)
		}
	}

	// Ципф: первая комната популярнее последней.
	if sizes[0] <= sizes[rooms-1] {
		t.Fatalf("размеры %v", sizes)
	}

	// Равномерно (skew 0) и все комнаты (per == rooms).
	all := assignRooms(rand.New(rand.NewPCG(1, 1)), 3, 4, 4, 0)
	if len(all[2]) != 4 {
		t.Fatalf("%v", all)
	}

	if weightedIndex(rand.New(rand.NewPCG(1, 1)), []float64{0, 0}) != -1 {
		t.Fatal("нулевые веса")
	}
}

func TestRecorder(t *testing.T) {
	t.Parallel()

	r := newRecorder()
	r.observe("send", time.Millisecond, 200) // до замера - не пишется
	r.count("x", 1)
	r.measuring.Store(true)

	for range 97 {
		r.observe("send", 10*time.Millisecond, 200)
	}

	r.observe("send", time.Second, 429)
	r.observe("send", time.Second, 500)
	r.observe("sync", time.Second, 0)
	r.count("dropped", 2)

	eps, counters := r.snapshot(10 * time.Second)
	send := eps["send"]

	if send.Count != 99 || send.Errors != 2 || send.RateLimited != 1 || send.RPS != 9.9 ||
		send.Statuses["200"] != 97 || send.Statuses["500"] != 1 {
		t.Fatalf("send %+v", send)
	}

	if eps["sync"].Statuses["net"] != 1 || counters["dropped"] != 2 || counters["x"] != 0 {
		t.Fatalf("sync %+v counters %v", eps["sync"], counters)
	}

	tot := sumEndpoints(eps, 10*time.Second)
	// Доля ошибок без 429: (3 - 1) / 100.
	if tot.Requests != 100 || tot.Errors != 3 || tot.RateLimited != 1 || tot.ErrorRate != 0.02 || tot.RPS != 10 {
		t.Fatalf("totals %+v", tot)
	}

	if names := sortedEndpoints(eps); names[0] != "send" || names[1] != "sync" {
		t.Fatalf("порядок %v", names)
	}
}

func TestDeliveryTracker(t *testing.T) {
	t.Parallel()

	tr := newDeliveryTracker("run1")
	base := time.Unix(1000, 0)
	m := loadMarker{Run: "run1", TS: base.UnixMicro()}

	// Обычный путь: ответ отправителю, потом два получателя.
	tr.acked("$a", base, base.Add(50*time.Millisecond), 2, true)
	tr.seen("$a", m, base.Add(150*time.Millisecond))
	tr.seen("$a", m, base.Add(250*time.Millisecond))
	// Третий (вошел после отправки) - не сверх expected.
	tr.seen("$a", m, base.Add(5*time.Second))

	// Доставка раньше ответа отправителю: задержка 0.
	tr.seen("$b", m, base.Add(10*time.Millisecond))
	tr.acked("$b", base, base.Add(20*time.Millisecond), 1, true)

	// Не доставлено одному из двух.
	tr.acked("$c", base, base.Add(time.Millisecond), 2, true)
	tr.seen("$c", m, base.Add(2*time.Millisecond))

	// Вне окна замера и чужой прогон - не считаются.
	tr.acked("$d", base, base, 3, false)
	tr.seen("$d", m, base.Add(time.Second))
	tr.seen("$e", loadMarker{Run: "other"}, base)

	rep := tr.report()
	if rep.Messages != 3 || rep.Expected != 5 || rep.Delivered != 4 || rep.Undelivered != 1 ||
		rep.BeforeAck != 1 || rep.Completeness != 0.8 {
		t.Fatalf("report %+v", rep)
	}

	// Задержки от ответа: 0 (b), 1 (c), 100, 200 (a).
	if rep.FromAck.Count != 4 || rep.FromAck.MaxMS != 200 || rep.FromAck.P50MS < 0.99 || rep.FromAck.P50MS > 1.01 {
		t.Fatalf("from_ack %+v", rep.FromAck)
	}

	if rep.FromSendStart.MaxMS != 250 {
		t.Fatalf("from_send_start %+v", rep.FromSendStart)
	}
}

func TestPromParse(t *testing.T) {
	t.Parallel()

	text := `# HELP x
# TYPE x counter
syngo_http_requests_total{method="GET",route="/sync",code="200"} 10
syngo_http_requests_total{code="429",method="PUT",route="/send"} 3 1700000000
weird{a="q\"uote\\n",b="x y"} 1.5
nl{a="x\ny"} 2
no_labels 7
broken{a="x" 1
broken2{a=x} 1
bad_value{a="x"} abc
lonely
`

	set, err := parsePrometheus(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	if len(set) != 5 {
		t.Fatalf("разобрано %d серий: %v", len(set), set)
	}

	if s := set[seriesKey("weird", map[string]string{"a": "q\"uote\\n", "b": "x y"})]; s.Value != 1.5 {
		t.Fatalf("экранирование меток: %v", set)
	}

	if s := set[seriesKey("nl", map[string]string{"a": "x\ny"})]; s.Value != 2 {
		t.Fatalf("\\n в метке: %v", set)
	}

	if v, ok := set.sum("syngo_http_requests_total", map[string]string{"code": "429"}); !ok || v != 3 {
		t.Fatalf("sum = %g", v)
	}

	if _, ok := set.sum("missing", nil); ok {
		t.Fatal("нет метрики - not ok")
	}

	start, _ := parsePrometheus(strings.NewReader("c 5\nreq{r=\"a\"} 1\n"))
	end, _ := parsePrometheus(strings.NewReader("c 8\nreq{r=\"a\"} 4\nreq{r=\"b\"} 2\n"))

	if d, _ := delta(start, end, "c"); d != 3 {
		t.Fatalf("delta %g", d)
	}

	// Сброс счетчика (рестарт) - значение конца.
	if d, _ := delta(end, start, "c"); d != 5 {
		t.Fatalf("delta после сброса %g", d)
	}

	if _, ok := delta(start, end, "missing"); ok {
		t.Fatal("delta отсутствующей метрики")
	}

	if g := groupedDelta(start, end, "req", "r"); g["a"] != 3 || g["b"] != 2 {
		t.Fatalf("groupedDelta %v", g)
	}
}

// promHist - текст гистограммы Prometheus с накопленными счетами.
func promHist(name, labels string, bounds []string, counts []int) string {
	var b strings.Builder
	for i, le := range bounds {
		b.WriteString(name + `_bucket{` + labels + `le="` + le + `"} ` + strconv.Itoa(counts[i]) + "\n")
	}

	return b.String()
}

func TestHistogramQuantilesProm(t *testing.T) {
	t.Parallel()

	bounds := []string{"0.1", "0.5", "1", "+Inf"}
	start, _ := parsePrometheus(strings.NewReader(promHist("h", `r="a",`, bounds, []int{0, 0, 0, 0})))
	// За окно: 4 в (0, 0.1], 4 в (0.1, 0.5], 2 в (0.5, 1] - всего 10.
	end, _ := parsePrometheus(strings.NewReader(promHist("h", `r="a",`, bounds, []int{4, 8, 10, 10})))

	got := histogramQuantiles(start, end, "h", 0.5, "r")
	// Ранг 5 - в корзине (0.1, 0.5]: 0.1 + 0.4 * (5-4)/(8-4) = 0.2.
	if math.Abs(got["a"]-0.2) > 1e-9 {
		t.Fatalf("p50 = %v", got)
	}

	if got := histogramQuantiles(start, end, "h", 0.95, "r"); math.Abs(got["a"]-0.875) > 1e-9 {
		t.Fatalf("p95 = %v", got)
	}

	// Хвост в +Inf - последняя конечная граница.
	if v, ok := bucketQuantile(0.99, []histBucket{{1, 1}, {math.Inf(1), 10}}); !ok || v != 1 {
		t.Fatalf("+Inf: %v %v", v, ok)
	}

	if _, ok := bucketQuantile(0.5, nil); ok {
		t.Fatal("пусто")
	}

	if _, ok := bucketQuantile(0.5, []histBucket{{1, 0}, {math.Inf(1), 0}}); ok {
		t.Fatal("без замеров")
	}

	if v, _ := bucketQuantile(0.5, []histBucket{{1, 0}, {2, 0}, {3, 4}}); v != 2.5 {
		t.Fatalf("v = %v", v)
	}
}

func TestMatrixHelpers(t *testing.T) {
	t.Parallel()

	if got := pathEscape("!room:host/x #a$b~c"); got != "%21room%3Ahost%2Fx%20%23a%24b~c" {
		t.Fatalf("pathEscape %q", got)
	}

	resp := &http.Response{StatusCode: 429, Header: http.Header{}}

	ae := parseAPIError(resp, []byte(`{"errcode":"M_LIMIT_EXCEEDED","error":"slow","retry_after_ms":1500}`))
	if ae.ErrCode != "M_LIMIT_EXCEEDED" || ae.RetryAfter != 1500*time.Millisecond || !strings.Contains(ae.Error(), "429") {
		t.Fatalf("%+v", ae)
	}

	resp.Header.Set("Retry-After", "3")

	ae = parseAPIError(resp, []byte("not json"))
	if ae.Message != "not json" || ae.RetryAfter != 3*time.Second {
		t.Fatalf("%+v", ae)
	}

	if statusOf(ae) != 429 || statusOf(errors.New("net")) != 0 {
		t.Fatal("statusOf")
	}
}

func TestSharedSecretMAC(t *testing.T) {
	t.Parallel()

	// Эталон - hmac.new(b"secret", b"abc\0alice\0pw\0notadmin", sha1) в Python
	// (как register_new_matrix_user).
	if got := sharedSecretMAC("secret", "abc", "alice", "pw", false); got != "42e32623fe124933c326f9d2d67ff7c617915a10" {
		t.Fatalf("mac %s", got)
	}

	if got := sharedSecretMAC("secret", "abc", "alice", "pw", true); got != "dc5ed98b283077684552e3f72d67f26b3366bf58" {
		t.Fatalf("admin mac %s", got)
	}
}

func TestTokenCache(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sub", "tokens.json")

	c, err := loadTokenCache(path)
	if err != nil {
		t.Fatal(err)
	}

	c.put("http://a", "u1", cachedUser{UserID: "@u1:a", Token: "t"})

	if err := c.save(); err != nil {
		t.Fatal(err)
	}

	c2, err := loadTokenCache(path)
	if err != nil {
		t.Fatal(err)
	}

	if u, ok := c2.get("http://a", "u1"); !ok || u.Token != "t" {
		t.Fatalf("%+v", u)
	}

	if _, ok := c2.get("http://b", "u1"); ok {
		t.Fatal("кеш другого сервера")
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadTokenCache(path); err == nil {
		t.Fatal("битый кеш")
	}

	noCache, _ := loadTokenCache("")
	if noCache.save() != nil {
		t.Fatal("без кеша save - no-op")
	}
}

func TestWithRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	calls := 0

	err := withRetry(ctx, func() error {
		calls++
		if calls < 3 {
			return &apiError{Status: 429, RetryAfter: time.Millisecond}
		}

		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("429: %v, вызовов %d", err, calls)
	}

	calls = 0

	err = withRetry(ctx, func() error {
		calls++

		return &apiError{Status: 400}
	})
	if statusOf(err) != 400 || calls != 1 {
		t.Fatalf("400 повторять нельзя: %v, вызовов %d", err, calls)
	}

	calls = 0

	err = withRetry(ctx, func() error {
		calls++

		return errUserInUse
	})
	if !errors.Is(err, errUserInUse) || calls != 1 {
		t.Fatalf("user in use: %v, вызовов %d", err, calls)
	}
}

func TestChecksAndMarkdown(t *testing.T) {
	t.Parallel()

	prof, _ := builtinProfile("S")
	r := &report{
		Profile: prof, Target: targetInfo{BaseURL: "http://x", Kind: "syngo", Build: "dev"},
		Config: map[string]string{"rate_limit": "prod"},
		Endpoints: map[string]endpointReport{
			"send_text":      {latencySummary: latencySummary{Count: 10, P95MS: 700}},
			"send_encrypted": {latencySummary: latencySummary{Count: 20, P95MS: 100}},
		},
		Summary:  summary{totals: totals{ErrorRate: 0.05}},
		Delivery: deliveryReport{Messages: 5, Completeness: 1, FromAck: latencySummary{P95MS: 1200, P99MS: 1500}},
		Counters: map[string]int64{"actions": 3, "rps_dropped_inflight": 2},
		Server: serverReport{
			MetricsURL: "http://m", Kind: "syngo", Requests: map[string]float64{"GET /sync": 3},
			RouteP95MS: map[string]float64{"GET /sync": 9}, Status: map[string]float64{"200": 3},
		},
		Process: processReport{PID: 1, RSSEndKB: 10},
		Postgres: postgresReport{
			Version: "PostgreSQL 17.1 on x", Counters: map[string]int64{"xact_commit": 1},
			Statements: []statementReport{{Query: "SELECT |1|", Calls: 1, TotalMS: 2}},
		},
	}
	r.Checks = buildChecks(r, thresholds{
		maxErrorRate: 0.01, minDelivery: 0.99, sloDeliveryP95: time.Second,
		sloDeliveryP99: 2 * time.Second, sloSendP95: 500 * time.Millisecond,
	})

	byName := map[string]check{}
	for _, c := range r.Checks {
		byName[c.Name] = c
	}

	// send - по эндпоинту с большим числом замеров (здесь шифрованные).
	if byName["error_rate (без 429)"].OK || !byName["delivery.completeness"].OK ||
		byName["delivery.from_ack.p95_ms"].OK || !byName["send.p95_ms"].OK {
		t.Fatalf("checks %+v", r.Checks)
	}

	if len(failed(r.Checks, false)) != 1 || len(failed(r.Checks, true)) != 2 {
		t.Fatalf("failed: %v / %v", failed(r.Checks, false), failed(r.Checks, true))
	}

	var md bytes.Buffer

	renderMarkdown(&md, r)

	for _, want := range []string{
		"# Нагрузочный прогон: профиль S против syngo", "| rate_limit | prod |", "## Доставка сообщений",
		"| send_encrypted |", "| GET /sync | 3 |", "rps_dropped_inflight", "НЕ ПРОЙДЕНА", "SLO (предложение)",
		"PID 1: RSS", "`SELECT \\|1\\|`", "PostgreSQL 17.1.",
	} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("в markdown нет %q", want)
		}
	}

	// Без источников - пояснения вместо цифр.
	md.Reset()
	renderMarkdown(&md, &report{Profile: prof, Server: serverReport{Kind: "synapse", MetricsURL: "u", Status: map[string]float64{"200": 1}}})

	for _, want := range []string{"Метрики Synapse", "PID не задан", "DSN не задан"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("в markdown нет %q", want)
		}
	}

	md.Reset()
	renderMarkdown(&md, &report{
		Profile: prof, Postgres: postgresReport{Error: "pg down"},
		Process: processReport{PID: 5, Error: "no proc"}, Server: serverReport{MetricsURL: "u", Error: "refused"},
	})

	for _, want := range []string{"pg down", "no proc", "/metrics: refused"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("в markdown нет %q", want)
		}
	}

	if truncate("абвгд", 3) != "абв…" || firstWords("a b c", 2) != "a b" {
		t.Fatal("truncate/firstWords")
	}
}

func TestLabelFlags(t *testing.T) {
	t.Parallel()

	l := labelFlags{}
	if err := l.Set(" rate_limit = prod "); err != nil || l["rate_limit"] != "prod" {
		t.Fatalf("%v %v", l, err)
	}

	if l.Set("novalue") == nil || l.Set("=x") == nil {
		t.Fatal("плохой -label принят")
	}

	if !strings.Contains(l.String(), "rate_limit") {
		t.Fatal(l.String())
	}
}
