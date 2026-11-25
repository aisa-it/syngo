package transport_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// loopbackPolicy - разрешен только 127.0.0.1 (httptest), остальной loopback и
// частные сети закрыты по умолчанию: так проверяется именно блок-лист.
func loopbackPolicy(t *testing.T) *transport.AddressPolicy {
	t.Helper()

	policy, err := transport.NewAddressPolicy(nil, []string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	return policy
}

// serverOn поднимает httptest на заданном IP (127.0.0.2 - "внутренний" адрес).
func serverOn(t *testing.T, ip string, handler http.Handler) (*httptest.Server, bool) {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return nil, false
	}

	srv := httptest.NewUnstartedServer(handler)
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, true
}

func noSRV(context.Context, string, string, string) ([]*net.SRV, error) {
	return nil, errors.New("no srv")
}

// TestWellKnownToLoopbackRefused - m.server и сам well-known на
// внутреннем адресе - отказ (запрос туда не уходит).
func TestWellKnownToLoopbackRefused(t *testing.T) {
	t.Parallel()

	var internalHits atomic.Int32

	internal, ok := serverOn(t, "127.0.0.2", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = w.Write([]byte(`{"m.server":"x.test:8448"}`))
	}))
	if !ok {
		t.Skip("127.0.0.2 недоступен")
	}

	internalAddr := strings.TrimPrefix(internal.URL, "http://")

	wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"m.server": internalAddr})
	}))
	defer wk.Close()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.LookupSRV = noSRV
	resolver.LookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "deleg.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "evil.test":
			return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
		}

		return nil, errors.New("nxdomain")
	}
	resolver.WellKnownURL = func(scheme, hostname string) string {
		if hostname == "evil.test" {
			return scheme + "://evil.test:" + internal.URL[strings.LastIndex(internal.URL, ":")+1:] + "/.well-known/matrix/server"
		}

		return wk.URL + "/.well-known/matrix/server"
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", AddressPolicy: loopbackPolicy(t),
	})

	// m.server указывает на 127.0.0.2: делегирование принято, но подключение - отказ.
	if _, _, err := client.Send(context.Background(), "deleg.test", http.MethodGet, "/_matrix/federation/v1/version", nil); !errors.Is(err, transport.ErrBlockedAddress) {
		t.Fatalf("запрос на внутренний m.server: %v, ожидали ErrBlockedAddress", err)
	}

	// well-known самого сервера на внутреннем адресе: не запрашивается.
	target, err := resolver.Resolve(context.Background(), "evil.test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if target.Host != "evil.test:8448" {
		t.Fatalf("well-known с внутреннего адреса не должен приниматься: %+v", target)
	}

	if internalHits.Load() != 0 {
		t.Fatalf("на внутренний адрес ушло %d запросов", internalHits.Load())
	}
}

// TestWellKnownRedirectToPrivateRefused - редирект well-known на
// 10.0.0.0/8 (и другой внутренний адрес) не выполняется.
func TestWellKnownRedirectToPrivateRefused(t *testing.T) {
	t.Parallel()

	var internalHits atomic.Int32

	internal, ok := serverOn(t, "127.0.0.2", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalHits.Add(1)
		_, _ = w.Write([]byte(`{"m.server":"internal.test:8448"}`))
	}))
	if !ok {
		t.Skip("127.0.0.2 недоступен")
	}

	for _, location := range []string{
		"http://10.20.30.40:8448/.well-known/matrix/server",
		internal.URL + "/.well-known/matrix/server",
	} {
		wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusFound)
		}))

		resolver := transport.NewResolver()
		resolver.Scheme = "http"
		resolver.Policy = loopbackPolicy(t)
		resolver.LookupSRV = noSRV
		resolver.WellKnownURL = func(string, string) string { return wk.URL + "/.well-known/matrix/server" }

		target, err := resolver.Resolve(context.Background(), "deleg.test")
		wk.Close()

		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		if target.Host != "deleg.test:8448" {
			t.Fatalf("редирект на %s выполнен: %+v", location, target)
		}
	}

	if internalHits.Load() != 0 {
		t.Fatalf("на внутренний адрес ушло %d запросов", internalHits.Load())
	}
}

// TestWellKnownRedirectFollowed - редирект well-known на разрешенный адрес
// выполняется (спека: следовать редиректам).
func TestWellKnownRedirectFollowed(t *testing.T) {
	t.Parallel()

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"m.server":"target.test:8448"}`))
	}))
	defer final.Close()

	wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/.well-known/matrix/server", http.StatusMovedPermanently)
	}))
	defer wk.Close()

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.Policy = loopbackPolicy(t)
	resolver.LookupSRV = noSRV
	resolver.WellKnownURL = func(string, string) string { return wk.URL + "/.well-known/matrix/server" }

	target, err := resolver.Resolve(context.Background(), "deleg.test")
	if err != nil || target.Host != "target.test:8448" {
		t.Fatalf("target = %+v, err = %v", target, err)
	}
}

// TestWellKnownMServerWithPathRefused - m.server с путем и query не
// принимается (раньше уходил запрос на 127.0.0.1:9200/_cluster/settings?x=...).
func TestWellKnownMServerWithPathRefused(t *testing.T) {
	t.Parallel()

	for _, server := range []string{
		"127.0.0.1:9200/_cluster/settings?x=", "target.test:8448/path", "user@target.test", "target.test:8448#x",
	} {
		wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"m.server": server})
		}))

		resolver := transport.NewResolver()
		resolver.HTTPClient = wk.Client()
		resolver.Scheme = "http"
		resolver.LookupSRV = noSRV
		resolver.WellKnownURL = func(string, string) string { return wk.URL + "/.well-known/matrix/server" }

		target, err := resolver.Resolve(context.Background(), "deleg.test")
		wk.Close()

		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		if target.Host != "deleg.test:8448" {
			t.Fatalf("m.server %q принят: %+v", server, target)
		}
	}
}

// TestClientDoesNotFollowRedirects - федеративный запрос не следует 30x.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var otherHits atomic.Int32

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer remote.Close()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	client := transport.NewClient(transport.Options{
		Key: &key, Origin: "ours.test", Scheme: "http", AddressPolicy: transport.AllowAllAddresses(),
	})

	_, status, _ := client.Send(context.Background(), strings.TrimPrefix(remote.URL, "http://"), http.MethodGet, "/_matrix/federation/v1/version", nil)
	if status != http.StatusTemporaryRedirect || otherHits.Load() != 0 {
		t.Fatalf("status = %d, запросов по редиректу %d", status, otherHits.Load())
	}
}

// TestClientDefaultPolicyBlocksLoopback - политика по умолчанию закрывает loopback.
func TestClientDefaultPolicyBlocksLoopback(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer remote.Close()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	client := transport.NewClient(transport.Options{Key: &key, Origin: "ours.test", Scheme: "http"})

	_, _, err = client.Send(context.Background(), strings.TrimPrefix(remote.URL, "http://"), http.MethodGet, "/x", nil)
	if !errors.Is(err, transport.ErrBlockedAddress) || hits.Load() != 0 {
		t.Fatalf("err = %v, hits = %d", err, hits.Load())
	}
}

// TestAddressPolicy - whitelist сильнее blacklist, unspecified закрыт всегда.
func TestAddressPolicy(t *testing.T) {
	t.Parallel()

	policy, err := transport.NewAddressPolicy(nil, []string{"10.1.0.0/16", "fd00::1"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	cases := map[string]bool{
		"8.8.8.8": true, "10.0.0.1": false, "10.1.2.3": true, "127.0.0.1": false,
		"::ffff:127.0.0.1": false, "::1": false, "fd00::1": true, "fd00::2": false,
		"0.0.0.0": false, "::": false, "2001:4860::1": true, "169.254.169.254": false,
	}

	for raw, want := range cases {
		if got := policy.Allowed(netip.MustParseAddr(raw)); got != want {
			t.Errorf("Allowed(%s) = %v, ожидали %v", raw, got, want)
		}
	}

	empty, err := transport.NewAddressPolicy([]string{}, nil)
	if err != nil || !empty.Allowed(netip.MustParseAddr("10.0.0.1")) || empty.Allowed(netip.MustParseAddr("0.0.0.0")) {
		t.Fatalf("пустой blacklist: %v", err)
	}

	if _, err := transport.NewAddressPolicy([]string{"bad"}, nil); err == nil {
		t.Fatal("невалидный диапазон должен отвергаться")
	}
}

// tlsRemote - TLS-сервер httptest (сертификат на example.com, 127.0.0.1, ::1) и
// пул с его CA.
func tlsRemote(t *testing.T, gotHost *atomic.Value) (port uint16, pool *x509.CertPool) {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	pool = x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	_, rawPort, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	parsed, _ := strconv.Atoi(rawPort)

	return uint16(parsed), pool //nolint:gosec // порт httptest
}

// TestClientTLSNameIsServerNameForSRV - при SRV сертификат проверяется
// на server_name, а не на имя цели SRV (подмена DNS не дает MITM).
func TestClientTLSNameIsServerNameForSRV(t *testing.T) {
	t.Parallel()

	var gotHost atomic.Value

	port, pool := tlsRemote(t, &gotHost)

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	newClient := func(srvTarget string) *transport.Client {
		resolver := transport.NewResolver()
		resolver.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no well-known")
		})}
		resolver.LookupSRV = func(_ context.Context, service, _, _ string) ([]*net.SRV, error) {
			if service != "matrix-fed" {
				return nil, errors.New("no srv")
			}

			return []*net.SRV{{Target: srvTarget + ".", Port: port}}, nil
		}
		resolver.LookupIP = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}

		return transport.NewClient(transport.Options{
			Resolver: resolver, Key: &key, Origin: "ours.test", RootCAs: pool,
			AddressPolicy: transport.AllowAllAddresses(),
		})
	}

	// Сертификат на server_name (example.com), цель SRV - другое имя: принимается.
	body, status, err := newClient("target.test").Send(context.Background(), "example.com", http.MethodGet, "/_matrix/federation/v1/version", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("сертификат на server_name: %d %s %v", status, body, err)
	}

	if host, _ := gotHost.Load().(string); host != "example.com" {
		t.Fatalf("Host = %q, ожидали server_name", host)
	}

	// Сертификат на имя цели SRV (example.com), server_name другой: отказ.
	if _, _, err := newClient("example.com").Send(context.Background(), "other.test", http.MethodGet, "/_matrix/federation/v1/version", nil); err == nil {
		t.Fatal("сертификат на имя цели SRV не должен приниматься для другого server_name")
	}
}

// TestClientServerNameWithPortAndIPLiteral - server_name с портом и
// IP-литерал резолвятся без well-known/SRV, сертификат - на имя или IP.
func TestClientServerNameWithPortAndIPLiteral(t *testing.T) {
	t.Parallel()

	var gotHost atomic.Value

	port, pool := tlsRemote(t, &gotHost)

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	var wellKnownHits atomic.Int32

	resolver := transport.NewResolver()
	resolver.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		wellKnownHits.Add(1)

		return nil, errors.New("no well-known")
	})}
	resolver.LookupSRV = noSRV
	resolver.LookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", RootCAs: pool,
		AddressPolicy: transport.AllowAllAddresses(),
	})

	portStr := strconv.Itoa(int(port))

	for _, name := range []string{"example.com:" + portStr, "127.0.0.1:" + portStr} {
		if _, status, err := client.Send(context.Background(), name, http.MethodGet, "/_matrix/federation/v1/version", nil); err != nil || status != http.StatusOK {
			t.Fatalf("%s: %d %v", name, status, err)
		}

		if host, _ := gotHost.Load().(string); host != name {
			t.Fatalf("Host = %q, ожидали %q", host, name)
		}
	}

	if wellKnownHits.Load() != 0 {
		t.Fatalf("для server_name с портом/IP запрошен well-known (%d)", wellKnownHits.Load())
	}

	// Сертификат не содержит other.test: имя с портом проверяется по имени.
	if _, _, err := client.Send(context.Background(), "other.test:"+portStr, http.MethodGet, "/x", nil); err == nil {
		t.Fatal("сертификат без server_name должен отвергаться")
	}
}
