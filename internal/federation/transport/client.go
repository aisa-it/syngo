package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

// Ошибки клиента.
var (
	ErrDeniedByPolicy = errors.New("transport: destination is not allowed")
	ErrBackoff        = errors.New("transport: destination is in backoff")
	ErrTooLarge       = errors.New("transport: response body is too large")
)

// DestinationStore — состояние ретраев (реализуется store.DB).
type DestinationStore interface {
	Destination(ctx context.Context, name string) (store.DestinationState, error)
	SaveDestinationFailure(ctx context.Context, name string, at time.Time, retryInterval time.Duration) error
	SaveDestinationSuccess(ctx context.Context, name string) error
}

// Options — настройки клиента федерации.
type Options struct {
	Resolver *Resolver
	// Scheme — схема федеративных запросов (https по умолчанию, http для стенда).
	Scheme string
	// InsecureSkipVerify отключает проверку TLS-сертификата (локальный стенд с self-signed CA).
	InsecureSkipVerify bool
	// RootCAs - доверенные корни (nil - системные; в тестах - CA httptest).
	RootCAs *x509.CertPool
	// AddressPolicy - допустимые IP назначения (nil - DefaultAddressPolicy).
	AddressPolicy    *AddressPolicy
	Key              *signing.Key
	Origin           string
	Store            DestinationStore
	AllowHosts       []string
	DenyHosts        []string
	MaxResponseBytes int64
	ConnectTimeout   time.Duration
	RequestTimeout   time.Duration
	MinBackoff       time.Duration
	MaxBackoff       time.Duration
	Jitter           func(time.Duration) time.Duration
	Now              func() time.Time
}

// Client — исходящие федеративные запросы с подписью, ретраями и лимитами.
type Client struct {
	opts    Options
	http    *http.Client
	metrics Metrics
}

// NewClient создаёт клиент федерации.
func NewClient(opts Options) *Client {
	if opts.Resolver == nil {
		opts.Resolver = NewResolver()
	}

	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 1 << 20
	}

	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 5 * time.Second
	}

	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 30 * time.Second
	}

	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 5 * time.Second
	}

	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = time.Hour
	}

	if opts.Jitter == nil {
		opts.Jitter = func(d time.Duration) time.Duration {
			// ±20% jitter: не синхронизируем ретраи всех серверов.
			delta := float64(d) * 0.2

			return d + time.Duration((rand.Float64()*2-1)*delta) //nolint:gosec // jitter, не криптография
		}
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Scheme != "" {
		opts.Resolver.Scheme = opts.Scheme
	}

	if opts.AddressPolicy == nil {
		opts.AddressPolicy = DefaultAddressPolicy()
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    opts.RootCAs,
		// Только локальный стенд: валидация конфига запрещает опцию в prod.
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // стенд с self-signed CA
	}

	// well-known ходит с той же политикой адресов и теми же корнями.
	if opts.Resolver.Policy == nil {
		opts.Resolver.Policy = opts.AddressPolicy
	}

	if opts.Resolver.TLSConfig == nil {
		opts.Resolver.TLSConfig = tlsConfig
	}

	dialer := &Dialer{Policy: opts.AddressPolicy, LookupIP: opts.Resolver.LookupIP, Timeout: opts.ConnectTimeout}

	transport := &http.Transport{
		// Proxy: nil - прокси из окружения обходил бы проверку адреса назначения.
		Proxy: nil,
		// URL запроса несет TLSName:port (по нему Go ставит SNI и проверяет
		// сертификат), реальный адрес подключения (цель SRV, delegated) - в контексте.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if real, ok := ctx.Value(dialAddressKey{}).(string); ok && real != "" {
				address = real
			}

			return dialer.DialContext(ctx, network, address)
		},
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    opts.ConnectTimeout,
		MaxIdleConnsPerHost:    4,
		MaxResponseHeaderBytes: 64 << 10,
	}

	httpClient := &http.Client{
		Timeout:   opts.RequestTimeout,
		Transport: transport,
		// Федеративный API не следует редиректам (как Synapse): иначе 30x уводит
		// подписанный запрос на произвольный, в том числе внутренний, адрес.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &Client{opts: opts, http: httpClient}
}

// dialAddressKey - ключ контекста с реальным адресом подключения запроса.
type dialAddressKey struct{}

// requestURL собирает URL запроса к цели: authority - TLSName:port, чтобы SNI и
// проверка сертификата шли на server_name (или delegated_hostname), а не на имя
// цели SRV или IP. Пул соединений Go ключуется по этому authority.
func requestURL(ctx context.Context, target Target, path string) (reqCtx context.Context, url string) {
	if target.TLSName == "" {
		return ctx, target.Scheme + "://" + target.Host + path
	}

	_, port, err := net.SplitHostPort(target.Host)
	if err != nil {
		return ctx, target.Scheme + "://" + target.Host + path
	}

	authority := net.JoinHostPort(target.TLSName, port)

	return context.WithValue(ctx, dialAddressKey{}, target.Host), target.Scheme + "://" + authority + path
}

// RequestOptions - параметры запроса сверх Send.
type RequestOptions struct {
	// Unsigned - без X-Matrix: запросы ключей (/_matrix/key/v2/*) подписи не требуют,
	// и ключ сервера для них не нужен.
	Unsigned bool
	// IgnoreBackoff - не учитывать паузу destination: ключи нужны для проверки
	// входящего запроса (ignore_backoff в Synapse). Неудача все равно учитывается.
	IgnoreBackoff bool
	// MaxResponseBytes - лимит ответа вместо Options.MaxResponseBytes (0 - общий):
	// ответ send_join несет все состояние комнаты.
	MaxResponseBytes int64
}

// Send отправляет подписанный запрос на destination.
//
// Возвращает тело ответа (ограниченное) и статус. Ошибка сети и 5xx фиксируют
// backoff и возвращают ошибку; прочий ответ сервера сбрасывает состояние
// (сервер доступен), не-2xx с телом возвращается ошибкой с фрагментом тела.
// Слишком большой ответ - ErrTooLarge без backoff: это свойство ответа, а не
// недоступность сервера.
func (c *Client) Send(ctx context.Context, destination, method, path string, body []byte) (response []byte, status int, err error) {
	return c.Do(ctx, destination, method, path, body, RequestOptions{})
}

// Do - Send с параметрами запроса.
func (c *Client) Do(ctx context.Context, destination, method, path string, body []byte, opts RequestOptions) (response []byte, status int, err error) {
	if err := c.checkPolicy(destination); err != nil {
		return nil, 0, err
	}

	if !opts.IgnoreBackoff {
		if err := c.checkBackoff(ctx, destination); err != nil {
			return nil, 0, err
		}
	}

	if !opts.Unsigned && c.opts.Key == nil {
		return nil, 0, errors.New("transport: no signing key")
	}

	targets, err := c.opts.Resolver.Targets(ctx, destination)
	if err != nil {
		// Не смогли найти адрес — это тоже неудача доставки: учитываем в backoff.
		c.metrics.failure(destination, "resolve")
		c.recordFailure(ctx, destination)

		return nil, 0, err
	}

	var (
		resp       *http.Response
		usedTarget Target
	)

	// Fallback по портам: 8448 → 443. Переходим к следующему кандидату только при сетевой
	// ошибке (dial/TLS/timeout), но не при HTTP-ответе: сервер доступен, значит порт верный.
	for i, target := range targets {
		reqCtx, targetURL := requestURL(ctx, target, path)

		req, reqErr := http.NewRequestWithContext(reqCtx, method, targetURL, bytes.NewReader(body))
		if reqErr != nil {
			return nil, 0, fmt.Errorf("transport: build request: %w", reqErr)
		}

		if target.HostHeader != "" {
			req.Host = target.HostHeader
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "syngo/1.0")

		if !opts.Unsigned {
			auth, authErr := signing.MatrixAuthHeader(*c.opts.Key, c.opts.Origin, destination, method, path, body)
			if authErr != nil {
				return nil, 0, authErr
			}

			req.Header.Set("Authorization", auth)
		}

		if i == 0 {
			c.metrics.request(destination)
		}

		resp, err = c.http.Do(req)
		if err == nil {
			usedTarget = target

			break
		}

		if ctx.Err() != nil {
			c.metrics.failure(destination, "network")
			c.recordFailure(ctx, destination)

			return nil, 0, fmt.Errorf("transport: request to %s: %w", destination, err)
		}

		// Последний кандидат — фиксируем неудачу.
		if i == len(targets)-1 {
			c.metrics.failure(destination, "network")
			c.recordFailure(ctx, destination)

			return nil, 0, fmt.Errorf("transport: request to %s (%s): %w", destination, target.Host, err)
		}
	}

	// Запоминаем рабочий адрес, чтобы не пробовать недоступный порт каждый раз.
	if usedTarget.Host != "" {
		c.opts.Resolver.CacheTarget(destination, usedTarget)
	}
	defer func() { _ = resp.Body.Close() }()

	maxResponse := c.opts.MaxResponseBytes
	if opts.MaxResponseBytes > 0 {
		maxResponse = opts.MaxResponseBytes
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		c.metrics.failure(destination, "read")
		c.recordFailure(ctx, destination)

		return nil, resp.StatusCode, fmt.Errorf("transport: read response: %w", err)
	}

	// Слишком большой ответ - не повод ставить на паузу весь сервер.
	if int64(len(raw)) > maxResponse {
		c.metrics.failure(destination, "too_large")
		c.metrics.tooLarge.Add(1)

		return nil, resp.StatusCode, ErrTooLarge
	}

	// 5xx - сервер неисправен: backoff (раньше 5xx с телом уходил мимо).
	if resp.StatusCode >= 500 {
		c.metrics.failure(destination, fmt.Sprintf("http_%d", resp.StatusCode))
		c.recordFailure(ctx, destination)

		return raw, resp.StatusCode, fmt.Errorf("transport: %s returned HTTP %d%s", destination, resp.StatusCode, bodySnippet(raw))
	}

	// Любой другой ответ - сервер доступен: сбрасываем backoff.
	c.metrics.success(destination)
	c.recordSuccess(ctx, destination)

	// Тело отказа полезно вызывающему: Synapse объясняет 401/403 причиной (auth/подпись).
	if resp.StatusCode/100 != 2 && len(raw) > 0 {
		return nil, resp.StatusCode, fmt.Errorf("transport: %s returned HTTP %d%s", destination, resp.StatusCode, bodySnippet(raw))
	}

	return raw, resp.StatusCode, nil
}

// bodySnippet - начало тела ответа для текста ошибки.
func bodySnippet(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}

	snippet := string(raw)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}

	return ": " + snippet
}

// recordSuccess сбрасывает состояние ретраев destination.
func (c *Client) recordSuccess(ctx context.Context, destination string) {
	if c.opts.Store == nil {
		return
	}

	if err := c.opts.Store.SaveDestinationSuccess(ctx, destination); err != nil {
		// не критично: успех важнее чистки состояния
		_ = err
	}
}

// Allowed сообщает, разрешен ли destination политикой allow_hosts/deny_hosts:
// outbox не ставит события запрещенным получателям.
func (c *Client) Allowed(destination string) bool {
	return c.checkPolicy(destination) == nil
}

// checkPolicy проверяет allowlist/denylist.
func (c *Client) checkPolicy(destination string) error {
	return HostPolicy{Allow: c.opts.AllowHosts, Deny: c.opts.DenyHosts}.Check(destination)
}

// HostPolicy - allow_hosts/deny_hosts федерации: действует и на исходящие, и на
// входящие запросы. Allow пуст - разрешено все, кроме Deny.
type HostPolicy struct {
	Allow []string
	Deny  []string
}

// Check возвращает ErrDeniedByPolicy, если сервер не допускается.
func (p HostPolicy) Check(serverName string) error {
	host := strings.ToLower(serverName)

	for _, denied := range p.Deny {
		if matchesHost(host, denied) {
			return fmt.Errorf("%w: %s in deny_hosts", ErrDeniedByPolicy, serverName)
		}
	}

	if len(p.Allow) == 0 {
		return nil
	}

	for _, allowed := range p.Allow {
		if matchesHost(host, allowed) {
			return nil
		}
	}

	return fmt.Errorf("%w: %s is not in allow_hosts", ErrDeniedByPolicy, serverName)
}

// matchesHost сравнивает host с правилом: точное совпадение или суффикс с точкой.
func matchesHost(host, rule string) bool {
	rule = strings.ToLower(strings.TrimSpace(rule))
	if rule == "" {
		return false
	}

	return host == rule || strings.HasSuffix(host, "."+rule)
}

// checkBackoff отказывает, если сервер в паузе после серии неудач.
func (c *Client) checkBackoff(ctx context.Context, destination string) error {
	if c.opts.Store == nil {
		return nil
	}

	state, stateErr := c.opts.Store.Destination(ctx, destination)
	if stateErr != nil {
		return nil //nolint:nilerr // состояние ретраев не должно блокировать отправку
	}

	if state.RetryLastTS == nil || state.RetryInterval <= 0 {
		return nil
	}

	next := state.RetryLastTS.Add(state.RetryInterval)
	if c.opts.Now().Before(next) {
		c.metrics.retrySkipped(destination)

		return fmt.Errorf("%w: %s until %s", ErrBackoff, destination, next.Format(time.RFC3339))
	}

	return nil
}

// recordFailure считает следующий интервал backoff и сохраняет его.
func (c *Client) recordFailure(ctx context.Context, destination string) {
	if c.opts.Store == nil {
		return
	}

	next := c.opts.MinBackoff

	state, err := c.opts.Store.Destination(ctx, destination)
	if err == nil && state.RetryInterval > 0 {
		next = state.RetryInterval * 2
	}

	if next > c.opts.MaxBackoff {
		next = c.opts.MaxBackoff
	}

	next = c.opts.Jitter(next)
	if next < c.opts.MinBackoff {
		next = c.opts.MinBackoff
	}

	if err := c.opts.Store.SaveDestinationFailure(ctx, destination, c.opts.Now(), next); err != nil {
		_ = err
	}
}

// Metrics — счётчики федеративного транспорта без PII.
type Metrics struct {
	requests atomic.Int64
	failures atomic.Int64
	tooLarge atomic.Int64
	skipped  atomic.Int64
	byDest   sync.Map // destination -> *atomic.Int64 (запросы)
}

type destCounter struct{ n atomic.Int64 }

// Snapshot — сводка метрик (для логов/тестов).
type Snapshot struct {
	Requests      int64
	Failures      int64
	RetrySkipped  int64
	TooLarge      int64
	ByDestination map[string]int64
}

func (m *Metrics) request(destination string) {
	m.requests.Add(1)

	counter, ok := m.byDest.Load(destination)
	if !ok {
		counter = &destCounter{}
		m.byDest.Store(destination, counter)
	}

	if counter, ok := counter.(*destCounter); ok {
		counter.n.Add(1)
	}
}

func (m *Metrics) failure(string, string) {
	m.failures.Add(1)
}

func (m *Metrics) success(string) {}

func (m *Metrics) retrySkipped(string) { m.skipped.Add(1) }

// Snapshot возвращает текущие метрики.
func (c *Client) Snapshot() Snapshot {
	snap := Snapshot{
		Requests:      c.metrics.requests.Load(),
		Failures:      c.metrics.failures.Load(),
		RetrySkipped:  c.metrics.skipped.Load(),
		TooLarge:      c.metrics.tooLarge.Load(),
		ByDestination: map[string]int64{},
	}

	c.metrics.byDest.Range(func(key, value any) bool {
		name, _ := key.(string)
		counter, _ := value.(*destCounter)

		if counter != nil {
			snap.ByDestination[name] = counter.n.Load()
		}

		return true
	})

	return snap
}
