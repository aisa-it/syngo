// Package transport реализует сетевой слой федерации (ADR-0007 §5):
// резолв адреса сервера по спеке (IP-литерал/явный порт → well-known → SRV → A/AAAA),
// HTTP-клиент с таймаутами и ограничением тела, защита от SSRF (ip_range_blacklist),
// retry/backoff по destination, allowlist/denylist, лимиты входящих.
package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ошибки резолва.
var (
	ErrInsecureTarget = errors.New("transport: server must use HTTPS")
	ErrNoTarget       = errors.New("transport: cannot resolve server")
)

// Параметры well-known.
const (
	// wellKnownMaxBody - предел ответа /.well-known/matrix/server.
	wellKnownMaxBody = 64 << 10
	// wellKnownMaxRedirects - спека: редиректы well-known выполнять, избегая петель.
	wellKnownMaxRedirects = 5
	// defaultFederationPort - порт федерации по умолчанию.
	defaultFederationPort = 8448
)

// Resolver находит адрес федеративного API для server_name.
//
// Порядок по спеке (Server-Server API, Resolving server names):
//  1. IP-литерал - он и есть адрес (порт или 8448), сертификат на IP;
//  2. имя с явным портом - A/AAAA имени, сертификат на имя;
//  3. /.well-known/matrix/server → delegated_hostname, далее для него те же шаги
//     (IP, явный порт, SRV, 8448), сертификат и Host - delegated_hostname;
//  4. SRV _matrix-fed._tcp (затем устаревший _matrix._tcp) - подключение к цели SRV,
//     но сертификат и Host - server_name, а не имя цели (иначе подмена DNS = MITM);
//  5. A/AAAA имени на 8448 (наш фолбэк - затем 443).
type Resolver struct {
	// HTTPClient - клиент для well-known. nil - безопасный клиент по Policy
	// (подменяется в тестах на httptest).
	HTTPClient *http.Client
	// Scheme - https в проде; в тестах и на http-стенде допустим http.
	Scheme string
	// WellKnownTTL - кэш well-known и выбранного адреса.
	WellKnownTTL time.Duration
	// WellKnownFailureTTL - кэш отсутствия well-known (не ходим за ним на каждый запрос).
	WellKnownFailureTTL time.Duration
	// LookupSRV подменяется в тестах (service, proto, name → записи SRV).
	LookupSRV func(ctx context.Context, service, proto, name string) ([]*net.SRV, error)
	// LookupIP - A/AAAA имени для подключения (в тестах подменяется).
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	// WellKnownURL - адрес well-known для имени (в тестах подменяется).
	WellKnownURL func(scheme, hostname string) string
	// Policy - допустимые адреса для well-known (nil - DefaultAddressPolicy).
	Policy *AddressPolicy
	// TLSConfig - корневые сертификаты/insecure для well-known (nil - системные).
	TLSConfig *tls.Config
	// Now подменяется в тестах.
	Now func() time.Time
	// TargetsOverride подменяется в тестах (порядок кандидатов).
	TargetsOverride func(ctx context.Context, serverName string) ([]Target, error)

	mu    sync.Mutex
	cache map[string]wellKnownEntry
	// portCache - выбранный рабочий адрес (8448 или 443) на TTL.
	portCache map[string]targetEntry
	// wellKnownHTTP - лениво собранный безопасный клиент well-known.
	wellKnownHTTP *http.Client
}

type targetEntry struct {
	target   Target
	expireAt time.Time
}

// wellKnownEntry - результат well-known: delegated ("" - делегирования нет).
type wellKnownEntry struct {
	delegated string
	expireAt  time.Time
}

// Target - адрес федеративного API.
type Target struct {
	ServerName string
	// Host - адрес подключения host:port (имя или IP).
	Host string
	// TLSName - имя для SNI и проверки сертификата ("" - хост из Host).
	TLSName string
	// HostHeader - заголовок Host запроса ("" - по URL).
	HostHeader string
	Scheme     string
}

// NewResolver создаёт резолвер с разумными значениями по умолчанию.
func NewResolver() *Resolver {
	return &Resolver{
		Scheme:              "https",
		WellKnownTTL:        time.Hour,
		WellKnownFailureTTL: 2 * time.Minute,
		LookupSRV: func(ctx context.Context, service, proto, name string) ([]*net.SRV, error) {
			_, records, err := net.DefaultResolver.LookupSRV(ctx, service, proto, name)

			return records, err
		},
		LookupIP:  defaultLookupIP,
		Now:       time.Now,
		cache:     map[string]wellKnownEntry{},
		portCache: map[string]targetEntry{},
	}
}

// Targets возвращает кандидатов адреса в порядке попыток.
//
// Если ранее рабочий адрес закэширован (CacheTarget), возвращается только он - до истечения TTL
// не тратим попытки на недоступный 8448.
func (r *Resolver) Targets(ctx context.Context, serverName string) ([]Target, error) {
	if r.TargetsOverride != nil {
		return r.TargetsOverride(ctx, serverName)
	}

	if cached, ok := r.cachedTarget(serverName); ok {
		return []Target{cached}, nil
	}

	name, err := ParseServerName(serverName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoTarget, err)
	}

	scheme := r.scheme()

	// 1-2. IP-литерал или явный порт: well-known и SRV не используются.
	if name.IP || name.Port != 0 {
		return []Target{{
			ServerName: serverName, Host: name.HostPort(defaultFederationPort),
			TLSName: name.Host, HostHeader: serverName, Scheme: scheme,
		}}, nil
	}

	// 3. Делегирование через well-known.
	if delegated, ok := r.delegation(ctx, name.Host); ok {
		return r.delegatedTargets(ctx, serverName, delegated)
	}

	// 4. SRV самого имени: подключаемся к цели, но сертификат - на server_name.
	if host, ok := r.srv(ctx, name.Host); ok {
		return []Target{{
			ServerName: serverName, Host: host, TLSName: name.Host, HostHeader: name.Host, Scheme: scheme,
		}}, nil
	}

	// 5. A/AAAA на 8448, затем фолбэк на 443.
	return []Target{
		{ServerName: serverName, Host: name.HostPort(defaultFederationPort), TLSName: name.Host, HostHeader: name.Host, Scheme: scheme},
		{ServerName: serverName, Host: name.HostPort(443), TLSName: name.Host, HostHeader: name.Host, Scheme: scheme},
	}, nil
}

// delegatedTargets - шаги 3.1-3.5 спеки для delegated_hostname из well-known.
func (r *Resolver) delegatedTargets(ctx context.Context, serverName, delegated string) ([]Target, error) {
	name, err := ParseServerName(delegated)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoTarget, err)
	}

	scheme := r.scheme()

	if name.IP || name.Port != 0 {
		return []Target{{
			ServerName: serverName, Host: name.HostPort(defaultFederationPort),
			TLSName: name.Host, HostHeader: delegated, Scheme: scheme,
		}}, nil
	}

	if host, ok := r.srv(ctx, name.Host); ok {
		return []Target{{
			ServerName: serverName, Host: host, TLSName: name.Host, HostHeader: name.Host, Scheme: scheme,
		}}, nil
	}

	return []Target{{
		ServerName: serverName, Host: name.HostPort(defaultFederationPort),
		TLSName: name.Host, HostHeader: name.Host, Scheme: scheme,
	}}, nil
}

// srv ищет _matrix-fed._tcp, затем устаревший _matrix._tcp; возвращает host:port цели.
func (r *Resolver) srv(ctx context.Context, hostname string) (string, bool) {
	if r.LookupSRV == nil {
		return "", false
	}

	for _, service := range []string{"matrix-fed", "matrix"} {
		records, err := r.LookupSRV(ctx, service, "tcp", hostname)
		if err != nil || len(records) == 0 {
			continue
		}

		best := records[0]
		for _, rec := range records[1:] {
			if rec.Priority < best.Priority || (rec.Priority == best.Priority && rec.Weight > best.Weight) {
				best = rec
			}
		}

		// Цель "." - сервис явно недоступен (RFC 2782).
		host := strings.TrimSuffix(best.Target, ".")
		if host == "" || best.Port == 0 {
			return "", false
		}

		return net.JoinHostPort(host, strconv.Itoa(int(best.Port))), true
	}

	return "", false
}

// CacheTarget запоминает рабочий адрес сервера на TTL well-known.
func (r *Resolver) CacheTarget(serverName string, target Target) {
	ttl := r.WellKnownTTL
	if ttl <= 0 {
		ttl = time.Hour
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.portCache == nil {
		r.portCache = map[string]targetEntry{}
	}

	r.portCache[serverName] = targetEntry{target: target, expireAt: r.now().Add(ttl)}
}

// cachedTarget возвращает закэшированный рабочий адрес.
func (r *Resolver) cachedTarget(serverName string) (Target, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.portCache[serverName]
	if !ok || r.now().After(entry.expireAt) {
		return Target{}, false
	}

	return entry.target, true
}

// Resolve возвращает первый адрес федеративного API для server_name.
func (r *Resolver) Resolve(ctx context.Context, serverName string) (Target, error) {
	targets, err := r.Targets(ctx, serverName)
	if err != nil {
		return Target{}, err
	}

	if len(targets) == 0 {
		return Target{}, fmt.Errorf("%w: %s", ErrNoTarget, serverName)
	}

	return targets[0], nil
}

// delegation - delegated_hostname из well-known (с кэшем, включая отсутствие).
func (r *Resolver) delegation(ctx context.Context, hostname string) (string, bool) {
	if entry, ok := r.cachedWellKnown(hostname); ok {
		return entry.delegated, entry.delegated != ""
	}

	delegated, err := r.wellKnown(ctx, hostname)
	if err != nil {
		r.storeWellKnown(hostname, "")

		return "", false
	}

	r.storeWellKnown(hostname, delegated)

	return delegated, true
}

// wellKnown запрашивает /.well-known/matrix/server и проверяет m.server по
// грамматике server_name: путь, query, схема и userinfo отвергаются.
func (r *Resolver) wellKnown(ctx context.Context, hostname string) (string, error) {
	scheme := r.scheme()

	url := scheme + "://" + hostname + "/.well-known/matrix/server"
	if r.WellKnownURL != nil {
		url = r.WellKnownURL(scheme, hostname)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}

	resp, err := r.wellKnownClient().Do(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transport: well-known HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, wellKnownMaxBody))
	if err != nil {
		return "", err
	}

	var parsed struct {
		Server string `json:"m.server"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("transport: well-known JSON: %w", err)
	}

	if parsed.Server == "" {
		return "", errors.New("transport: well-known without m.server")
	}

	if strings.Contains(parsed.Server, "://") {
		return "", ErrInsecureTarget
	}

	if _, err := ParseServerName(parsed.Server); err != nil {
		return "", fmt.Errorf("transport: well-known m.server: %w", err)
	}

	return parsed.Server, nil
}

// wellKnownClient - клиент well-known: адреса по Policy, без прокси, редиректы
// ограничены (спека: следовать, избегая петель) и не меняют схему.
func (r *Resolver) wellKnownClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wellKnownHTTP != nil {
		return r.wellKnownHTTP
	}

	policy := r.Policy
	if policy == nil {
		policy = DefaultAddressPolicy()
	}

	dialer := &Dialer{Policy: policy, LookupIP: r.LookupIP, Timeout: 5 * time.Second}

	var tlsConfig *tls.Config
	if r.TLSConfig != nil {
		tlsConfig = r.TLSConfig.Clone()
	}

	r.wellKnownHTTP = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSClientConfig:        tlsConfig,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  5 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
			MaxIdleConnsPerHost:    2,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= wellKnownMaxRedirects {
				return errors.New("transport: well-known: too many redirects")
			}

			if req.URL.Scheme != via[0].URL.Scheme {
				return errors.New("transport: well-known: redirect changes scheme")
			}

			return nil
		},
	}

	return r.wellKnownHTTP
}

// cachedWellKnown возвращает кэш, если он жив.
func (r *Resolver) cachedWellKnown(hostname string) (wellKnownEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.cache[hostname]
	if !ok || r.now().After(entry.expireAt) {
		return wellKnownEntry{}, false
	}

	return entry, true
}

// storeWellKnown кладёт результат в кэш ("" - делегирования нет, короткий TTL).
func (r *Resolver) storeWellKnown(hostname, delegated string) {
	ttl := r.WellKnownTTL
	if ttl <= 0 {
		ttl = time.Hour
	}

	if delegated == "" {
		ttl = r.WellKnownFailureTTL
		if ttl <= 0 {
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cache == nil {
		r.cache = map[string]wellKnownEntry{}
	}

	r.cache[hostname] = wellKnownEntry{delegated: delegated, expireAt: r.now().Add(ttl)}
}

func (r *Resolver) scheme() string {
	if r.Scheme == "" {
		return "https"
	}

	return r.Scheme
}

func (r *Resolver) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}

	return r.Now()
}
