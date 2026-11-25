package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		tls  bool
		want string
	}{
		{"", false, "http://127.0.0.1:8008/health/ready"},
		{":8008", false, "http://127.0.0.1:8008/health/ready"},
		{"0.0.0.0:9000", false, "http://127.0.0.1:9000/health/ready"},
		{"127.0.0.1:28008", false, "http://127.0.0.1:28008/health/ready"},
		{"[::]:8448", true, "https://127.0.0.1:8448/health/ready"},
	}

	for _, tt := range tests {
		got, err := healthURL(tt.addr, tt.tls)
		if err != nil || got != tt.want {
			t.Errorf("healthURL(%q) = %q, %v; ожидали %q", tt.addr, got, err, tt.want)
		}
	}

	if _, err := healthURL("8008", false); err == nil {
		t.Error("адрес без порта должен давать ошибку")
	}
}

// TestCheckHealth - healthcheck падает, если сервис не готов (раньше
// healthcheck образа был `/syngod -version` и всегда завершался с 0).
func TestCheckHealth(t *testing.T) {
	t.Parallel()

	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"status":"unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	if err := checkHealth(context.Background(), srv.Client(), srv.URL+"/health/ready"); err == nil {
		t.Fatal("503 должен давать ошибку healthcheck")
	}

	status.Store(http.StatusOK)
	if err := checkHealth(context.Background(), srv.Client(), srv.URL+"/health/ready"); err != nil {
		t.Fatalf("200: %v", err)
	}

	srv.Close()

	if err := checkHealth(context.Background(), http.DefaultClient, srv.URL+"/health/ready"); err == nil {
		t.Fatal("недоступный сервер должен давать ошибку")
	}
}
