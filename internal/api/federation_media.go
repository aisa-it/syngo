package api

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/media"
)

// Медиа по федерации: скачивание медиа других серверов для наших
// клиентов (media.RemoteFetcher поверх транспорта) и отдача нашего медиа
// другим серверам (GET /_matrix/federation/v1/media/download|thumbnail).
// Раньше чужое mxc:// всегда давало 404, а Synapse не мог скачать вложение
// нашего пользователя: legacy-путь после freeze authenticated media его не отдает.

// federationMediaFetcher - media.RemoteFetcher поверх транспорта федерации.
type federationMediaFetcher struct{ s *Server }

var _ media.RemoteFetcher = federationMediaFetcher{}

// remoteFile переводит ответ транспорта в media.RemoteFile.
func remoteFile(file transport.MediaFile, err error) (media.RemoteFile, error) {
	if err != nil {
		if errors.Is(err, transport.ErrMediaNotFound) {
			return media.RemoteFile{}, media.ErrNotFound
		}

		return media.RemoteFile{}, err
	}

	return media.RemoteFile{ContentType: file.ContentType, FileName: file.FileName, Body: file.Body}, nil
}

// FetchRemote скачивает медиа у сервера-владельца.
func (f federationMediaFetcher) FetchRemote(ctx context.Context, serverName, mediaID string) (media.RemoteFile, error) {
	client := f.s.federationTransport
	if client == nil || f.s.federationKey == nil {
		return media.RemoteFile{}, media.ErrNotFound
	}

	return remoteFile(client.FetchMedia(ctx, serverName, mediaID, nil))
}

// FetchRemoteThumbnail запрашивает миниатюру у сервера-владельца.
func (f federationMediaFetcher) FetchRemoteThumbnail(
	ctx context.Context, serverName, mediaID string, params media.ThumbnailParams,
) (media.RemoteFile, error) {
	client := f.s.federationTransport
	if client == nil || f.s.federationKey == nil {
		return media.RemoteFile{}, media.ErrNotFound
	}

	query := url.Values{
		"width": {strconv.Itoa(params.Width)}, "height": {strconv.Itoa(params.Height)}, "method": {params.Method},
	}

	return remoteFile(client.FetchMedia(ctx, serverName, mediaID, query))
}

// writeFederationMedia - ответ multipart/mixed федеративного media API:
// JSON-метаданные (пустой объект) и файл.
func (s *Server) writeFederationMedia(w http.ResponseWriter, r *http.Request, contentType, disposition string, body io.Reader) {
	mw := multipart.NewWriter(w)

	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)

	meta, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	if err == nil {
		_, err = meta.Write([]byte("{}"))
	}

	if err == nil {
		var part io.Writer

		part, err = mw.CreatePart(textproto.MIMEHeader{
			"Content-Type": {contentType}, "Content-Disposition": {disposition},
		})
		if err == nil {
			_, err = io.Copy(part, body)
		}
	}

	if err == nil {
		err = mw.Close()
	}

	if err != nil {
		s.log.WarnContext(r.Context(), "api: federation media stream interrupted", "error", err)
	}
}

// handleFederationMediaDownload - GET /_matrix/federation/v1/media/download/{mediaId}:
// только наше медиа, чужое по федерации не ретранслируется.
func (s *Server) handleFederationMediaDownload(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Media not found")

		return
	}

	meta, body, err := s.media.DownloadWith(r.Context(), s.cfg.Matrix.ServerName, pathParamValue(r, "mediaId"),
		media.ReadOptions{MaxWait: 0})
	if err != nil {
		s.writeMediaReadError(w, r, err)

		return
	}

	defer func() { _ = body.Close() }()

	s.writeFederationMedia(w, r, meta.ContentType, contentDisposition(meta.ContentType, meta.FileName), body)
}

// handleFederationMediaThumbnail - GET /_matrix/federation/v1/media/thumbnail/{mediaId}.
func (s *Server) handleFederationMediaThumbnail(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Media not found")

		return
	}

	query := r.URL.Query()

	width, _ := strconv.Atoi(query.Get("width"))
	height, _ := strconv.Atoi(query.Get("height"))

	thumb, err := s.media.Thumbnail(r.Context(), s.cfg.Matrix.ServerName, pathParamValue(r, "mediaId"),
		media.ThumbnailParams{Width: width, Height: height, Method: query.Get("method")}, media.ReadOptions{})
	if err != nil {
		s.writeMediaReadError(w, r, err)

		return
	}

	s.writeFederationMedia(w, r, thumb.ContentType, "inline", bytesReader(thumb.Data))
}

// bytesReader - io.Reader над срезом.
func bytesReader(b []byte) io.Reader { return newBytesReader(b) }

// registerFederationMediaRoutes подключает федеративный media API.
func (s *Server) registerFederationMediaRoutes(r chi.Router) {
	r.Get("/_matrix/federation/v1/media/download/{mediaId}", s.handleFederationMediaDownload)
	r.Get("/_matrix/federation/v1/media/thumbnail/{mediaId}", s.handleFederationMediaThumbnail)
}
