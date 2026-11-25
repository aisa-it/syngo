package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// Профиль и алиасы чужих серверов для клиентского API: клиентские
// /profile/{userId} и /directory/room/{alias} для чужого сервера идут в его
// /query/profile и /query/directory с кэшем и таймаутом. Раньше - 404 без
// запроса, и Element на нашем сервере считал удаленных пользователей
// несуществующими, а вход по чужому алиасу не работал.

const (
	// remoteQueryTimeout - предел запроса к чужому серверу: клиент ждет ответа.
	remoteQueryTimeout = 10 * time.Second
	// remoteQueryTTL - срок ответа в кэше: профиль и алиас меняются редко, а
	// Element запрашивает профиль на каждое приглашение и открытие участника.
	remoteQueryTTL = time.Minute
	// remoteQueryNegativeTTL - срок отказа в кэше: недоступный сервер не держит
	// каждый запрос клиента на таймауте.
	remoteQueryNegativeTTL = 15 * time.Second
	// remoteQueryCacheMax - предел записей; при переполнении кэш очищается.
	remoteQueryCacheMax = 10000
)

// errRemoteQuery - чужой сервер не ответил или ответил отказом. Оборачивает
// core.ErrNotFound: клиент получает 404 M_NOT_FOUND.
var errRemoteQuery = fmt.Errorf("api: remote query failed: %w", core.ErrNotFound)

// remoteQueryCache - кэш ответов /query/* чужих серверов (положительный и
// отрицательный) с объединением одновременных запросов. Нулевое значение готово.
type remoteQueryCache struct {
	mu      sync.Mutex
	entries map[string]remoteQueryEntry
	flight  singleflight.Group
	now     func() time.Time
}

// remoteQueryEntry - ответ в кэше; value nil - отказ.
type remoteQueryEntry struct {
	value   json.RawMessage
	expires time.Time
}

func (c *remoteQueryCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}

	return time.Now()
}

// get возвращает запись, если она не истекла.
func (c *remoteQueryCache) get(key string) (remoteQueryEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return remoteQueryEntry{}, false
	}

	if !c.clock().Before(entry.expires) {
		delete(c.entries, key)

		return remoteQueryEntry{}, false
	}

	return entry, true
}

// put сохраняет запись на ttl.
func (c *remoteQueryCache) put(key string, value json.RawMessage, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil || len(c.entries) >= remoteQueryCacheMax {
		c.entries = map[string]remoteQueryEntry{}
	}

	c.entries[key] = remoteQueryEntry{value: value, expires: c.clock().Add(ttl)}
}

// remoteQuery выполняет GET /_matrix/federation/v1/query/{kind} к destination с
// кэшем и таймаутом. Ошибка - errRemoteQuery (в том числе из кэша).
func (s *Server) remoteQuery(ctx context.Context, destination, kind string, args url.Values) (json.RawMessage, error) {
	if s.federationTransport == nil {
		return nil, fmt.Errorf("%w: federation is disabled", errRemoteQuery)
	}

	if _, err := transport.ParseServerName(destination); err != nil {
		return nil, fmt.Errorf("%w: %w", errRemoteQuery, err)
	}

	path := "/_matrix/federation/v1/query/" + kind + "?" + args.Encode()
	key := destination + " " + path

	if entry, ok := s.remoteQueries.get(key); ok {
		if entry.value == nil {
			return nil, fmt.Errorf("%w: cached failure", errRemoteQuery)
		}

		return entry.value, nil
	}

	// Одновременные запросы одного ключа (Element открывает несколько вкладок)
	// идут в сеть один раз; отмена запроса клиента не обрывает общий запрос.
	result, err, _ := s.remoteQueries.flight.Do(key, func() (any, error) {
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteQueryTimeout)
		defer cancel()

		raw, err := s.fetchRemoteQuery(queryCtx, destination, path)
		if err != nil {
			s.remoteQueries.put(key, nil, remoteQueryNegativeTTL)

			return nil, err
		}

		s.remoteQueries.put(key, raw, remoteQueryTTL)

		return raw, nil
	})
	if err != nil {
		return nil, err
	}

	raw, _ := result.(json.RawMessage)

	return raw, nil
}

// fetchRemoteQuery - сам запрос: ответ должен быть 2xx с JSON-объектом.
func (s *Server) fetchRemoteQuery(ctx context.Context, destination, path string) (json.RawMessage, error) {
	// ignore_backoff, как make_query в Synapse: запрос пользователя, а не очередь.
	raw, status, err := s.federationTransport.Do(ctx, destination, http.MethodGet, path, nil,
		transport.RequestOptions{IgnoreBackoff: true})
	if err == nil && status/100 != 2 {
		err = fmt.Errorf("HTTP %d", status)
	}

	if err == nil {
		var object map[string]json.RawMessage
		if jsonErr := json.Unmarshal(raw, &object); jsonErr != nil || object == nil {
			err = errors.New("response is not a JSON object")
		}
	}

	if err != nil {
		// 404 - штатный ответ «нет такого»; остальное - сбой чужого сервера.
		if status != http.StatusNotFound {
			s.log.WarnContext(ctx, "api: remote query failed",
				"destination", destination, "path", strings.SplitN(path, "?", 2)[0], "status", status, "error", err)
		}

		return nil, fmt.Errorf("%w: %w", errRemoteQuery, err)
	}

	return raw, nil
}

// isRemote сообщает, что идентификатор принадлежит чужому серверу и федерация
// включена: тогда профиль и алиас запрашиваются у него.
func (s *Server) isRemote(id string) bool {
	server := domainOf(id)

	return s.federationTransport != nil && server != "" && server != s.cfg.Matrix.ServerName
}

// remoteProfile - профиль пользователя чужого сервера (/query/profile).
func (s *Server) remoteProfile(ctx context.Context, userID string) (profileResponse, error) {
	raw, err := s.remoteQuery(ctx, domainOf(userID), "profile", url.Values{"user_id": {userID}})
	if err != nil {
		return profileResponse{}, err
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return profileResponse{}, fmt.Errorf("%w: %w", errRemoteQuery, err)
	}

	// Поля не строкой (null, число) - как отсутствующие.
	displayName, _ := fields["displayname"].(string)
	avatarURL, _ := fields["avatar_url"].(string)

	return profileResponse{DisplayName: displayName, AvatarURL: avatarURL}, nil
}

// readProfile - профиль для клиентского GET: свой - из БД, чужой - через федерацию.
func (s *Server) readProfile(ctx context.Context, userID string) (profileResponse, error) {
	if s.isRemote(userID) {
		return s.remoteProfile(ctx, userID)
	}

	profile, err := s.profiles.Profile(ctx, userID)
	if err != nil {
		return profileResponse{}, err
	}

	return profileResponse{DisplayName: profile.DisplayName, AvatarURL: profile.AvatarURL}, nil
}

// remoteAlias резолвит алиас чужого сервера (/query/directory). servers без
// значений - сервер алиаса.
func (s *Server) remoteAlias(ctx context.Context, alias string) (roomID string, servers []string, err error) {
	raw, err := s.remoteQuery(ctx, domainOf(alias), "directory", url.Values{"room_alias": {alias}})
	if err != nil {
		return "", nil, err
	}

	var body struct {
		RoomID  string `json:"room_id"`
		Servers []any  `json:"servers"`
	}

	if err := json.Unmarshal(raw, &body); err != nil || !strings.HasPrefix(body.RoomID, "!") {
		return "", nil, fmt.Errorf("%w: bad directory response", errRemoteQuery)
	}

	for _, server := range body.Servers {
		if name, ok := server.(string); ok && name != "" {
			servers = append(servers, name)
		}
	}

	if len(servers) == 0 {
		servers = []string{domainOf(alias)}
	}

	return body.RoomID, servers, nil
}
