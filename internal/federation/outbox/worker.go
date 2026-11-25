// Package outbox — исходящая очередь федерации (ADR-0007 §5).
//
// Воркер захватывает пачки по destination (аренда в БД), собирает
// PDU по event_id, отправляет PUT /_matrix/federation/v1/send/{txnId} через
// транспорт, разбирает ответ по PDU и фиксирует результат: доставленное
// удаляется, недоставленное откладывается с экспоненциальным backoff, после
// предела попыток или срока элемент уходит в dead-letter.
package outbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/store"
)

// Store — операции с очередью (реализуется store.DB).
type Store interface {
	OutboxDestinations(ctx context.Context, now time.Time, limit int) ([]string, error)
	ClaimOutboxBatch(ctx context.Context, claim store.OutboxClaim) (store.OutboxBatch, error)
	AckOutbox(ctx context.Context, ids []int64) error
	FailOutbox(ctx context.Context, failure store.OutboxFailure) error
	ReleaseOutbox(ctx context.Context, ids []int64) error
	DeadLetterOutbox(ctx context.Context, dead []store.OutboxDead) error
	OutboxMetrics(ctx context.Context) (store.OutboxStats, error)
}

// Sender — отправка федеративной транзакции (реализуется transport.Client).
type Sender interface {
	Send(ctx context.Context, destination, method, path string, body []byte) ([]byte, int, error)
}

// PDUSource собирает федеративные PDU по event_id (подпись своих событий).
// Событие, которого нет, в ответе отсутствует.
type PDUSource func(ctx context.Context, eventIDs []string) (map[string]json.RawMessage, error)

// Options — параметры воркера.
type Options struct {
	Origin     string
	BatchSize  int
	MaxPerTick int
	// Concurrency - сколько destination отправляются одновременно: медленный
	// получатель не задерживает остальных.
	Concurrency int
	// DeliverTimeout - предел одной отправки (захват, сборка, запрос, фиксация).
	DeliverTimeout time.Duration
	// Lease - аренда пачки в БД; больше DeliverTimeout.
	Lease time.Duration
	// MaxAttempts - после стольких неудачных попыток элемент уходит в dead-letter.
	MaxAttempts int
	// MaxRejects - предел для элемента, который получатель отвергает 4xx
	// поодиночке: такой отказ почти всегда детерминирован.
	MaxRejects int
	// MaxAge - элемент старше уходит в dead-letter без отправки (0 - без срока).
	MaxAge     time.Duration
	MinBackoff time.Duration
	MaxBackoff time.Duration
	PDUs       PDUSource
	Now        func() time.Time
	Logger     *slog.Logger
}

// Worker — отправка накопленных PDU и EDU по destination.
type Worker struct {
	store  Store
	sender Sender
	opts   Options

	txnPrefix string
	txnSeq    atomic.Int64

	slots    chan struct{}
	mu       sync.Mutex
	inFlight map[string]struct{}
	running  sync.WaitGroup
}

// New создаёт воркер.
func New(st Store, sender Sender, opts Options) *Worker {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}

	if opts.MaxPerTick <= 0 {
		opts.MaxPerTick = 100
	}

	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}

	if opts.DeliverTimeout <= 0 {
		opts.DeliverTimeout = 90 * time.Second
	}

	if opts.Lease <= opts.DeliverTimeout {
		opts.Lease = 2 * opts.DeliverTimeout
	}

	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 20
	}

	if opts.MaxRejects <= 0 || opts.MaxRejects > opts.MaxAttempts {
		opts.MaxRejects = min(5, opts.MaxAttempts)
	}

	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 5 * time.Second
	}

	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = time.Hour
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	var prefix [4]byte

	_, _ = rand.Read(prefix[:])

	return &Worker{
		store: st, sender: sender, opts: opts,
		txnPrefix: hex.EncodeToString(prefix[:]),
		slots:     make(chan struct{}, opts.Concurrency),
		inFlight:  map[string]struct{}{},
	}
}

// Tick делает один проход по очереди, дожидается отправок и возвращает статистику.
func (w *Worker) Tick(ctx context.Context) (store.OutboxStats, error) {
	if _, err := w.round(ctx, false); err != nil {
		return store.OutboxStats{}, err
	}

	return w.store.OutboxMetrics(ctx)
}

// Drain доставляет очередь, пока проходы что-то доставляют, или до отмены
// контекста (graceful shutdown). Сначала дожидается отправок Run.
//
// Раньше Drain делал ровно один Tick: условие «очередь не уменьшается»
// сравнивалось с previous=-1 и было истинно сразу.
func (w *Worker) Drain(ctx context.Context, interval time.Duration) (store.OutboxStats, error) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}

	w.waitInFlight(ctx)

	for ctx.Err() == nil {
		delivered, err := w.round(ctx, false)
		if err != nil {
			return store.OutboxStats{}, err
		}

		// Ничего не доставлено: очередь пуста или получатели недоступны.
		// Недоставленное остается в БД и уйдет после старта.
		if delivered == 0 {
			break
		}

		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}

	stats, err := w.store.OutboxMetrics(context.WithoutCancel(ctx))
	if err != nil {
		return stats, err
	}

	return stats, ctx.Err()
}

// Run крутит воркер с заданным интервалом до отмены контекста. Отправки идут
// в фоне и не обрываются отменой: их дожидается Drain.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.round(ctx, true); err != nil {
				w.opts.Logger.Warn("federation outbox: tick failed", "error", err)

				continue
			}

			stats, err := w.store.OutboxMetrics(ctx)
			if err == nil && stats.Depth > 0 {
				w.opts.Logger.Info("federation outbox",
					"depth", stats.Depth, "oldest", stats.Oldest.String(), "attempts", stats.Attempts,
					"dead_letters", stats.DeadLetters)
			}
		}
	}
}

// round запускает отправку готовых destination параллельно (не больше
// Concurrency). background=false - ждет завершения и возвращает число
// доставленных элементов; background=true - не ждет, отправки отвязаны от
// отмены ctx.
func (w *Worker) round(ctx context.Context, background bool) (int64, error) {
	destinations, err := w.store.OutboxDestinations(ctx, w.opts.Now(), w.opts.MaxPerTick)
	if err != nil {
		return 0, err
	}

	var (
		delivered atomic.Int64
		wg        sync.WaitGroup
	)

	for _, destination := range destinations {
		if !w.begin(destination) {
			continue
		}

		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			w.end(destination)

			return delivered.Load(), nil
		}

		deliverCtx := ctx
		if background {
			deliverCtx = context.WithoutCancel(ctx)
		}

		wg.Add(1)
		w.running.Add(1)

		go func() {
			defer func() {
				<-w.slots
				w.end(destination)
				w.running.Done()
				wg.Done()
			}()

			sendCtx, cancel := context.WithTimeout(deliverCtx, w.opts.DeliverTimeout)
			defer cancel()

			n, err := w.deliver(sendCtx, destination)
			delivered.Add(int64(n))

			if err != nil {
				w.opts.Logger.Warn("federation outbox: delivery failed", "destination", destination, "error", err)
			}
		}()
	}

	if !background {
		wg.Wait()
	}

	return delivered.Load(), nil
}

// begin отмечает destination занятым в этом процессе (одна пачка за раз).
func (w *Worker) begin(destination string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, busy := w.inFlight[destination]; busy {
		return false
	}

	w.inFlight[destination] = struct{}{}

	return true
}

// end снимает отметку destination.
func (w *Worker) end(destination string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.inFlight, destination)
}

// waitInFlight ждет фоновые отправки Run или отмену ctx.
func (w *Worker) waitInFlight(ctx context.Context) {
	done := make(chan struct{})

	go func() {
		w.running.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// newTxnID — уникальный txn_id новой пачки: время, случайный префикс процесса, счетчик.
func (w *Worker) newTxnID(now time.Time) string {
	return strconv.FormatInt(now.UnixMilli(), 10) + "." + w.txnPrefix + "." +
		strconv.FormatInt(w.txnSeq.Add(1), 10)
}

// deliver отправляет одну пачку destination и возвращает число доставленных элементов.
func (w *Worker) deliver(ctx context.Context, destination string) (int, error) {
	now := w.opts.Now()

	batch, err := w.store.ClaimOutboxBatch(ctx, store.OutboxClaim{
		Destination: destination, BatchSize: w.opts.BatchSize, Now: now,
		Lease: w.opts.Lease, NewTxnID: w.newTxnID(now),
	})
	if err != nil {
		return 0, err
	}

	if len(batch.Items) == 0 {
		return 0, nil
	}

	items, pdus, edus, dead, err := w.prepare(ctx, batch.Items, now)
	if err != nil {
		// PDU не собрать (БД): пачка вернется после аренды тем же txn_id.
		_ = w.store.ReleaseOutbox(context.WithoutCancel(ctx), itemIDs(batch.Items))

		return 0, err
	}

	if err := w.store.DeadLetterOutbox(ctx, dead); err != nil {
		return 0, err
	}

	if len(items) == 0 {
		return 0, nil
	}

	if pdus == nil {
		pdus = []json.RawMessage{}
	}

	if edus == nil {
		edus = []json.RawMessage{}
	}

	// Без HTML-экранирования: json.Marshal переписал бы <, >, & и U+2028 внутри
	// PDU в \u-последовательности, а PDU уходит ровно в том виде, в каком хранится.
	var buf bytes.Buffer

	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(map[string]any{
		"origin":           w.opts.Origin,
		"origin_server_ts": now.UnixMilli(),
		"pdus":             pdus,
		"edus":             edus,
	}); err != nil {
		return 0, fmt.Errorf("outbox: marshal transaction: %w", err)
	}

	body := bytes.TrimRight(buf.Bytes(), "\n")

	respBody, status, sendErr := w.sender.Send(ctx, destination, "PUT", "/_matrix/federation/v1/send/"+batch.TxnID, body)

	// Результат фиксируем и при отмене ctx: иначе пачка висит до конца аренды.
	saveCtx := context.WithoutCancel(ctx)

	switch {
	case sendErr == nil && status/100 == 2:
		return w.accepted(saveCtx, destination, items, respBody)
	case errors.Is(sendErr, transport.ErrDeniedByPolicy):
		return 0, w.deadAll(saveCtx, items, "destination denied by federation policy")
	case errors.Is(sendErr, transport.ErrBackoff):
		// Отправки не было: попытку не считаем, txn_id сохраняется.
		return 0, w.store.ReleaseOutbox(saveCtx, itemIDs(items))
	}

	failErr := w.failed(saveCtx, items, status, sendErr, now)
	if failErr != nil {
		return 0, failErr
	}

	if sendErr != nil {
		return 0, sendErr
	}

	// Тело отказа (errcode/error) обязательно в ошибке: без него 403 от Synapse
	// не диагностируется — непонятно, подпись PDU или auth-проверка.
	return 0, fmt.Errorf("outbox: %s returned HTTP %d: %s", destination, status, snippet(respBody))
}

// prepare собирает PDU и EDU пачки. Элементы старше MaxAge и события, которых
// больше нет (purge), уходят в dead-letter. EDU (typing, receipts, to-device,
// device lists) лежат в элементе готовым JSON и идут в "edus" транзакции:
// раньше транзакция всегда уходила с пустым "edus".
func (w *Worker) prepare(ctx context.Context, batch []store.OutboxItem, now time.Time) (
	items []store.OutboxItem, pdus, edus []json.RawMessage, dead []store.OutboxDead, err error,
) {
	var eventIDs []string

	for i := range batch {
		if batch[i].EventID != "" && len(batch[i].PDU) == 0 {
			eventIDs = append(eventIDs, batch[i].EventID)
		}
	}

	built := map[string]json.RawMessage{}

	if len(eventIDs) > 0 {
		if w.opts.PDUs == nil {
			return nil, nil, nil, nil, errors.New("outbox: pdu source is not configured")
		}

		built, err = w.opts.PDUs(ctx, eventIDs)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("outbox: build pdus: %w", err)
		}
	}

	for i := range batch {
		item := batch[i]

		if w.opts.MaxAge > 0 && !item.QueuedAt.IsZero() && now.Sub(item.QueuedAt) > w.opts.MaxAge {
			dead = append(dead, store.OutboxDead{ID: item.ID, Reason: "expired: older than " + w.opts.MaxAge.String()})

			continue
		}

		if len(item.EDU) > 0 && item.EventID == "" && len(item.PDU) == 0 {
			items = append(items, item)
			edus = append(edus, json.RawMessage(item.EDU))

			continue
		}

		pdu := json.RawMessage(item.PDU)

		if len(pdu) == 0 && item.EventID != "" {
			pdu = built[item.EventID]
		}

		if len(pdu) == 0 {
			dead = append(dead, store.OutboxDead{ID: item.ID, Reason: "event not found: " + item.EventID})

			continue
		}

		items = append(items, item)
		pdus = append(pdus, pdu)
	}

	return items, pdus, edus, dead, nil
}

// sendResponse — ответ PUT /send: результат обработки по event_id.
type sendResponse struct {
	PDUs map[string]struct {
		Error string `json:"error"`
	} `json:"pdus"`
}

// accepted фиксирует принятую транзакцию. PDU, отвергнутые получателем
// (pdus.<id>.error), повторно не шлются - получатель их уже разобрал, -
// и уходят в dead-letter с причиной (раньше 200 подтверждался без разбора).
func (w *Worker) accepted(ctx context.Context, destination string, items []store.OutboxItem, body []byte) (int, error) {
	var resp sendResponse

	if len(body) > 0 {
		if err := json.Unmarshal(body, &resp); err != nil {
			w.opts.Logger.Warn("federation outbox: unparsable /send response",
				"destination", destination, "error", err)
		}
	}

	var (
		acked []int64
		dead  []store.OutboxDead
	)

	for i := range items {
		eventID := itemEventID(items[i])

		if result, ok := resp.PDUs[eventID]; ok && eventID != "" && result.Error != "" {
			w.opts.Logger.Warn("federation outbox: pdu rejected",
				"destination", destination, "event_id", eventID, "error", result.Error)

			dead = append(dead, store.OutboxDead{ID: items[i].ID, Reason: "rejected by destination: " + result.Error})

			continue
		}

		acked = append(acked, items[i].ID)
	}

	if err := w.store.AckOutbox(ctx, acked); err != nil {
		return 0, err
	}

	if err := w.store.DeadLetterOutbox(ctx, dead); err != nil {
		return len(acked), err
	}

	return len(acked), nil
}

// failed фиксирует неудачу. Отказ 4xx (кроме 401/408/429) - получатель
// транзакцию не принял: txn_id снимается, пачка из нескольких элементов
// разбирается по одному (ядовитый элемент не блокирует остальных). Иначе
// (сеть, 5xx) получатель мог ее принять: повтор той же пачкой с тем же
// txn_id. Элементы, исчерпавшие MaxAttempts (MaxRejects для отказов), уходят
// в dead-letter.
func (w *Worker) failed(ctx context.Context, items []store.OutboxItem, status int, sendErr error, now time.Time) error {
	rejected := status >= 400 && status < 500 &&
		status != 401 && status != 408 && status != 429

	reason := fmt.Sprintf("HTTP %d", status)
	if sendErr != nil {
		reason = sendErr.Error()
	}

	limit := w.opts.MaxAttempts
	if rejected {
		limit = w.opts.MaxRejects
	}

	maxAttempts := 0

	var (
		retry []int64
		dead  []store.OutboxDead
	)

	for i := range items {
		attempts := items[i].Attempts + 1

		// Отвергнутая пачка из нескольких элементов - еще не вина элемента:
		// по одному он получит свой шанс.
		if attempts >= limit && (!rejected || len(items) == 1) {
			dead = append(dead, store.OutboxDead{
				ID: items[i].ID, Reason: fmt.Sprintf("gave up after %d attempts: %s", attempts, reason),
			})

			continue
		}

		retry = append(retry, items[i].ID)
		maxAttempts = max(maxAttempts, items[i].Attempts)
	}

	if err := w.store.DeadLetterOutbox(ctx, dead); err != nil {
		return err
	}

	failure := store.OutboxFailure{
		IDs:     retry,
		RetryAt: now.Add(backoff(maxAttempts, w.opts.MinBackoff, w.opts.MaxBackoff)),
		KeepTxn: !rejected,
		Error:   snippet([]byte(reason)),
	}

	if rejected && len(items) > 1 {
		// Сразу по одному: виноват, скорее всего, один элемент.
		failure.Isolate = true
		failure.RetryAt = now
	}

	return w.store.FailOutbox(ctx, failure)
}

// deadAll переносит все элементы в dead-letter с одной причиной.
func (w *Worker) deadAll(ctx context.Context, items []store.OutboxItem, reason string) error {
	dead := make([]store.OutboxDead, 0, len(items))

	for i := range items {
		dead = append(dead, store.OutboxDead{ID: items[i].ID, Reason: reason})
	}

	return w.store.DeadLetterOutbox(ctx, dead)
}

// itemEventID — event_id элемента: из ссылки или из готового PDU (v1/v2).
func itemEventID(item store.OutboxItem) string {
	if item.EventID != "" {
		return item.EventID
	}

	var object struct {
		EventID string `json:"event_id"`
	}

	if len(item.PDU) > 0 && json.Unmarshal(item.PDU, &object) == nil {
		return object.EventID
	}

	return ""
}

// itemIDs — id элементов.
func itemIDs(items []store.OutboxItem) []int64 {
	ids := make([]int64, 0, len(items))

	for i := range items {
		ids = append(ids, items[i].ID)
	}

	return ids
}

// backoff считает задержку: 2^attempts * min, но не больше max.
func backoff(attempts int, minDelay, maxDelay time.Duration) time.Duration {
	delay := minDelay

	for i := 0; i < attempts && delay < maxDelay; i++ {
		delay *= 2
	}

	if delay > maxDelay {
		delay = maxDelay
	}

	return delay
}

// snippet — тело ответа в одну строку, не длиннее 400 символов.
func snippet(body []byte) string {
	const limit = 400

	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}

	if text == "" {
		text = "<пустое тело>"
	}

	return text
}
