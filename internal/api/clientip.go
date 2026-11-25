package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
)

// clientIPResolver определяет IP клиента с учетом доверенных прокси.
//
// Прод-схема ставит TLS-прокси перед сервисом: без него у всех клиентов один
// адрес - адрес прокси, и лимиты по IP общие на всех. Заголовки X-Forwarded-For
// и X-Real-IP принимаются только от адресов из server.trusted_proxies: от
// остальных они подделываются клиентом и игнорируются.
type clientIPResolver struct {
	trusted []netip.Prefix
}

// trusts сообщает, что адрес входит в доверенные прокси.
func (c clientIPResolver) trusts(addr netip.Addr) bool {
	for _, prefix := range c.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// resolve возвращает IP клиента запроса.
//
//   - соединение не от доверенного прокси - адрес соединения, заголовки не читаются;
//   - X-Forwarded-For - справа налево, пропуская доверенные прокси: первый
//     недоверенный адрес и есть клиент (левее него список пишет сам клиент);
//   - без X-Forwarded-For - X-Real-IP;
//   - иначе - адрес соединения.
func (c clientIPResolver) resolve(r *http.Request) string {
	peer, ok := parseForwardedAddr(r.RemoteAddr)
	if !ok {
		return remoteHost(r.RemoteAddr)
	}

	if len(c.trusted) == 0 || !c.trusts(peer) {
		return peer.String()
	}

	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		return c.fromForwardedFor(values, peer).String()
	}

	if realIP, ok := parseForwardedAddr(r.Header.Get("X-Real-IP")); ok {
		return realIP.String()
	}

	return peer.String()
}

// fromForwardedFor проходит цепочку X-Forwarded-For справа налево. Несколько
// заголовков склеиваются по порядку (RFC 9110, 5.3). Нераспознанный элемент
// обрывает разбор: левее него доверять нечему, клиентом считается последний
// разобранный адрес.
func (c clientIPResolver) fromForwardedFor(values []string, peer netip.Addr) netip.Addr {
	hops := strings.Split(strings.Join(values, ","), ",")
	client := peer

	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}

		addr, ok := parseForwardedAddr(hop)
		if !ok {
			return client
		}

		client = addr
		if !c.trusts(addr) {
			return addr
		}
	}

	return client
}

// parseForwardedAddr разбирает адрес из RemoteAddr или заголовка прокси:
// "1.2.3.4", "1.2.3.4:5678", "2001:db8::1", "[2001:db8::1]:443". IPv4 в
// IPv6-записи приводится к IPv4, зона IPv6 отбрасывается.
func parseForwardedAddr(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, false
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		host, _, splitErr := net.SplitHostPort(value)
		if splitErr != nil {
			return netip.Addr{}, false
		}

		if addr, err = netip.ParseAddr(host); err != nil {
			return netip.Addr{}, false
		}
	}

	return addr.Unmap().WithZone(""), true
}

// remoteHost - хост из RemoteAddr без порта (или RemoteAddr как есть).
func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}

	return host
}

// resolveClientIP кладет IP клиента в контекст запроса: его читают лимиты, лог
// запросов и аудит admin. r.RemoteAddr не подменяется - в логе
// остается и адрес соединения (прокси).
func (s *Server) resolveClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIPs.resolve(r)

		next.ServeHTTP(w, r.WithContext(core.WithClientIP(r.Context(), ip)))
	})
}

// clientIP - IP клиента запроса. Вне resolveClientIP (middleware в тестах
// вызывается напрямую) - адрес соединения: заголовкам без проверки прокси не
// доверяем.
func clientIP(r *http.Request) string {
	if ip, ok := core.ClientIPFrom(r.Context()); ok {
		return ip
	}

	return remoteHost(r.RemoteAddr)
}
