package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Безопасная доставка push-уведомлений:
//   - SSRF: только https (http - только push.allow_http на стенде), путь
//     /_matrix/push/v1/notify, без userinfo; внутренние адреса запрещены и при
//     регистрации (IP-литерал), и при подключении - после резолва DNS, на
//     уровне dial (DNS rebinding: проверяется именно тот адрес, к которому
//     идет подключение);
//   - без редиректов и без прокси из окружения (иначе проверялся бы адрес
//     прокси, а не шлюза);
//   - один транспорт на процесс, таймауты, ограничение ответа (pushers.Dispatcher).

// pushNotifyPath - единственный допустимый путь шлюза (спецификация Push Gateway).
const pushNotifyPath = "/_matrix/push/v1/notify"

// maxPushURLLength - предел длины URL шлюза.
const maxPushURLLength = 2048

// pushBlockedPrefixes - диапазоны, недоступные для доставки push: частные,
// служебные, документационные, multicast, а также IPv6-туннели к IPv4 (NAT64,
// 6to4, Teredo), через которые доступны внутренние IPv4.
var pushBlockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
)

func mustPrefixes(raw ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))

	for _, r := range raw {
		out = append(out, netip.MustParsePrefix(r))
	}

	return out
}

// isUnsafePushAddr - адрес нельзя использовать для доставки push.
func isUnsafePushAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}

	addr = addr.WithZone("").Unmap()

	for _, prefix := range pushBlockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// isUnsafePushIP - то же для net.IP.
func isUnsafePushIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}

	return isUnsafePushAddr(addr)
}

// pushResolver - резолвер имен шлюзов (в тестах подменяется).
type pushResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// pushDialer подключается только к разрешенным адресам: хост резолвится один
// раз, проверяются все адреса, подключение - к проверенному IP; Control
// дополнительно проверяет адрес сокета перед connect.
type pushDialer struct {
	resolver pushResolver
	unsafe   func(netip.Addr) bool
	timeout  time.Duration
}

func (d *pushDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	var addrs []netip.Addr

	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else {
		addrs, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("push: no addresses for %s", host)
	}

	for _, addr := range addrs {
		if d.unsafe(addr) {
			return nil, fmt.Errorf("push: refusing to connect to %s (%s)", host, addr)
		}
	}

	dialer := &net.Dialer{
		Timeout: d.timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || d.unsafe(ap.Addr()) {
				return fmt.Errorf("push: refusing to connect to %s", address)
			}

			return nil
		},
	}

	var lastErr error

	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}

		lastErr = err
	}

	return nil, lastErr
}

// newPushClient - общий клиент доставки push. Защитный dialer выключается
// только push.allow_private (интеграционные тесты, стенд); allow_http его
// больше не отключает.
func (s *Server) newPushClient() *http.Client {
	timeout := s.cfg.Push.Timeout.Std()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	dial := (&net.Dialer{Timeout: timeout}).DialContext

	if !s.cfg.Push.AllowPrivate {
		resolver := s.pushResolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}

		unsafe := s.pushUnsafeAddr
		if unsafe == nil {
			unsafe = isUnsafePushAddr
		}

		dial = (&pushDialer{resolver: resolver, unsafe: unsafe, timeout: timeout}).DialContext
	}

	return newPushHTTPClient(dial, timeout)
}

// newPushHTTPClient собирает клиент: без прокси, без редиректов, с таймаутами.
func newPushHTTPClient(dial func(context.Context, string, string) (net.Conn, error), timeout time.Duration) *http.Client {
	transport := &http.Transport{
		// Proxy: nil - прокси из окружения обходил бы проверку адреса шлюза.
		Proxy:                  nil,
		DialContext:            dial,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    8,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}

	return &http.Client{
		Timeout:   2 * timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// checkPushURL проверяет URL шлюза по конфигу: push.allow_private снимает
// только запрет внутренних адресов, схема и путь проверяются всегда.
func (s *Server) checkPushURL(raw string) error {
	if s.cfg.Push.AllowPrivate {
		_, err := parsePushURL(raw, s.cfg.Push.AllowHTTP)

		return err
	}

	return validatePushURL(raw, s.cfg.Push.AllowHTTP)
}

// validatePushURL проверяет URL шлюза при регистрации pusher и перед отправкой.
func validatePushURL(raw string, allowHTTP bool) error {
	parsed, err := parsePushURL(raw, allowHTTP)
	if err != nil {
		return err
	}

	if ip, err := netip.ParseAddr(parsed.Hostname()); err == nil && isUnsafePushAddr(ip) {
		return fmt.Errorf("push: address %s is not allowed", ip)
	}

	return nil
}

// parsePushURL - форма URL без проверки адреса: схема, путь, userinfo, хост.
func parsePushURL(raw string, allowHTTP bool) (*url.URL, error) {
	if len(raw) > maxPushURLLength {
		return nil, errors.New("push: url is too long")
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("push: invalid url")
	}

	isHTTPS := parsed.Scheme == "https"
	isAllowedHTTP := allowHTTP && parsed.Scheme == "http"

	if !isHTTPS && !isAllowedHTTP {
		return nil, fmt.Errorf("push: scheme %q is not allowed", parsed.Scheme)
	}

	if parsed.User != nil {
		return nil, errors.New("push: userinfo is not allowed")
	}

	if parsed.Path != pushNotifyPath {
		return nil, fmt.Errorf("push: path must be %s", pushNotifyPath)
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, errors.New("push: empty host")
	}

	if _, err := netip.ParseAddr(host); err != nil && !isDNSName(host) {
		return nil, fmt.Errorf("push: host %q is not allowed", host)
	}

	return parsed, nil
}

// isDNSName - имя хоста, а не нестандартная запись IP: "2130706433" и "0x7f.1"
// системный резолвер превращает в 127.0.0.1. Последняя метка должна содержать
// букву и не быть шестнадцатеричным числом.
func isDNSName(host string) bool {
	host = strings.TrimSuffix(host, ".")

	labels := strings.Split(host, ".")
	last := strings.ToLower(labels[len(labels)-1])

	if strings.HasPrefix(last, "0x") {
		return false
	}

	hasLetter := false

	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}

		for _, c := range label {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9', c == '-':
				continue
			default:
				return false
			}
		}
	}

	for _, c := range last {
		if c >= 'a' && c <= 'z' {
			hasLetter = true
		}
	}

	return hasLetter
}
