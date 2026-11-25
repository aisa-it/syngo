package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

// TestClientIPResolver - разбор X-Forwarded-For и X-Real-IP.
func TestClientIPResolver(t *testing.T) {
	t.Parallel()

	trusted, err := config.Server{TrustedProxies: []string{"10.0.0.0/8", "fd00::/8", "192.0.2.1"}}.TrustedProxyPrefixes()
	if err != nil {
		t.Fatalf("prefixes: %v", err)
	}

	resolver := clientIPResolver{trusted: trusted}

	cases := []struct {
		name   string
		remote string
		xff    []string
		realIP string
		want   string
	}{
		{name: "без прокси", remote: "203.0.113.10:1000", want: "203.0.113.10"},
		{name: "недоверенный с XFF", remote: "203.0.113.10:1000", xff: []string{"198.51.100.1"}, want: "203.0.113.10"},
		{name: "недоверенный с X-Real-IP", remote: "203.0.113.10:1000", realIP: "198.51.100.1", want: "203.0.113.10"},
		{name: "доверенный без заголовков", remote: "10.0.0.5:1000", want: "10.0.0.5"},
		{name: "доверенный, XFF", remote: "10.0.0.5:1000", xff: []string{"203.0.113.10"}, want: "203.0.113.10"},
		{name: "цепочка прокси", remote: "10.0.0.5:1000", xff: []string{"198.51.100.1, 203.0.113.10, 10.1.1.1"}, want: "203.0.113.10"},
		{name: "несколько заголовков", remote: "10.0.0.5:1000", xff: []string{"198.51.100.1", "203.0.113.10"}, want: "203.0.113.10"},
		{name: "XFF важнее X-Real-IP", remote: "10.0.0.5:1000", xff: []string{"203.0.113.10"}, realIP: "198.51.100.1", want: "203.0.113.10"},
		{name: "X-Real-IP", remote: "10.0.0.5:1000", realIP: "203.0.113.10", want: "203.0.113.10"},
		{name: "мусорный X-Real-IP", remote: "10.0.0.5:1000", realIP: "not-an-ip", want: "10.0.0.5"},
		{name: "все звенья доверенные", remote: "10.0.0.5:1000", xff: []string{"10.2.2.2, 10.1.1.1"}, want: "10.2.2.2"},
		{name: "мусор в цепочке", remote: "10.0.0.5:1000", xff: []string{"203.0.113.10, garbage, 10.1.1.1"}, want: "10.1.1.1"},
		{name: "пустые звенья", remote: "10.0.0.5:1000", xff: []string{" , 203.0.113.10 ,"}, want: "203.0.113.10"},
		{name: "XFF с портом", remote: "10.0.0.5:1000", xff: []string{"203.0.113.10:5555"}, want: "203.0.113.10"},
		{name: "IPv6 в скобках с портом", remote: "10.0.0.5:1000", xff: []string{"[2001:db8::1]:443"}, want: "2001:db8::1"},
		{name: "IPv6-прокси", remote: "[fd00::5]:1000", xff: []string{"2001:db8::1"}, want: "2001:db8::1"},
		{name: "IPv4 в IPv6-записи", remote: "[::ffff:10.0.0.5]:1000", xff: []string{"::ffff:203.0.113.10"}, want: "203.0.113.10"},
		{name: "одиночный IP прокси", remote: "192.0.2.1:1000", xff: []string{"203.0.113.10"}, want: "203.0.113.10"},
		{name: "сосед одиночного IP", remote: "192.0.2.2:1000", xff: []string{"203.0.113.10"}, want: "192.0.2.2"},
		{name: "RemoteAddr не IP", remote: "@unix", xff: []string{"203.0.113.10"}, want: "@unix"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req.RemoteAddr = tc.remote

			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}

			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}

			if got := resolver.resolve(req); got != tc.want {
				t.Fatalf("resolve = %q, ожидали %q", got, tc.want)
			}
		})
	}
}
