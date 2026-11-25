package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// TestRemoteQueryCacheExpiry - запись живет ttl, отказ хранится как
// запись без значения, переполнение очищает кэш.
func TestRemoteQueryCacheExpiry(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	cache := &remoteQueryCache{now: func() time.Time { return now }}

	if _, ok := cache.get("a"); ok {
		t.Fatal("пустой кэш вернул запись")
	}

	cache.put("a", []byte(`{}`), time.Minute)
	cache.put("fail", nil, time.Second)

	if entry, ok := cache.get("a"); !ok || string(entry.value) != `{}` {
		t.Fatalf("запись a: %v %v", entry, ok)
	}

	if entry, ok := cache.get("fail"); !ok || entry.value != nil {
		t.Fatalf("отказ: %v %v", entry, ok)
	}

	now = now.Add(2 * time.Second)

	if _, ok := cache.get("fail"); ok {
		t.Fatal("отказ не истек")
	}

	if _, ok := cache.get("a"); !ok {
		t.Fatal("запись a истекла раньше срока")
	}

	for i := range remoteQueryCacheMax {
		cache.put(strings.Repeat("k", i%7)+string(rune('a'+i%26))+time.Duration(i).String(), nil, time.Minute)
	}

	if len(cache.entries) > remoteQueryCacheMax {
		t.Fatalf("кэш вырос до %d", len(cache.entries))
	}
}

// TestRemoteQueryClient - профиль и алиас чужого сервера через
// /query/*, ответы и отказы кэшируются, мусорный ответ - ошибка ErrNotFound.
func TestRemoteQueryClient(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		switch r.URL.Query().Get("user_id") + r.URL.Query().Get("room_alias") {
		case "@bob:remote.test":
			_, _ = w.Write([]byte(`{"displayname":"Bob","avatar_url":42}`))
		case "#room:remote.test":
			_, _ = w.Write([]byte(`{"room_id":"!r:remote.test","servers":[1,""]}`))
		case "#bad:remote.test":
			_, _ = w.Write([]byte(`{"room_id":"not-a-room"}`))
		case "@array:remote.test":
			_, _ = w.Write([]byte(`[]`))
		case "@empty:remote.test":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(remote.Close)

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	host := strings.TrimPrefix(remote.URL, "http://")
	resolver := transport.NewResolver()
	resolver.TargetsOverride = func(_ context.Context, serverName string) ([]transport.Target, error) {
		return []transport.Target{{ServerName: serverName, Host: host, Scheme: "http"}}, nil
	}

	cfg := config.Default()
	cfg.Matrix.ServerName = "test.local"

	s := &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	if _, err := s.remoteProfile(ctx, "@bob:remote.test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("без транспорта: %v", err)
	}

	if s.isRemote("@bob:remote.test") {
		t.Fatal("без транспорта пользователь не должен считаться удаленным")
	}

	s.SetFederationTransport(transport.NewClient(transport.Options{
		Resolver: resolver, Key: &key, Origin: "test.local", AddressPolicy: transport.AllowAllAddresses(),
	}))

	if !s.isRemote("@bob:remote.test") || s.isRemote("@alice:test.local") {
		t.Fatal("isRemote")
	}

	for range 2 {
		profile, err := s.remoteProfile(ctx, "@bob:remote.test")
		if err != nil || profile.DisplayName != "Bob" || profile.AvatarURL != "" {
			t.Fatalf("профиль: %+v %v", profile, err)
		}
	}

	roomID, servers, err := s.remoteAlias(ctx, "#room:remote.test")
	if err != nil || roomID != "!r:remote.test" || len(servers) != 1 || servers[0] != "remote.test" {
		t.Fatalf("алиас: %s %v %v", roomID, servers, err)
	}

	if hits.Load() != 2 {
		t.Fatalf("запросов: %d, ожидали 2 (кэш)", hits.Load())
	}

	for _, id := range []string{"@array:remote.test", "@empty:remote.test", "@down:remote.test", "@x:bad_name!"} {
		if _, err := s.remoteProfile(ctx, id); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("%s: %v, ожидали ErrNotFound", id, err)
		}
	}

	if _, _, err := s.remoteAlias(ctx, "#bad:remote.test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("мусорный алиас: %v", err)
	}

	before := hits.Load()

	if _, err := s.remoteProfile(ctx, "@down:remote.test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("отказ из кэша: %v", err)
	}

	if hits.Load() != before {
		t.Fatal("отказ не закэширован")
	}
}
