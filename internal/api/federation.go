package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/core"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/build"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// SetFederationKey задаёт ключ сервера для подписи ответов и федеративных запросов.
// Вызывается из main после загрузки ключа; при выключенной федерации ключа нет.
func (s *Server) SetFederationKey(key *signing.Key) {
	s.federationKey = key

	// Свои события подписываются при создании, и исходный PDU хранится:
	// в федерацию уходит ровно он, без пересборки из колонок.
	if s.rooms != nil && key != nil {
		serverName := s.cfg.Matrix.ServerName
		s.rooms.WithPDUSigner(func(object map[string]any, version int) (map[string]any, error) {
			return eventjson.SignVersioned(*key, serverName, object, version)
		})
	}
}

// handleServerKeys — GET /_matrix/key/v2/server[/{keyId}].
func (s *Server) handleServerKeys(w http.ResponseWriter, r *http.Request) {
	if s.federationKey == nil {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Federation is disabled")

		return
	}

	requested := ""
	if keyID := pathParamValue(r, "keyId"); keyID != "" {
		decoded, ok := pathParam(r, "keyId")
		if !ok {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid key id")

			return
		}

		requested = decoded
	}

	oldKeys := s.cfg.Federation.OldSigningKeys

	if _, isOld := oldKeys[requested]; requested != "" && requested != s.federationKey.KeyID && !isOld {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Unknown key id")

		return
	}

	// Получатель все равно ограничит срок 7 днями (спека), публиковать больше незачем.
	days := s.cfg.Federation.ValidUntilDays
	if days <= 0 || days > 7 {
		days = 7
	}

	// old_verify_keys - прежние ключи из federation.old_signing_keys: ими подписаны
	// старые события, получатель проверяет их до expired_ts.
	old := make(map[string]any, len(oldKeys))
	for keyID, key := range oldKeys {
		if keyID == s.federationKey.KeyID {
			continue
		}

		old[keyID] = map[string]any{"key": key.Key, "expired_ts": key.ExpiredTS}
	}

	object := map[string]any{
		"server_name":     s.cfg.Matrix.ServerName,
		"valid_until_ts":  time.Now().AddDate(0, 0, days).UnixMilli(),
		"verify_keys":     map[string]any{s.federationKey.KeyID: map[string]any{"key": s.federationKey.Public()}},
		"old_verify_keys": old,
	}

	signed, err := signing.SignJSON(*s.federationKey, s.cfg.Matrix.ServerName, object)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api: sign key server response", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to sign response")

		return
	}

	writeJSON(w, http.StatusOK, signed)
}

// requireFederation — middleware для федеративных маршрутов: проверяет X-Matrix.
//
// Тело читается с ограничением и восстанавливается, чтобы обработчик получил его целиком.
// До проверки подписи origin проверяется по грамматике server_name и по
// allow_hosts/deny_hosts (403, ключи чужого сервера не запрашиваются); лимит по
// origin - только после проверки подписи.
func (s *Server) requireFederation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.federationKey == nil {
			writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Federation is disabled")

			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxFederationBodySize+1))
		if err != nil {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeBadJSON, "Cannot read request body")

			return
		}

		// Больше предела - 413, а не молча обрезанное тело и 401 на подписи.
		if len(body) > maxFederationBodySize {
			writeMatrixError(w, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "Request body is too large")

			return
		}

		_ = r.Body.Close()

		r.Body = io.NopCloser(newBytesReader(body))

		if header, err := signing.ParseMatrixAuth(r.Header.Get("Authorization")); err == nil {
			if _, err := transport.ParseServerName(header.Origin); err != nil {
				writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Invalid origin")

				return
			}

			if err := s.federationHosts.Check(header.Origin); err != nil {
				s.log.WarnContext(r.Context(), "api: federation request denied by policy",
					"origin", header.Origin, "route", routePattern(r))
				writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Federation denied with "+header.Origin)

				return
			}
		}

		auth, err := signing.VerifyRequest(r, body, s.cfg.Matrix.ServerName, s.federationKeyLookup(r.Context()))
		if err != nil {
			// Без заголовка Authorization и тела: в них подпись и
			// содержимое событий; для разбора хватает ошибки и размера.
			s.log.WarnContext(r.Context(), "api: federation auth failed", "error", err,
				"route", routePattern(r), "body_bytes", len(body))
			writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "Unauthorized federation request")

			return
		}

		if s.federationIncoming != nil && !s.federationIncoming.AllowOrigin(w, auth.Origin) {
			return
		}

		next.ServeHTTP(w, r)
	})
}

// lookupServerKeyCtx - ключ origin/keyID: кэш БД, при промахе или истечении срока -
// нотарии и сам сервер (через резолвер).
//
// Возвращаемый срок - valid_until_ts (не дольше 7 дней от получения) или
// expired_ts для old_verify_keys. Просроченный ключ из кэша возвращается, если
// свежего нет: событие с более ранним origin_server_ts им еще проверяется, а
// проверка запроса (по текущему времени) его отвергнет.
func (s *Server) lookupServerKeyCtx(ctx context.Context, origin, keyID string) (publicKey string, validUntil int64, err error) {
	if s.db == nil {
		return "", 0, fmt.Errorf("api: no database for key cache")
	}

	cachedKey, cachedUntil, ok, err := s.db.CachedServerKey(ctx, origin, keyID)
	if err != nil {
		return "", 0, err
	}

	// valid_until_ts 0 в старом кэше раньше означал «вечно» - теперь это просрочка.
	if ok && time.Now().UnixMilli() < cachedUntil {
		return cachedKey, cachedUntil, nil
	}

	stale := func(cause error) (string, int64, error) {
		if ok && cachedUntil > 0 {
			return cachedKey, cachedUntil, nil
		}

		return "", 0, cause
	}

	if s.keyFetcher == nil {
		return stale(errors.New("api: no key fetcher"))
	}

	fetched, err := s.keyFetcher.Fetch(ctx, origin, keyID)
	if err != nil {
		return stale(err)
	}

	s.saveServerKeys(ctx, fetched)

	key, until, found := fetched.Lookup(keyID)
	if !found {
		return stale(fmt.Errorf("%w: %s %s", transport.ErrKeyNotFound, origin, keyID))
	}

	return key, until, nil
}

// saveServerKeys кэширует ключи: verify_keys - со сроком valid_until_ts,
// old_verify_keys - каждый со своим expired_ts.
func (s *Server) saveServerKeys(ctx context.Context, keys transport.ServerKeys) {
	if err := s.db.SaveServerKeys(ctx, keys.ServerName, keys.VerifyKeys, keys.ValidUntilTS); err != nil {
		s.log.WarnContext(ctx, "api: cache server keys failed", "origin", keys.ServerName, "error", err)
	}

	for keyID, old := range keys.OldVerifyKeys {
		if _, current := keys.VerifyKeys[keyID]; current {
			continue
		}

		if err := s.db.SaveServerKeys(ctx, keys.ServerName, map[string]string{keyID: old.Key}, old.ExpiredTS); err != nil {
			s.log.WarnContext(ctx, "api: cache old server key failed", "origin", keys.ServerName, "error", err)
		}
	}
}

// verifyFederationJSON проверяет подпись федеративного события. На интероп-стенде
// (federation.insecure_skip_signature_verify) проверка отключается с громким логом:
// Synapse 1.165 присылает события, подпись которых не воспроизводится его же ключом.
// В prod опция запрещена валидацией конфига.
func (s *Server) verifyFederationJSON(ctx context.Context, object map[string]any, origin string) error {
	return s.verifyFederationJSONVersion(ctx, object, origin, s.versionForObject(ctx, object))
}

// verifyFederationJSONVersion - то же по правилам известной версии комнаты (room_version
// /v2/invite: комнаты v12 у нас еще нет).
func (s *Server) verifyFederationJSONVersion(ctx context.Context, object map[string]any, origin string, version int) error {
	if s.cfg.Federation.InsecureSkipSignatureVerify {
		s.log.WarnContext(ctx, "api: federation signature verify DISABLED (insecure skip, стенд)", "origin", origin)

		return nil
	}

	err := eventjson.VerifyVersioned(object, origin, version, s.federationKeyLookup(ctx))
	if err != nil {
		// Дамп canonical JSON и версии комнаты: без него расхождение подписи
		// (набор полей, redaction, hashes) не разобрать.
		if raw, mErr := json.Marshal(object); mErr == nil {
			s.log.WarnContext(ctx, "api: федеративное событие не прошло проверку подписи",
				"origin", origin, "room_version", version,
				"b64", base64.RawStdEncoding.EncodeToString(raw), "error", err)
		}
	}

	return err
}

// versionForObject — версия комнаты события: из rooms, иначе из content create
// (первое событие комнаты ещё не записано в rooms), иначе DefaultRoomVersion.
// От версии зависят правила redaction при проверке подписи (v11 ≠ v10).
func (s *Server) versionForObject(ctx context.Context, object map[string]any) int {
	roomID, _ := object["room_id"].(string)
	if roomID != "" {
		if version := s.roomVersionOf(ctx, roomID); version != 0 {
			return version
		}
	}

	if version := roomVersionFromCreate(object); version != 0 {
		return version
	}

	// Входящее приглашение: комнаты у нас ещё нет, но версия приходит в
	// unsigned.invite_room_state (урезанный m.room.create). Без этого invite
	// v11-комнаты проверялся бы по правилам v10 и подпись была бы «invalid».
	return inviteRoomVersion(object)
}

// roomVersionFromCreate читает content.room_version из события m.room.create.
func roomVersionFromCreate(object map[string]any) int {
	typ, _ := object["type"].(string)
	if typ != "m.room.create" {
		return 0
	}

	return roomVersionFromContent(object["content"])
}

// inviteRoomVersion ищет m.room.create в unsigned.invite_room_state приглашения.
func inviteRoomVersion(object map[string]any) int {
	unsigned, ok := object["unsigned"].(map[string]any)
	if !ok {
		return core.DefaultRoomVersion
	}

	states, ok := unsigned["invite_room_state"].([]any)
	if !ok {
		return core.DefaultRoomVersion
	}

	for _, item := range states {
		state, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if version := roomVersionFromCreate(state); version != 0 {
			return version
		}
	}

	return core.DefaultRoomVersion
}

// roomVersionFromContent разбирает room_version из content события.
func roomVersionFromContent(content any) int {
	fields, ok := content.(map[string]any)
	if !ok {
		return 0
	}

	raw, ok := fields["room_version"].(string)
	if !ok {
		return 0
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}

	return parsed
}

// maxFederationBodySize — предел тела федеративного запроса (транзакция событий).
const maxFederationBodySize = 1 << 20

// SetRemoteJoiner задаёт исходящий join-флоу (наш пользователь → чужая комната).
func (s *Server) SetRemoteJoiner(joiner *join.Joiner) {
	// Ключи серверов для проверки ответа send_join - те же, что у входящей
	// федерации: кэш БД и KeyFetcher (нотарии, самоподпись, 7 дней).
	if joiner != nil {
		joiner.SetKeyLookup(s.federationKeyLookup)
	}

	s.remoteJoiner = joiner
}

// SetFederationTransport задаёт исходящий клиент федерации (нужен блокам 3+).
func (s *Server) SetFederationTransport(client *transport.Client) {
	s.federationTransport = client
}

// federationRoutes подключает публичные маршруты федерации.
func (s *Server) registerFederationRoutes(r chi.Router) {
	r.Get("/_matrix/key/v2/server", s.handleServerKeys)
	r.Get("/_matrix/key/v2/server/{keyId}", s.handleServerKeys)
	// OpenID userinfo: аутентификация по одноразовому access_token, X-Matrix не нужен.
	r.Get("/_matrix/federation/v1/openid/userinfo", s.handleOpenIDUserInfo)
	// Лицензия и ссылка на исходники доступны даже при выключенной федерации:
	// это публичное AGPL-предложение для всех сетевых пользователей.
	r.Get("/_syngo/v1/source", s.handleFederationVersion)
	// make_join, как и остальные запросы сервер-сервер, подписан X-Matrix (спека):
	// без подписи любой мог получить шаблон и состояние комнаты.

	// Федеративные маршруты: rate limit/размер тела (incoming) → проверка X-Matrix.
	r.Group(func(r chi.Router) {
		if s.federationIncoming != nil {
			r.Use(s.federationIncoming.Wrap)
		}

		r.Use(s.requireFederation)
		r.Get("/_matrix/federation/v1/version", s.handleFederationVersion)
		s.registerFederationSendRoutes(r)
		s.registerFederationJoinRoutes(r)
		s.registerFederationStateRoutes(r)
		s.registerFederationLeaveRoutes(r)
		s.registerFederationKeysRoutes(r)
		s.registerFederationMediaRoutes(r)
		s.registerFederationQueryRoutes(r)
		s.registerFederationHierarchyRoutes(r)
	})
}

// handleFederationVersion обслуживает /_matrix/federation/v1/version и публичный
// /_syngo/v1/source: имя, версия, лицензия и Corresponding Source (AGPL section 13).
func (s *Server) handleFederationVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server": map[string]string{
			"name":       build.Name,
			"version":    build.Semver(),
			"license":    build.LicenseID,
			"source_url": build.SourceURL(),
		},
	})
}

// newBytesReader — io.Reader над срезом байт (без bytes-импорта в основном коде).
func newBytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}

	n := copy(p, r.b[r.i:])
	r.i += n

	return n, nil
}
