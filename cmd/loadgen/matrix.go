package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// apiError - ответ сервера не 2xx.
type apiError struct {
	Status     int
	ErrCode    string
	Message    string
	RetryAfter time.Duration
	// Body - сырое тело (для UIA 401 при регистрации).
	Body []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d %s: %.200s", e.Status, e.ErrCode, e.Message)
}

// statusOf - HTTP-код ошибки (0 - сетевая ошибка или таймаут).
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}

	return 0
}

// client - HTTP-клиент Matrix CS API, который пишет каждую попытку в recorder.
type client struct {
	base string
	http *http.Client
	rec  *recorder
}

// newHTTPClient - транспорт под нагрузку: у каждого пользователя long-poll /sync
// плюс запрос действия, поэтому пул соединений на хост - не меньше 2*users.
func newHTTPClient(users int, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: max(2*users+16, 64),
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	return &http.Client{Transport: transport, Timeout: timeout}
}

// request - один вызов API.
type request struct {
	// endpoint - метка в отчете ("send", "sync_longpoll"), не путь: путь содержит id.
	endpoint string
	method   string
	path     string
	token    string
	// body - JSON-тело (nil - без тела); raw/contentType - произвольное тело.
	body        any
	raw         []byte
	contentType string
	// out - куда разобрать JSON ответа (nil - тело читается и отбрасывается).
	out any
}

// call выполняет запрос и пишет латентность и код в recorder. Тело ответа
// дочитывается всегда: иначе соединение не возвращается в пул.
func (c *client) call(ctx context.Context, req request) error {
	var body io.Reader = http.NoBody

	switch {
	case req.raw != nil:
		body = bytes.NewReader(req.raw)
	case req.body != nil:
		data, err := json.Marshal(req.body)
		if err != nil {
			return fmt.Errorf("%s: тело: %w", req.endpoint, err)
		}

		body = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, c.base+req.path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", req.endpoint, err)
	}

	if req.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.token)
	}

	switch {
	case req.contentType != "":
		httpReq.Header.Set("Content-Type", req.contentType)
	case req.body != nil:
		httpReq.Header.Set("Content-Type", "application/json")
	}

	started := time.Now()

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Отмена контекста в конце прогона - не ошибка сервера.
		if ctx.Err() == nil {
			c.rec.observe(req.endpoint, time.Since(started), 0)
		}

		return fmt.Errorf("%s: %w", req.endpoint, err)
	}

	defer resp.Body.Close()

	data, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(started)

	if readErr != nil {
		if ctx.Err() == nil {
			c.rec.observe(req.endpoint, elapsed, 0)
		}

		return fmt.Errorf("%s: чтение ответа: %w", req.endpoint, readErr)
	}

	c.rec.observe(req.endpoint, elapsed, resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp, data)
	}

	if req.out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, req.out); err != nil {
			return fmt.Errorf("%s: разбор ответа: %w", req.endpoint, err)
		}
	}

	return nil
}

// parseAPIError - ошибка Matrix (errcode, error, retry_after_ms или Retry-After).
func parseAPIError(resp *http.Response, data []byte) *apiError {
	ae := &apiError{Status: resp.StatusCode, Body: data}

	var body struct {
		ErrCode      string `json:"errcode"`
		Error        string `json:"error"`
		RetryAfterMS int64  `json:"retry_after_ms"`
	}
	if json.Unmarshal(data, &body) == nil {
		ae.ErrCode, ae.Message = body.ErrCode, body.Error
		ae.RetryAfter = time.Duration(body.RetryAfterMS) * time.Millisecond
	} else {
		ae.Message = strings.TrimSpace(string(data))
	}

	if ae.RetryAfter == 0 {
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ae.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	return ae
}

// pathEscape - сегмент пути (id комнат и событий содержат ! $ : #).
func pathEscape(s string) string {
	var b strings.Builder

	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}

	return b.String()
}
