package main

import (
	"sync"
	"time"
)

// markerKey - поле content, которым генератор метит свои сообщения: id прогона и
// момент начала PUT /send. Сервер хранит content как есть (и у m.room.encrypted).
const markerKey = "io.syngo.loadgen"

// loadMarker - метка сообщения генератора.
type loadMarker struct {
	Run string `json:"run"`
	// TS - время начала PUT /send, unix-микросекунды часов генератора (наносекунды
	// не влезают в диапазон целых canonical JSON, ±2^53: сервер отвечает 400).
	TS int64 `json:"ts"`
}

// sentEvent - отправленное сообщение, ожидающее появления в /sync получателей.
type sentEvent struct {
	start, ack time.Time
	// expected - сколько получателей должны его увидеть (участники комнаты с
	// активным /sync на момент ответа, без отправителя).
	expected int
	seen     int
	measured bool
}

// deliveryTracker меряет задержку доставки: от ответа на PUT /send у отправителя
// до появления события в /sync каждого получателя (основная метрика), и от начала
// PUT (дополнительно: включает запись события).
type deliveryTracker struct {
	run string

	mu   sync.Mutex
	sent map[string]*sentEvent
	// early - моменты, когда событие пришло в /sync раньше, чем отправитель
	// получил ответ на PUT.
	early     map[string][]time.Time
	fromAck   Histogram
	fromStart Histogram
	// beforeAck - доставлено получателю раньше ответа отправителю (задержка 0).
	beforeAck int64
}

func newDeliveryTracker(run string) *deliveryTracker {
	return &deliveryTracker{run: run, sent: map[string]*sentEvent{}, early: map[string][]time.Time{}}
}

// acked - отправитель получил event_id. measured - отправка начата в окне замера.
func (t *deliveryTracker) acked(eventID string, start, ack time.Time, expected int, measured bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ev := &sentEvent{start: start, ack: ack, expected: expected, measured: measured}
	t.sent[eventID] = ev

	for _, at := range t.early[eventID] {
		ev.seen++

		if measured && ev.seen <= expected {
			t.beforeAck++
			t.fromAck.Observe(0)
			t.fromStart.Observe(at.Sub(start))
		}
	}

	delete(t.early, eventID)
}

// seen - событие с меткой пришло в /sync получателя в момент at.
func (t *deliveryTracker) seen(eventID string, marker loadMarker, at time.Time) {
	if marker.Run != t.run {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	ev := t.sent[eventID]
	if ev == nil {
		t.early[eventID] = append(t.early[eventID], at)

		return
	}

	// Получатель, вошедший в комнату после отправки, тоже видит событие: seen
	// может превысить expected, в Delivered и задержку идет не больше expected.
	ev.seen++

	if !ev.measured || ev.seen > ev.expected {
		return
	}

	t.fromAck.Observe(at.Sub(ev.ack))
	t.fromStart.Observe(at.Sub(ev.start))
}

// deliveryReport - доставка в отчете.
type deliveryReport struct {
	// FromAck - от ответа на PUT /send до /sync получателя (основная метрика).
	FromAck latencySummary `json:"from_ack"`
	// FromSendStart - от начала PUT /send до /sync получателя.
	FromSendStart latencySummary `json:"from_send_start"`
	// Messages - отправленных в окне замера; Expected - ожидаемых доставок (сумма
	// получателей), Delivered - состоявшихся, Completeness - Delivered/Expected.
	Messages     int64   `json:"messages"`
	Expected     int64   `json:"expected"`
	Delivered    int64   `json:"delivered"`
	Completeness float64 `json:"completeness"`
	BeforeAck    int64   `json:"before_ack"`
	Undelivered  int64   `json:"undelivered_messages"`
}

// report - сводка доставки (вызывать после drain).
func (t *deliveryTracker) report() deliveryReport {
	t.mu.Lock()
	defer t.mu.Unlock()

	rep := deliveryReport{
		FromAck:       t.fromAck.Summary(),
		FromSendStart: t.fromStart.Summary(),
		BeforeAck:     t.beforeAck,
	}

	for _, ev := range t.sent {
		if !ev.measured {
			continue
		}

		rep.Messages++
		rep.Expected += int64(ev.expected)
		rep.Delivered += int64(min(ev.seen, ev.expected))

		if ev.seen < ev.expected {
			rep.Undelivered++
		}
	}

	if rep.Expected > 0 {
		rep.Completeness = round3(float64(rep.Delivered) / float64(rep.Expected))
	}

	return rep
}
