package join

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
)

// KeyLookup - поиск ключа сервера origin/keyID в контексте запроса: публичный
// ключ и срок действия (valid_until_ts, для old_verify_keys - expired_ts).
type KeyLookup func(ctx context.Context) func(origin, keyID string) (string, int64, error)

// SetKeyLookup подключает поиск ключей сервера (в проде - кэш БД и
// transport.KeyFetcher: нотарии, самоподпись ответа, old_verify_keys, срок не
// больше 7 дней). Без него ключи запрашиваются у самого сервера и
// проверяются transport.ParseServerKeys.
func (j *Joiner) SetKeyLookup(lookup KeyLookup) {
	j.keys = lookup
}

// lookupForJoin - поиск ключей на один join: каждый сервер спрашивается не
// больше одного раза, сколько бы его событий ни было в ответе. Раньше ключ
// запрашивался заново на каждое событие, без кэша и проверки самоподписи.
func (j *Joiner) lookupForJoin(ctx context.Context) func(origin, keyID string) (string, int64, error) {
	var base func(origin, keyID string) (string, int64, error)

	if j.keys != nil {
		base = j.keys(ctx)
	} else {
		base = j.directKeys(ctx)
	}

	type result struct {
		key   string
		until int64
		err   error
	}

	var (
		mu    sync.Mutex
		cache = map[[2]string]result{}
	)

	return func(origin, keyID string) (string, int64, error) {
		// Свое имя: проверяем своим ключом из памяти - ходить в себя по HTTP незачем.
		if origin == j.origin && j.key != nil {
			if keyID != "" && keyID != j.key.KeyID {
				return "", 0, signing.ErrNoSignature
			}

			return j.key.Public(), 0, nil
		}

		mu.Lock()
		defer mu.Unlock()

		k := [2]string{origin, keyID}
		if cached, ok := cache[k]; ok {
			return cached.key, cached.until, cached.err
		}

		key, until, err := base(origin, keyID)
		cache[k] = result{key: key, until: until, err: err}

		return key, until, err
	}
}

// directKeys - ключи у самого сервера (GET /_matrix/key/v2/server) через
// транспорт joiner: ответ проверяется transport.ParseServerKeys (server_name,
// самоподпись, old_verify_keys, срок не больше 7 дней). Ответ сервера
// запоминается на один join.
func (j *Joiner) directKeys(ctx context.Context) func(origin, keyID string) (string, int64, error) {
	fetched := map[string]transport.ServerKeys{}
	failed := map[string]error{}

	return func(origin, keyID string) (string, int64, error) {
		if err, ok := failed[origin]; ok {
			return "", 0, err
		}

		keys, ok := fetched[origin]
		if !ok {
			var err error

			keys, err = j.fetchServerKeys(ctx, origin)
			if err != nil {
				failed[origin] = err

				return "", 0, err
			}

			fetched[origin] = keys
		}

		key, until, found := keys.Lookup(keyID)
		if !found {
			return "", 0, fmt.Errorf("%w: %s %s", transport.ErrKeyNotFound, origin, keyID)
		}

		return key, until, nil
	}
}

// fetchServerKeys запрашивает и проверяет ключи сервера.
func (j *Joiner) fetchServerKeys(ctx context.Context, origin string) (transport.ServerKeys, error) {
	const path = "/_matrix/key/v2/server"

	var (
		body   []byte
		status int
		err    error
	)

	if client, ok := j.client.(largeSender); ok {
		body, status, err = client.Do(ctx, origin, http.MethodGet, path, nil,
			transport.RequestOptions{Unsigned: true, IgnoreBackoff: true})
	} else {
		body, status, err = j.client.Send(ctx, origin, http.MethodGet, path, nil)
	}

	if err != nil {
		return transport.ServerKeys{}, fmt.Errorf("join: keys of %s: %w", origin, err)
	}

	if status != http.StatusOK {
		return transport.ServerKeys{}, fmt.Errorf("join: keys of %s: HTTP %d", origin, status)
	}

	return transport.ParseServerKeys(body, origin, time.Now())
}
