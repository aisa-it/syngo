package transport

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// ErrInvalidServerName - строка не соответствует грамматике server_name.
var ErrInvalidServerName = errors.New("transport: invalid server name")

// maxServerNameLength - предел длины server_name (спека: dns-name до 255 символов,
// плюс порт).
const maxServerNameLength = 255 + len(":65535")

// ServerName - разобранный server_name по грамматике спеки (Appendices, Server Name):
//
//	server_name = hostname [ ":" port ]
//	port        = 1*5DIGIT
//	hostname    = IPv4address / "[" IPv6address "]" / dns-name
//	dns-name    = 1*255dns-char ; DIGIT / ALPHA / "-" / "."
type ServerName struct {
	// Host - имя или IP без скобок и порта.
	Host string
	// Port - явный порт (0 - не задан).
	Port int
	// IP - Host является IP-литералом.
	IP bool
}

// ParseServerName разбирает server_name. Путь, query, userinfo, схема, пробелы и
// прочее, что не входит в грамматику, отвергаются: m.server из well-known и origin
// из X-Matrix приходят от чужого сервера.
func ParseServerName(raw string) (ServerName, error) {
	if raw == "" || len(raw) > maxServerNameLength {
		return ServerName{}, fmt.Errorf("%w: %q", ErrInvalidServerName, raw)
	}

	host, portPart, hasPort, err := splitServerName(raw)
	if err != nil {
		return ServerName{}, err
	}

	parsed := ServerName{Host: host}

	if hasPort {
		port, err := parsePort(portPart)
		if err != nil {
			return ServerName{}, fmt.Errorf("%w: %q: %w", ErrInvalidServerName, raw, err)
		}

		parsed.Port = port
	}

	if strings.HasPrefix(raw, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return ServerName{}, fmt.Errorf("%w: bad IPv6 literal %q", ErrInvalidServerName, raw)
		}

		parsed.IP = true

		return parsed, nil
	}

	if looksLikeIPv4(host) {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is4() {
			return ServerName{}, fmt.Errorf("%w: bad IPv4 literal %q", ErrInvalidServerName, raw)
		}

		parsed.IP = true

		return parsed, nil
	}

	if !validDNSName(host) {
		return ServerName{}, fmt.Errorf("%w: %q", ErrInvalidServerName, raw)
	}

	return parsed, nil
}

// HostPort - адрес host:port для подключения (порт по умолчанию - defaultPort).
func (n ServerName) HostPort(defaultPort int) string {
	port := n.Port
	if port == 0 {
		port = defaultPort
	}

	if n.IP && strings.Contains(n.Host, ":") {
		return "[" + n.Host + "]:" + strconv.Itoa(port)
	}

	return n.Host + ":" + strconv.Itoa(port)
}

// splitServerName отделяет hostname от порта с учетом скобок IPv6.
func splitServerName(raw string) (host, port string, hasPort bool, err error) {
	if strings.HasPrefix(raw, "[") {
		end := strings.IndexByte(raw, ']')
		if end < 0 {
			return "", "", false, fmt.Errorf("%w: unclosed IPv6 literal %q", ErrInvalidServerName, raw)
		}

		host = raw[1:end]

		rest := raw[end+1:]
		if rest == "" {
			return host, "", false, nil
		}

		if !strings.HasPrefix(rest, ":") {
			return "", "", false, fmt.Errorf("%w: %q", ErrInvalidServerName, raw)
		}

		return host, rest[1:], true, nil
	}

	host, port, hasPort = strings.Cut(raw, ":")

	if hasPort && strings.Contains(port, ":") {
		return "", "", false, fmt.Errorf("%w: %q", ErrInvalidServerName, raw)
	}

	return host, port, hasPort, nil
}

// parsePort - 1*5DIGIT в диапазоне 1..65535.
func parsePort(raw string) (int, error) {
	if raw == "" || len(raw) > 5 {
		return 0, errors.New("bad port")
	}

	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, errors.New("bad port")
		}
	}

	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("bad port")
	}

	return port, nil
}

// looksLikeIPv4 - строка из цифр и точек: по грамматике это IPv4address, и
// нестрогие формы ("1.2.3", "01.2.3.4") не должны пройти как dns-name.
func looksLikeIPv4(host string) bool {
	if host == "" {
		return false
	}

	for _, c := range host {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}

	return true
}

// validDNSName - dns-name спеки: 1..255 символов из букв, цифр, "-" и ".".
func validDNSName(host string) bool {
	if host == "" || len(host) > 255 {
		return false
	}

	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}

	return true
}
