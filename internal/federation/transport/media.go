package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Скачивание медиа другого сервера: федеративный media API
// (GET /_matrix/federation/v1/media/download/{mediaId}, ответ multipart/mixed:
// JSON-метаданные и файл либо Location), для старых серверов - фолбэк на
// /_matrix/media/v3/download/{serverName}/{mediaId}?allow_remote=false, как в
// Synapse (MediaRepository._download_remote_file).

// ErrMediaNotFound - сервер-владелец ответил, что медиа нет.
var ErrMediaNotFound = errors.New("transport: remote media not found")

// MediaFile - скачиваемый файл: тело читается потоком, его закрывает вызывающий.
type MediaFile struct {
	ContentType string
	FileName    string
	// Length - Content-Length, если сервер его прислал (-1 - неизвестен).
	Length int64
	Body   io.ReadCloser
}

// FetchMedia скачивает медиа mediaID сервера destination (или миниатюру, если
// thumbnail не пуст: query width/height/method).
func (c *Client) FetchMedia(ctx context.Context, destination, mediaID string, thumbnail url.Values) (MediaFile, error) {
	kind := "download"
	if thumbnail != nil {
		kind = "thumbnail"
	}

	path := "/_matrix/federation/v1/media/" + kind + "/" + url.PathEscape(mediaID)
	if thumbnail != nil {
		path += "?" + thumbnail.Encode()
	}

	resp, err := c.doSigned(ctx, destination, http.MethodGet, path)
	if err != nil {
		return MediaFile{}, err
	}

	if resp.StatusCode == http.StatusOK {
		file, err := c.readMultipartMedia(ctx, resp)
		if err != nil {
			_ = resp.Body.Close()

			return MediaFile{}, err
		}

		return file, nil
	}

	errcode := responseErrcode(resp)
	_ = resp.Body.Close()

	// Сервер не знает федеративного media API: старый Synapse отвечает 404/400
	// M_UNRECOGNIZED (или без JSON), реальное отсутствие медиа - M_NOT_FOUND.
	if !legacyMediaFallback(resp.StatusCode, errcode) {
		return MediaFile{}, mediaStatusError(destination, resp.StatusCode, errcode)
	}

	legacy := "/_matrix/media/v3/" + kind + "/" + url.PathEscape(destination) + "/" + url.PathEscape(mediaID)

	query := url.Values{"allow_remote": {"false"}}
	for key, values := range thumbnail {
		query[key] = values
	}

	resp, err = c.doSigned(ctx, destination, http.MethodGet, legacy+"?"+query.Encode())
	if err != nil {
		return MediaFile{}, err
	}

	if resp.StatusCode != http.StatusOK {
		errcode := responseErrcode(resp)
		_ = resp.Body.Close()

		return MediaFile{}, mediaStatusError(destination, resp.StatusCode, errcode)
	}

	return MediaFile{
		ContentType: resp.Header.Get("Content-Type"), FileName: dispositionFileName(resp.Header.Get("Content-Disposition")),
		Length: resp.ContentLength, Body: resp.Body,
	}, nil
}

// legacyMediaFallback - ответ означает «эндпоинт неизвестен», а не «медиа нет».
func legacyMediaFallback(status int, errcode string) bool {
	switch status {
	case http.StatusNotFound, http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return errcode == "" || errcode == "M_UNRECOGNIZED"
	}

	return false
}

// mediaStatusError - ошибка по ответу сервера-владельца.
func mediaStatusError(destination string, status int, errcode string) error {
	if status == http.StatusNotFound {
		return ErrMediaNotFound
	}

	return fmt.Errorf("transport: media from %s: HTTP %d %s", destination, status, errcode)
}

// responseErrcode - errcode из JSON-тела ответа ("" - тела нет или не JSON).
func responseErrcode(resp *http.Response) string {
	var body struct {
		Errcode string `json:"errcode"`
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || json.Unmarshal(raw, &body) != nil {
		return ""
	}

	return body.Errcode
}

// readMultipartMedia разбирает multipart/mixed: первая часть - JSON-метаданные,
// вторая - файл или заголовок Location (файл отдается по ссылке без подписи).
func (c *Client) readMultipartMedia(ctx context.Context, resp *http.Response) (MediaFile, error) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		return MediaFile{}, fmt.Errorf("transport: media response is not multipart: %q", resp.Header.Get("Content-Type"))
	}

	reader := multipart.NewReader(resp.Body, params["boundary"])

	meta, err := reader.NextPart()
	if err != nil {
		return MediaFile{}, fmt.Errorf("transport: media metadata part: %w", err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(meta, 64<<10))

	part, err := reader.NextPart()
	if err != nil {
		return MediaFile{}, fmt.Errorf("transport: media file part: %w", err)
	}

	if location := part.Header.Get("Location"); location != "" {
		_ = resp.Body.Close()

		return c.fetchMediaLocation(ctx, location)
	}

	return MediaFile{
		ContentType: part.Header.Get("Content-Type"),
		FileName:    dispositionFileName(part.Header.Get("Content-Disposition")),
		Length:      -1,
		Body:        readCloser{Reader: part, Closer: resp.Body},
	}, nil
}

// fetchMediaLocation скачивает файл по ссылке из multipart-ответа: без подписи,
// через тот же HTTP-клиент (политика адресов диалера, редиректы не следуются).
func (c *Client) fetchMediaLocation(ctx context.Context, location string) (MediaFile, error) {
	target, err := url.Parse(location)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") {
		return MediaFile{}, fmt.Errorf("transport: bad media location %q", location)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return MediaFile{}, err
	}

	resp, err := c.http.Do(req) //nolint:gosec // G704: ссылку выдал сервер-владелец медиа
	if err != nil {
		return MediaFile{}, fmt.Errorf("transport: media location: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return MediaFile{}, fmt.Errorf("transport: media location: HTTP %d", resp.StatusCode)
	}

	return MediaFile{
		ContentType: resp.Header.Get("Content-Type"), FileName: dispositionFileName(resp.Header.Get("Content-Disposition")),
		Length: resp.ContentLength, Body: resp.Body,
	}, nil
}

// dispositionFileName - filename из Content-Disposition.
func dispositionFileName(header string) string {
	if header == "" {
		return ""
	}

	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}

	return params["filename"]
}

// readCloser - часть multipart, закрывающая весь ответ.
type readCloser struct {
	io.Reader
	io.Closer
}

// doSigned выполняет подписанный GET с политикой, backoff и перебором адресов,
// как Do, но отдает ответ потоком (тело медиа больше MaxResponseBytes). URL и
// подключение - как в Do: SNI и проверка сертификата на server_name
// (или delegated), реальный адрес цели - в контексте, IP назначения проверяет
// AddressPolicy диалера. Сетевая ошибка и 5xx учитываются в backoff, прочий
// ответ сбрасывает его.
func (c *Client) doSigned(ctx context.Context, destination, method, path string) (*http.Response, error) {
	if err := c.checkPolicy(destination); err != nil {
		return nil, err
	}

	if err := c.checkBackoff(ctx, destination); err != nil {
		return nil, err
	}

	if c.opts.Key == nil {
		return nil, errors.New("transport: no signing key")
	}

	targets, err := c.opts.Resolver.Targets(ctx, destination)
	if err != nil {
		c.metrics.failure(destination, "resolve")
		c.recordFailure(ctx, destination)

		return nil, err
	}

	c.metrics.request(destination)

	var lastErr error

	for _, target := range targets {
		reqCtx, targetURL := requestURL(ctx, target, path)

		req, err := http.NewRequestWithContext(reqCtx, method, targetURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("transport: build request: %w", err)
		}

		if target.HostHeader != "" {
			req.Host = target.HostHeader
		}

		auth, err := signing.MatrixAuthHeader(*c.opts.Key, c.opts.Origin, destination, method, path, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("Authorization", auth)
		req.Header.Set("User-Agent", "syngo/1.0")

		resp, err := c.http.Do(req) //nolint:gosec // G704: адрес из резолвера федерации
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}

			continue
		}

		c.opts.Resolver.CacheTarget(destination, target)

		if resp.StatusCode >= 500 {
			c.metrics.failure(destination, fmt.Sprintf("http_%d", resp.StatusCode))
			c.recordFailure(ctx, destination)
		} else {
			c.metrics.success(destination)
			c.recordSuccess(ctx, destination)
		}

		return resp, nil
	}

	c.metrics.failure(destination, "network")
	c.recordFailure(ctx, destination)

	return nil, fmt.Errorf("transport: request to %s: %w", destination, lastErr)
}
