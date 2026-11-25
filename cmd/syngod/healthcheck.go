package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/config"
)

// Healthcheck контейнера: в distroless-образе нет shell и curl, а
// `/syngod -version` всегда завершался с 0. `syngod -healthcheck` запрашивает
// /health/ready у запущенного процесса и возвращает ненулевой код, если сервис
// не готов (нет БД, дренаж при остановке).

// healthURL строит адрес /health/ready по адресу листенера (SYNGOD_SERVER_ADDR).
func healthURL(addr string, tlsOn bool) (string, error) {
	if strings.TrimSpace(addr) == "" {
		addr = ":8008"
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("healthcheck: server addr %q: %w", addr, err)
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	scheme := "http"
	if tlsOn {
		scheme = "https"
	}

	return scheme + "://" + net.JoinHostPort(host, port) + "/health/ready", nil
}

// checkHealth выполняет запрос: 200 - готов, иначе ошибка с началом тела ответа.
func checkHealth(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody) //nolint:gosec // loopback собственного процесса из SYNGOD_SERVER_ADDR
	if err != nil {
		return err
	}

	resp, err := client.Do(req) //nolint:gosec // loopback собственного процесса из SYNGOD_SERVER_ADDR
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s -> %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// runHealthcheck - режим `syngod -healthcheck`. Конфиг не грузится: проверка не
// должна зависеть от секретов, адрес берется из окружения контейнера.
func runHealthcheck() error {
	tlsOn := os.Getenv(config.EnvTLSCertFile) != ""

	url, err := healthURL(os.Getenv(config.EnvServerAddr), tlsOn)
	if err != nil {
		return err
	}

	transport := &http.Transport{}
	if tlsOn {
		// Запрос идет на loopback собственного процесса: имя в сертификате не совпадет.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // loopback своего процесса
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	return checkHealth(ctx, &http.Client{Transport: transport}, url)
}
