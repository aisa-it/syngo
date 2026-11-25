package pushers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Доставка push на шлюзы. Раньше push уходил из одной горутины на
// событие, последовательно и с общим бюджетом 10 с: один медленный шлюз лишал
// push остальных, ответ шлюза (rejected) не читался, повторов не было.
//
// Теперь у каждого pusher своя ограниченная очередь и свой обработчик:
//   - Enqueue не блокирует отправку события (при переполнении уведомление
//     отбрасывается и учитывается в метрике);
//   - недоступный шлюз задерживает только свой pusher;
//   - повторы с экспоненциальной задержкой при сетевой ошибке, 5xx и 429;
//   - pushkeys из rejected ответа шлюза передаются в OnRejected (удаление pusher);
//   - ответ шлюза читается не больше MaxResponseBytes.

// Notification - одно уведомление на один pusher.
type Notification struct {
	UserID  string
	AppID   string
	Pushkey string
	URL     string
	Body    []byte
}

func (n Notification) key() string {
	return n.UserID + "\x00" + n.AppID + "\x00" + n.Pushkey
}

// DeliveryOptions - параметры доставки; нулевые значения заменяются дефолтами.
type DeliveryOptions struct {
	// Client - HTTP-клиент с защитой от SSRF (собирает вызывающий).
	Client *http.Client
	// QueueSize - очередь одного pusher (по умолчанию 32).
	QueueSize int
	// MaxActive - одновременно активных очередей pushers (по умолчанию 1024):
	// предел числа горутин доставки.
	MaxActive int
	// MaxAttempts - попыток на уведомление, включая первую (по умолчанию 5).
	MaxAttempts int
	// MinBackoff/MaxBackoff - задержка между попытками (1 с ... 1 мин).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// MaxResponseBytes - сколько читать из ответа шлюза (64 КиБ).
	MaxResponseBytes int64
	// OnRejected вызывается с pushkeys, которые шлюз вернул в rejected.
	OnRejected func(ctx context.Context, userID, appID string, pushkeys []string)
	Logger     *slog.Logger
}

// DeliveryStats - счетчики для /metrics.
type DeliveryStats struct {
	Sent, Failed, Retried, Dropped, Rejected int64
}

// Dispatcher - очереди доставки push по pushers.
type Dispatcher struct {
	opts DeliveryOptions

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	queues map[string]chan Notification
	closed bool

	sent, failed, retried, dropped, rejected atomic.Int64
}

// NewDispatcher создает диспетчер; обработчики стартуют по мере появления
// уведомлений и завершаются, когда очередь pusher пуста.
func NewDispatcher(opts DeliveryOptions) *Dispatcher {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}

	if opts.QueueSize <= 0 {
		opts.QueueSize = 32
	}

	if opts.MaxActive <= 0 {
		opts.MaxActive = 1024
	}

	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}

	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}

	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = max(time.Minute, opts.MinBackoff)
	}

	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 64 << 10
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Dispatcher{opts: opts, ctx: ctx, cancel: cancel, queues: map[string]chan Notification{}}
}

// Enqueue ставит уведомление в очередь pusher без ожидания. false - очередь
// переполнена, предел активных очередей исчерпан или диспетчер остановлен.
func (d *Dispatcher) Enqueue(n Notification) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		d.dropped.Add(1)

		return false
	}

	key := n.key()

	queue, ok := d.queues[key]
	if !ok {
		if len(d.queues) >= d.opts.MaxActive {
			d.dropped.Add(1)

			return false
		}

		queue = make(chan Notification, d.opts.QueueSize)
		d.queues[key] = queue
		d.wg.Add(1)

		go d.run(key, queue)
	}

	select {
	case queue <- n:
		return true
	default:
		d.dropped.Add(1)

		return false
	}
}

// run обрабатывает очередь одного pusher, пока в ней есть уведомления.
func (d *Dispatcher) run(key string, queue chan Notification) {
	defer d.wg.Done()

	defer func() {
		// Паника в фоне не должна ронять homeserver.
		if rec := recover(); rec != nil {
			d.opts.Logger.Error("push: delivery panic", "panic", fmt.Sprint(rec))
			d.mu.Lock()
			delete(d.queues, key)
			d.mu.Unlock()
		}
	}()

	for {
		d.mu.Lock()

		var (
			n  Notification
			ok bool
		)

		select {
		case n = <-queue:
			ok = true
		default:
		}

		if !ok || d.ctx.Err() != nil {
			// Очередь удаляется под тем же мьютексом, что и Enqueue: уведомление
			// не может попасть в очередь, которую уже никто не читает.
			if d.queues[key] == queue {
				delete(d.queues, key)
			}

			d.dropped.Add(int64(len(queue)))
			d.mu.Unlock()

			return
		}

		d.mu.Unlock()

		d.deliver(n)
	}
}

// deliver отправляет уведомление с повторами.
func (d *Dispatcher) deliver(n Notification) {
	backoff := d.opts.MinBackoff

	for attempt := 1; ; attempt++ {
		retry, err := d.attempt(n)
		if err == nil {
			d.sent.Add(1)

			return
		}

		if !retry || attempt >= d.opts.MaxAttempts {
			d.failed.Add(1)
			d.opts.Logger.Warn("push: delivery failed",
				"gateway", RedactURL(n.URL), "attempts", attempt, "error", err)

			return
		}

		d.retried.Add(1)

		timer := time.NewTimer(backoff)
		select {
		case <-d.ctx.Done():
			timer.Stop()
			d.failed.Add(1)

			return
		case <-timer.C:
		}

		backoff = min(backoff*2, d.opts.MaxBackoff)
	}
}

// errGatewayStatus - шлюз ответил не 2xx.
var errGatewayStatus = errors.New("push: gateway status")

// attempt - одна попытка. retry сообщает, имеет ли смысл повторять.
func (d *Dispatcher) attempt(n Notification) (bool, error) {
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, n.URL, bytes.NewReader(n.Body))
	if err != nil {
		return false, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := d.opts.Client.Do(req)
	if err != nil {
		return d.ctx.Err() == nil, scrubURLError(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, d.opts.MaxResponseBytes))

	switch {
	case resp.StatusCode/100 == 2:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		return true, fmt.Errorf("%w %d", errGatewayStatus, resp.StatusCode)
	default:
		// 3xx (редиректы не выполняются) и 4xx повторять бессмысленно.
		return false, fmt.Errorf("%w %d", errGatewayStatus, resp.StatusCode)
	}

	var parsed struct {
		Rejected []string `json:"rejected"`
	}

	if json.Unmarshal(body, &parsed) == nil && len(parsed.Rejected) > 0 && d.opts.OnRejected != nil {
		// Удаляются только свои pushkeys: шлюз не может удалить чужой pusher.
		own := make([]string, 0, 1)

		for _, key := range parsed.Rejected {
			if key == n.Pushkey {
				own = append(own, key)
			}
		}

		if len(own) > 0 {
			d.rejected.Add(int64(len(own)))
			d.opts.OnRejected(d.ctx, n.UserID, n.AppID, own)
		}
	}

	return false, nil
}

// Stats возвращает счетчики доставки.
func (d *Dispatcher) Stats() DeliveryStats {
	return DeliveryStats{
		Sent: d.sent.Load(), Failed: d.failed.Load(), Retried: d.retried.Load(),
		Dropped: d.dropped.Load(), Rejected: d.rejected.Load(),
	}
}

// Active - число очередей pushers с необработанными уведомлениями.
func (d *Dispatcher) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.queues)
}

// Pending - уведомления в очередях, еще не взятые в доставку:
// растет, когда шлюз push отвечает медленно или недоступен.
func (d *Dispatcher) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	pending := 0
	for _, queue := range d.queues {
		pending += len(queue)
	}

	return pending
}

// MaxActive - предел одновременно активных очередей (для алерта насыщения).
func (d *Dispatcher) MaxActive() int { return d.opts.MaxActive }

// Close останавливает доставку: новые уведомления не принимаются, текущие
// запросы прерываются, ожидание - до отмены ctx.
func (d *Dispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	d.cancel()

	done := make(chan struct{})

	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RedactURL оставляет от адреса шлюза схему и хост: в пути и query бывают
// токены шлюза, им не место в логах.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "invalid"
	}

	return parsed.Scheme + "://" + parsed.Host
}

// scrubURLError убирает полный URL из *url.Error (он попадает в лог).
func scrubURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s %s: %w", urlErr.Op, RedactURL(urlErr.URL), urlErr.Err)
	}

	return err
}
