package transport_test

import (
	"context"
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
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/store"
)

// fakeStore — состояние destination в памяти.
type fakeStore struct {
	states map[string]store.DestinationState
}

func newFakeStore() *fakeStore { return &fakeStore{states: map[string]store.DestinationState{}} }

func (f *fakeStore) Destination(_ context.Context, name string) (store.DestinationState, error) {
	state, ok := f.states[name]
	if !ok {
		return store.DestinationState{Destination: name}, nil
	}

	return state, nil
}

func (f *fakeStore) SaveDestinationFailure(_ context.Context, name string, at time.Time, interval time.Duration) error {
	f.states[name] = store.DestinationState{Destination: name, RetryLastTS: &at, RetryInterval: interval}

	return nil
}

func (f *fakeStore) SaveDestinationSuccess(_ context.Context, name string) error {
	delete(f.states, name)

	return nil
}

// TestResolverWellKnown — резолв через well-known с кэшем TTL.
func TestResolverWellKnown(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"m.server": "target.test:8448"})
	}))
	defer wk.Close()

	resolver := transport.NewResolver()
	resolver.HTTPClient = wk.Client()
	resolver.Scheme = "http"
	// well-known запрашивается у имени без порта: адрес подменяем на httptest.
	resolver.WellKnownURL = func(string, string) string { return wk.URL + "/.well-known/matrix/server" }

	serverName := "deleg.test"

	target, err := resolver.Resolve(context.Background(), serverName)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Делегирование с портом: сертификат и Host - delegated_hostname (спека, шаг 3.2).
	if target.Host != "target.test:8448" || target.TLSName != "target.test" || target.HostHeader != "target.test:8448" {
		t.Fatalf("target = %+v", target)
	}

	// Второй резолв — из кэша (без нового HTTP-запроса).
	if _, err := resolver.Resolve(context.Background(), serverName); err != nil {
		t.Fatalf("resolve cached: %v", err)
	}

	if hits.Load() != 1 {
		t.Fatalf("well-known запросов %d, ожидали 1 (кэш)", hits.Load())
	}
}

// TestResolverSRVFallback — при отсутствии well-known используется SRV.
func TestResolverSRVFallback(t *testing.T) {
	t.Parallel()

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no well-known")
	})}
	resolver.LookupSRV = func(context.Context, string, string, string) ([]*net.SRV, error) {
		return []*net.SRV{{Target: "matrix.example.org.", Port: 8449, Priority: 10, Weight: 5}}, nil
	}

	target, err := resolver.Resolve(context.Background(), "example.org")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Подключение - к цели SRV, но сертификат и Host - server_name.
	if target.Host != "matrix.example.org:8449" || target.TLSName != "example.org" || target.HostHeader != "example.org" {
		t.Fatalf("target = %+v", target)
	}
}

// TestResolverInsecureTarget — m.server со схемой отвергается (только HTTPS).
func TestResolverInsecureTarget(t *testing.T) {
	t.Parallel()

	wk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"m.server": "http://target.test:8448"})
	}))
	defer wk.Close()

	resolver := transport.NewResolver()
	resolver.HTTPClient = wk.Client()
	resolver.Scheme = "http"
	resolver.WellKnownURL = func(string, string) string { return wk.URL + "/.well-known/matrix/server" }
	resolver.LookupSRV = func(context.Context, string, string, string) ([]*net.SRV, error) {
		return nil, errors.New("no srv")
	}

	target, err := resolver.Resolve(context.Background(), "deleg.test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// m.server со схемой отвергнут: делегирования нет, адрес - само имя на 8448.
	if target.Host != "deleg.test:8448" {
		t.Fatalf("m.server со схемой должен отвергаться, target = %+v", target)
	}
}

// TestClientAllowDenyAndBackoff — политика хостов и рост backoff с потолком.
func TestClientAllowDenyAndBackoff(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	st := newFakeStore()

	now := time.Unix(1_700_000_000, 0)
	current := func() time.Time { return now }
	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no well-known")
	})}
	resolver.LookupSRV = func(context.Context, string, string, string) ([]*net.SRV, error) {
		return nil, errors.New("no srv")
	}
	resolver.LookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("no host")
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", Store: st,
		AllowHosts: []string{"allowed.test"}, DenyHosts: []string{"blocked.allowed.test"},
		MinBackoff: 5 * time.Second, MaxBackoff: 40 * time.Second,
		Jitter: func(d time.Duration) time.Duration { return d },
		Now:    current,
	})

	// Denylist важнее allowlist.
	if _, _, err := client.Send(context.Background(), "blocked.allowed.test", http.MethodGet, "/x", nil); !errors.Is(err, transport.ErrDeniedByPolicy) {
		t.Fatalf("deny: %v", err)
	}

	if _, _, err := client.Send(context.Background(), "other.test", http.MethodGet, "/x", nil); !errors.Is(err, transport.ErrDeniedByPolicy) {
		t.Fatalf("allowlist: %v", err)
	}

	// Backoff растёт: 5 → 10 → 20 → 40 → 40 (потолок).
	expected := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 40 * time.Second}

	for _, want := range expected {
		// Резолв сломан → каждая отправка падает и записывает новый интервал.
		// Перед попыткой сдвигаем время на текущий интервал, иначе сработает backoff-пауза.
		if state, ok := st.states["allowed.test"]; ok && state.RetryInterval > 0 {
			now = now.Add(state.RetryInterval)
		}

		_, _, _ = client.Send(context.Background(), "allowed.test", http.MethodGet, "/x", nil)

		if got := st.states["allowed.test"].RetryInterval; got != want {
			t.Fatalf("retry interval = %s, ожидали %s", got, want)
		}
	}

	// Пока backoff не истёк — отправка не идёт (ErrBackoff).
	if _, _, err := client.Send(context.Background(), "allowed.test", http.MethodGet, "/x", nil); !errors.Is(err, transport.ErrBackoff) {
		t.Fatalf("ожидали ErrBackoff, получили %v", err)
	}

	if client.Snapshot().RetrySkipped == 0 {
		t.Fatal("метрика retry_skipped не увеличилась")
	}
}

// TestClientSendsSignedRequest — интеграция против httptest: подпись X-Matrix валидна.
func TestClientSendsSignedRequest(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	var gotAuth, gotHost string

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer remote.Close()

	host := strings.TrimPrefix(remote.URL, "http://")

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no well-known")
	})}
	// Порт httptest нестандартный: SRV remote.test указывает на него.
	resolver.LookupSRV = func(context.Context, string, string, string) ([]*net.SRV, error) {
		_, port, _ := net.SplitHostPort(host)
		portNum, _ := strconv.Atoi(port)

		return []*net.SRV{{Target: "127.0.0.1.", Port: uint16(portNum)}}, nil //nolint:gosec // порт httptest
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test",
		AddressPolicy: transport.AllowAllAddresses(),
		Jitter:        func(d time.Duration) time.Duration { return d },
	})

	body, status, err := client.Send(context.Background(), "remote.test", http.MethodGet, "/_matrix/federation/v1/version", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("response: %d %s", status, body)
	}

	if !strings.HasPrefix(gotAuth, "X-Matrix ") || !strings.Contains(gotAuth, `destination="remote.test"`) {
		t.Fatalf("X-Matrix: %q", gotAuth)
	}

	if gotHost != "remote.test" {
		t.Fatalf("Host = %q, ожидали server_name", gotHost)
	}
}

// TestIncomingLimits — лимит тела (413) и rate limit до подписи по IP (429).
//
// До проверки подписи origin из X-Matrix не подтвержден, поэтому лимит
// считается по IP: чужой IP с поддельным origin не выбивает 429 настоящему серверу.
func TestIncomingLimits(t *testing.T) {
	t.Parallel()

	incoming := transport.NewIncoming(0.001, 1, 10)

	handler := incoming.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Большое тело — 413.
	req := httptest.NewRequest(http.MethodPost, "/_matrix/federation/v1/send/1", strings.NewReader(strings.Repeat("x", 100)))
	req.Header.Set("Authorization", `X-Matrix origin="remote.test",destination="ours.test",key="ed25519:a_1",sig="x"`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("большое тело: %d, ожидали 413", rec.Code)
	}

	request := func(ip, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/_matrix/federation/v1/send/1", strings.NewReader("{}"))
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("Authorization", `X-Matrix origin="`+origin+`",destination="ours.test",key="ed25519:a_1",sig="x"`)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}

	// Атакующий с поддельным origin исчерпывает лимит своего IP.
	if rec := request("198.51.100.7", "remote.test"); rec.Code != http.StatusOK {
		t.Fatalf("первый запрос: %d", rec.Code)
	}

	rec = request("198.51.100.7", "other.test")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос с того же IP: %d, ожидали 429", rec.Code)
	}

	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("нет Retry-After на 429")
	}

	var body struct {
		Code         string `json:"errcode"`
		RetryAfterMS int64  `json:"retry_after_ms"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "M_LIMIT_EXCEEDED" || body.RetryAfterMS <= 0 {
		t.Fatalf("тело 429 без retry_after_ms: %s", rec.Body.String())
	}

	// Настоящий remote.test с другого IP не задет.
	if rec := request("203.0.113.9", "remote.test"); rec.Code != http.StatusOK {
		t.Fatalf("настоящий сервер получил %d из-за чужого IP", rec.Code)
	}

	// После проверки подписи - лимит по origin.
	if !incoming.AllowOrigin(httptest.NewRecorder(), "remote.test") {
		t.Fatal("первый запрос origin после подписи")
	}

	if incoming.AllowOrigin(httptest.NewRecorder(), "remote.test") {
		t.Fatal("лимит по проверенному origin не сработал")
	}
}

// roundTripFunc — http.RoundTripper для подмены сетевых вызовов.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestClientFallbackTo443 - если 8448 недоступен, клиент пробует 443 и кэширует порт.
func TestClientFallbackTo443(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	var hits443 atomic.Int32

	// 443-сервер: сюда должно переключиться после отказа на 8448.
	server443 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits443.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server443.Close()

	host443 := strings.TrimPrefix(server443.URL, "http://")

	// 8448-порт: гарантированно закрытый (порт 1 на localhost).
	closedHost := "127.0.0.1:1"

	resolver := transport.NewResolver()
	resolver.Scheme = "http"
	resolver.WellKnownTTL = time.Hour
	resolver.TargetsOverride = func(ctx context.Context, _ string) ([]transport.Target, error) {
		_ = ctx

		return []transport.Target{
			{ServerName: "remote.test", Host: closedHost, Scheme: "http"},
			{ServerName: "remote.test", Host: host443, Scheme: "http"},
		}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", AddressPolicy: transport.AllowAllAddresses(),
	})

	body, status, err := client.Send(context.Background(), "remote.test", http.MethodGet, "/_matrix/federation/v1/version", nil)
	if err != nil {
		t.Fatalf("send с fallback: %v", err)
	}

	if status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("response: %d %s", status, body)
	}

	if hits443.Load() != 1 {
		t.Fatalf("443 получил %d запросов, ожидали 1", hits443.Load())
	}

	// Второй запрос идёт сразу на закэшированный рабочий адрес (8448 больше не пробуем).
	resolver.TargetsOverride = func(ctx context.Context, _ string) ([]transport.Target, error) {
		_ = ctx

		return []transport.Target{
			{ServerName: "remote.test", Host: closedHost, Scheme: "http"},
			{ServerName: "remote.test", Host: host443, Scheme: "http"},
		}, nil
	}

	if _, _, err := client.Send(context.Background(), "remote.test", http.MethodGet, "/_matrix/federation/v1/version", nil); err != nil {
		t.Fatalf("second send: %v", err)
	}

	if hits443.Load() != 2 {
		t.Fatalf("после второго запроса 443 получил %d", hits443.Load())
	}
}

// TestClientBackoffOn5xxNotOnTooLarge - 5xx с телом ставит destination
// в backoff, слишком большой ответ - нет (это свойство ответа, а не сервера).
func TestClientBackoffOn5xxNotOnTooLarge(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fail":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"upstream"}`))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN"}`))
		}
	}))
	defer remote.Close()

	host := strings.TrimPrefix(remote.URL, "http://")
	st := newFakeStore()

	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(_ context.Context, name string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: name, Host: host, Scheme: "http"}}, nil
	}

	client := transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "ours.test", Store: st, MaxResponseBytes: 1024,
		AddressPolicy: transport.AllowAllAddresses(), MinBackoff: time.Minute,
		Jitter: func(d time.Duration) time.Duration { return d },
	})

	if _, status, err := client.Send(context.Background(), "fail.test", http.MethodGet, "/fail", nil); err == nil || status != http.StatusBadGateway {
		t.Fatalf("5xx: status=%d err=%v", status, err)
	}

	if st.states["fail.test"].RetryInterval != time.Minute {
		t.Fatalf("5xx с телом не попал в backoff: %+v", st.states["fail.test"])
	}

	if _, _, err := client.Send(context.Background(), "big.test", http.MethodGet, "/big", nil); !errors.Is(err, transport.ErrTooLarge) {
		t.Fatalf("большой ответ: %v", err)
	}

	if _, ok := st.states["big.test"]; ok {
		t.Fatalf("ErrTooLarge поставил сервер в backoff: %+v", st.states["big.test"])
	}

	// Лимит запроса (send_join) заменяет общий.
	if raw, _, err := client.Do(context.Background(), "big.test", http.MethodGet, "/big", nil,
		transport.RequestOptions{MaxResponseBytes: 4096}); err != nil || len(raw) != 2048 {
		t.Fatalf("большой ответ с лимитом запроса: %d байт, %v", len(raw), err)
	}

	// 4xx - сервер доступен: прежний backoff сбрасывается.
	now := time.Now().Add(-time.Hour)
	st.states["forbidden.test"] = store.DestinationState{Destination: "forbidden.test", RetryLastTS: &now, RetryInterval: time.Second}

	if _, status, err := client.Send(context.Background(), "forbidden.test", http.MethodGet, "/forbidden", nil); err == nil || status != http.StatusForbidden {
		t.Fatalf("4xx: status=%d err=%v", status, err)
	}

	if _, ok := st.states["forbidden.test"]; ok {
		t.Fatal("ответ 4xx должен сбрасывать backoff")
	}
}
