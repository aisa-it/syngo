package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress - адрес назначения запрещен ip_range_blacklist (SSRF).
var ErrBlockedAddress = errors.New("transport: destination address is blocked")

// DefaultIPRangeBlacklist - диапазоны, закрытые для исходящих федеративных запросов
// по умолчанию: как ip_range_blacklist Synapse (частные, loopback, link-local,
// CGNAT, документационные, multicast), плюс IPv6-туннели к IPv4 (NAT64, 6to4,
// Teredo), через которые доступны внутренние IPv4.
var DefaultIPRangeBlacklist = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
}

// AddressPolicy - какие IP допустимы для исходящих федеративных запросов.
//
// Whitelist сильнее blacklist (как ip_range_whitelist Synapse); 0.0.0.0 и ::
// запрещены всегда. nil-политика разрешает все, кроме них (только тесты).
type AddressPolicy struct {
	blacklist []netip.Prefix
	whitelist []netip.Prefix
}

// NewAddressPolicy собирает политику из CIDR-строк. blacklist == nil - диапазоны
// по умолчанию (DefaultIPRangeBlacklist); пустой непустой срез - без запретов.
func NewAddressPolicy(blacklist, whitelist []string) (*AddressPolicy, error) {
	if blacklist == nil {
		blacklist = DefaultIPRangeBlacklist
	}

	black, err := ParseIPRanges(blacklist)
	if err != nil {
		return nil, fmt.Errorf("ip_range_blacklist: %w", err)
	}

	white, err := ParseIPRanges(whitelist)
	if err != nil {
		return nil, fmt.Errorf("ip_range_whitelist: %w", err)
	}

	return &AddressPolicy{blacklist: black, whitelist: white}, nil
}

// DefaultAddressPolicy - политика по умолчанию (DefaultIPRangeBlacklist без исключений).
func DefaultAddressPolicy() *AddressPolicy {
	policy, err := NewAddressPolicy(nil, nil)
	if err != nil {
		panic(err) // список по умолчанию задан в коде и валиден
	}

	return policy
}

// AllowAllAddresses - политика без запретов (тесты и httptest на 127.0.0.1).
func AllowAllAddresses() *AddressPolicy { return &AddressPolicy{} }

// ParseIPRanges разбирает CIDR или одиночные IP.
func ParseIPRanges(raw []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(raw))

	for _, item := range raw {
		if prefix, err := netip.ParsePrefix(item); err == nil {
			out = append(out, prefix.Masked())

			continue
		}

		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("bad range %q", item)
		}

		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}

	return out, nil
}

// Allowed - можно ли подключаться к адресу.
func (p *AddressPolicy) Allowed(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}

	addr = addr.WithZone("").Unmap()

	if addr.IsUnspecified() {
		return false
	}

	if p == nil {
		return true
	}

	for _, prefix := range p.whitelist {
		if prefix.Contains(addr) {
			return true
		}
	}

	for _, prefix := range p.blacklist {
		if prefix.Contains(addr) {
			return false
		}
	}

	return true
}

// Dialer подключается только к разрешенным адресам: имя резолвится один раз,
// запрещенные адреса отбрасываются (как BlocklistingReactorWrapper Synapse),
// подключение идет к проверенному IP; Control повторно проверяет адрес сокета
// перед connect (DNS rebinding не обходит проверку).
type Dialer struct {
	Policy *AddressPolicy
	// LookupIP резолвит имя (в тестах подменяется).
	LookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
	Timeout  time.Duration
}

// DialContext - net.Dialer.DialContext с проверкой адреса.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	var addrs []netip.Addr

	if literal, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{literal}
	} else {
		lookup := d.LookupIP
		if lookup == nil {
			lookup = defaultLookupIP
		}

		addrs, err = lookup(ctx, host)
		if err != nil {
			return nil, err
		}
	}

	allowed := make([]netip.Addr, 0, len(addrs))

	for _, addr := range addrs {
		if d.Policy.Allowed(addr) {
			allowed = append(allowed, addr.WithZone("").Unmap())
		}
	}

	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, host)
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !d.Policy.Allowed(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
			}

			return nil
		},
	}

	var lastErr error

	for _, addr := range allowed {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}

		lastErr = err
	}

	return nil, lastErr
}

// defaultLookupIP - системный резолвер.
func defaultLookupIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
