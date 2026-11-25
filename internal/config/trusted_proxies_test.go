package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

// server.trusted_proxies - из YAML и SYNGOD_TRUSTED_PROXIES, понятные ошибки.

func TestTrustedProxiesFromFileAndEnv(t *testing.T) {
	t.Setenv(config.EnvName, "dev")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "matrix:\n  server_name: test.local\nserver:\n  trusted_proxies: [\"10.0.0.0/8\", \"192.0.2.1\"]\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	prefixes, err := cfg.Server.TrustedProxyPrefixes()
	if err != nil {
		t.Fatalf("prefixes: %v", err)
	}

	if len(prefixes) != 2 || prefixes[0].String() != "10.0.0.0/8" || prefixes[1].String() != "192.0.2.1/32" {
		t.Fatalf("prefixes из файла = %v", prefixes)
	}

	// Переменная окружения заменяет список из файла.
	t.Setenv(config.EnvTrustedProxies, "172.18.0.1, fd00::/8")

	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("load с env: %v", err)
	}

	if got := strings.Join(cfg.Server.TrustedProxies, ","); got != "172.18.0.1,fd00::/8" {
		t.Fatalf("trusted_proxies из env = %q", got)
	}
}

func TestTrustedProxiesValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value string
		want  string
	}{
		{"10.0.0.0/33", "ожидается CIDR"},
		{"nginx", "ожидается CIDR"},
		{"", "ожидается CIDR"},
		{"0.0.0.0/0", "сеть /0 запрещена"},
		{"::/0", "сеть /0 запрещена"},
	} {
		cfg := validProd()
		cfg.Server.TrustedProxies = []string{"10.0.0.0/8", tc.value}

		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), "server.trusted_proxies[1]") {
			t.Fatalf("trusted_proxies %q: ошибка %v, ожидали %q", tc.value, err, tc.want)
		}
	}
}

func TestTrustedProxiesNormalized(t *testing.T) {
	t.Parallel()

	prefixes, err := config.Server{TrustedProxies: []string{
		" 10.1.2.3/8 ", "::ffff:172.16.0.0/108", "::ffff:192.0.2.9", "2001:db8::1",
	}}.TrustedProxyPrefixes()
	if err != nil {
		t.Fatalf("prefixes: %v", err)
	}

	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.0.2.9/32", "2001:db8::1/128"}
	for i, prefix := range prefixes {
		if prefix.String() != want[i] {
			t.Fatalf("prefix[%d] = %s, ожидали %s", i, prefix, want[i])
		}
	}
}
