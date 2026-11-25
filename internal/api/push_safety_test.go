package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testNotifyPath = "/_matrix/push/v1/notify"

// SSRF-защита доставки push: loopback/private/link-local запрещены,
// http допустим только на стенде (push.allow_http).
func TestPushURLSafety(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"http://127.0.0.1" + testNotifyPath,
		"https://127.0.0.1" + testNotifyPath,
		"https://10.0.0.5" + testNotifyPath,
		"https://192.168.1.10" + testNotifyPath,
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]" + testNotifyPath,
		"ftp://example.com" + testNotifyPath,
	}
	for _, raw := range blocked {
		if err := validatePushURL(raw, true); err == nil {
			t.Fatalf("%s: ожидали отказ", raw)
		}
	}

	if err := validatePushURL("https://push.example.com"+testNotifyPath, false); err != nil {
		t.Fatalf("публичный https должен проходить: %v", err)
	}

	if err := validatePushURL("http://gateway.test"+testNotifyPath, false); err == nil {
		t.Fatal("http без allow_http должен отвергаться")
	}

	if err := validatePushURL("http://gateway.test"+testNotifyPath, true); err != nil {
		t.Fatalf("http при allow_http должен проходить: %v", err)
	}
}

// TestReviewPushIPs - адреса вне блок-листа, нестандартные записи IP, путь и
// userinfo отвергаются.
func TestReviewPushIPs(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://[::ffff:127.0.0.1]/x", "https://0.0.0.1/x", "https://198.18.0.1/x", "https://192.0.0.8/x",
		"https://240.0.0.1/x", "https://[64:ff9b::7f00:1]/x", "https://224.0.0.251/x", "https://255.255.255.255/x",
		"https://[fec0::1]/x", "https://2130706433/x", "https://0x7f.1/x", "https://example.com/any/path?x=1",
		"https://user:pw@example.com:22/",
	} {
		if err := validatePushURL(raw, false); err == nil {
			t.Errorf("%s: ожидали отказ", raw)
		}

		// Тот же хост на правильном пути: отказ из-за адреса, а не пути.
		parsed, _ := url.Parse(raw)
		parsed.Path, parsed.RawQuery = testNotifyPath, ""

		if parsed.Host == "example.com" {
			continue
		}

		if err := validatePushURL(parsed.String(), false); err == nil {
			t.Errorf("%s: ожидали отказ по адресу", parsed)
		}
	}
}

// TestIsUnsafePushIP — отдельная проверка классификатора адресов (NAT64, 6to4,
// Teredo, benchmark, reserved, site-local, multicast).
func TestIsUnsafePushIP(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.5.5", "192.168.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"0.0.0.1", "198.18.0.1", "198.19.255.1", "192.0.0.8", "240.0.0.1", "255.255.255.255", "224.0.0.251",
		"64:ff9b::7f00:1", "64:ff9b:1::1", "2002:7f00:1::1", "2001::1", "fec0::1", "fc00::1", "fe80::1",
		"ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "2001:db8::1",
	} {
		if !isUnsafePushIP(net.ParseIP(raw)) {
			t.Errorf("%s должен считаться небезопасным", raw)
		}
	}

	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "2a00:1450:4001::1"} {
		if isUnsafePushIP(net.ParseIP(raw)) {
			t.Errorf("%s - публичный адрес", raw)
		}
	}
}

// fakeResolver - подставной резолвер: имя -> адреса.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}

	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// pushTestServer - сервер с подставным резолвером и без внешней сети.
func pushTestServer(t *testing.T, resolver pushResolver, unsafe func(netip.Addr) bool) *Server {
	t.Helper()

	srv := newTestServer(t)
	srv.cfg.Push.Timeout = 0
	srv.pushResolver = resolver
	srv.pushUnsafeAddr = unsafe

	return srv
}

// loopbackAllowed - в тестах 127.0.0.1 изображает публичный адрес шлюза,
// остальное классифицируется как в проде.
func loopbackAllowed(addr netip.Addr) bool {
	if addr == netip.MustParseAddr("127.0.0.1") {
		return false
	}

	return isUnsafePushAddr(addr)
}

func gatewayURL(t *testing.T, srv *httptest.Server, host string) string {
	t.Helper()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	return "http://" + net.JoinHostPort(host, port) + testNotifyPath
}

// TestPushDialChecksResolvedIP - имя, которое резолвится во
// внутренний адрес (DNS rebinding), отвергается на этапе dial; проверка не
// зависит от allow_http.
func TestPushDialChecksResolvedIP(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(gateway.Close)

	resolver := fakeResolver{
		"rebind.example":  {netip.MustParseAddr("127.0.0.1")},
		"mixed.example":   {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("10.0.0.1")},
		"gateway.example": {netip.MustParseAddr("127.0.0.1")},
	}

	// Прод-классификатор: 127.0.0.1 запрещен.
	srv := pushTestServer(t, resolver, nil)
	srv.cfg.Push.AllowHTTP = true // раньше allow_http выключал защитный dialer

	client := srv.newPushClient()

	for _, host := range []string{"rebind.example", "127.0.0.1"} {
		resp, err := client.Post(gatewayURL(t, gateway, host), "application/json", strings.NewReader(`{}`))
		if err == nil {
			_ = resp.Body.Close()

			t.Fatalf("%s: подключение к 127.0.0.1 разрешено", host)
		}
	}

	// Классификатор, где 127.0.0.1 - «публичный»: имя проходит, а смешанный
	// ответ DNS с внутренним адресом - нет.
	srv = pushTestServer(t, resolver, loopbackAllowed)
	srv.cfg.Push.AllowHTTP = true
	client = srv.newPushClient()

	resp, err := client.Post(gatewayURL(t, gateway, "gateway.example"), "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("разрешенный адрес: %v", err)
	}

	_ = resp.Body.Close()

	if resp, err := client.Post(gatewayURL(t, gateway, "mixed.example"), "application/json", strings.NewReader(`{}`)); err == nil {
		_ = resp.Body.Close()

		t.Fatal("ответ DNS с внутренним адресом принят")
	}

	if hits.Load() != 1 {
		t.Fatalf("шлюз получил %d запросов, ожидали 1", hits.Load())
	}
}

// TestPushClientNoRedirectNoProxy - Proxy: nil, редиректы не
// выполняются (редирект на внутренний адрес не обходит проверку).
func TestPushClientNoRedirectNoProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")

	var internalHits atomic.Int32

	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { internalHits.Add(1) }))
	t.Cleanup(internal.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	srv := pushTestServer(t, fakeResolver{"gateway.example": {netip.MustParseAddr("127.0.0.1")}}, loopbackAllowed)
	srv.cfg.Push.AllowHTTP = true
	client := srv.newPushClient()

	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("транспорт push должен быть с Proxy: nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL(t, redirect, "gateway.example"), strings.NewReader(`{}`))

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("запрос через прокси или ошибка: %v", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect || internalHits.Load() != 0 {
		t.Fatalf("редирект выполнен: status %d, внутренний сервер %d", resp.StatusCode, internalHits.Load())
	}
}
