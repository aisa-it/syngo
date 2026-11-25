package transport_test

import (
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/transport"
)

// TestParseServerName - грамматика server_name спеки.
func TestParseServerName(t *testing.T) {
	t.Parallel()

	valid := map[string]transport.ServerName{
		"example.com":       {Host: "example.com"},
		"example.com:8448":  {Host: "example.com", Port: 8448},
		"1.2.3.4":           {Host: "1.2.3.4", IP: true},
		"1.2.3.4:1234":      {Host: "1.2.3.4", Port: 1234, IP: true},
		"[1234:5678::abcd]": {Host: "1234:5678::abcd", IP: true},
		"[::1]:8448":        {Host: "::1", Port: 8448, IP: true},
		"matrix-1.org":      {Host: "matrix-1.org"},
	}

	for raw, want := range valid {
		got, err := transport.ParseServerName(raw)
		if err != nil {
			t.Errorf("ParseServerName(%q): %v", raw, err)

			continue
		}

		if got != want {
			t.Errorf("ParseServerName(%q) = %+v, ожидали %+v", raw, got, want)
		}
	}

	invalid := []string{
		"", "127.0.0.1:9200/_cluster/settings?x=", "host/path", "host?q", "user@host",
		"https://host", "host:", "host:0", "host:65536", "host:123456", "host:80:81",
		"[::1", "[::1]x", "[1.2.3.4]", "1.2.3", "01.2.3.4", "2130706433", "host name",
		"host_name", "[fe80::1%eth0]",
	}

	for _, raw := range invalid {
		if _, err := transport.ParseServerName(raw); !errors.Is(err, transport.ErrInvalidServerName) {
			t.Errorf("ParseServerName(%q) = %v, ожидали ErrInvalidServerName", raw, err)
		}
	}
}
