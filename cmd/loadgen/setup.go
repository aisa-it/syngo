package main

import (
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 задан протоколом /_synapse/admin/v1/register
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// Способы получить пул пользователей.
const (
	// regSharedSecret - /_synapse/admin/v1/register (registration_shared_secret);
	// работает и у syngod, и у Synapse при закрытой регистрации, без UIA.
	regSharedSecret = "shared-secret"
	// regOpen - POST /register с UIA m.login.dummy (открытая регистрация стенда).
	regOpen = "open"
	// regLogin - пользователи уже есть (созданы заранее), только /login.
	regLogin = "login"
)

// cachedUser - учетная запись из кеша токенов.
type cachedUser struct {
	UserID       string `json:"user_id"`
	Token        string `json:"access_token"`
	DeviceID     string `json:"device_id"`
	KeysUploaded bool   `json:"keys_uploaded"`
}

// tokenCache - токены пула между прогонами (ключ - адрес сервера и localpart):
// повторный прогон не регистрирует и не логинит пользователей заново и не плодит
// устройства. Сброс базы стенда делает токены недействительными - их проверяет
// /whoami, и пользователь заводится снова.
type tokenCache struct {
	path string

	mu   sync.Mutex
	data map[string]map[string]cachedUser
}

func loadTokenCache(path string) (*tokenCache, error) {
	c := &tokenCache{path: path, data: map[string]map[string]cachedUser{}}
	if path == "" {
		return c, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // путь задает оператор
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}

	if err != nil {
		return nil, fmt.Errorf("кеш токенов: %w", err)
	}

	if err := json.Unmarshal(data, &c.data); err != nil {
		return nil, fmt.Errorf("кеш токенов %s: %w (удалите файл)", path, err)
	}

	return c, nil
}

func (c *tokenCache) get(server, localpart string) (cachedUser, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	u, ok := c.data[server][localpart]

	return u, ok
}

func (c *tokenCache) put(server, localpart string, u cachedUser) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data[server] == nil {
		c.data[server] = map[string]cachedUser{}
	}

	c.data[server][localpart] = u
}

// save пишет кеш (0600: в нем токены).
func (c *tokenCache) save() error {
	if c.path == "" {
		return nil
	}

	c.mu.Lock()
	data, err := json.MarshalIndent(c.data, "", " ") //nolint:gosec // кеш токенов и есть назначение файла (0600)
	c.mu.Unlock()

	if err != nil {
		return fmt.Errorf("кеш токенов: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o750); err != nil {
		return fmt.Errorf("кеш токенов: %w", err)
	}

	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return fmt.Errorf("кеш токенов: %w", err)
	}

	return nil
}

// setupOptions - как заводить пользователей.
type setupOptions struct {
	register     string
	sharedSecret string
	prefix       string
	password     string
}

// setupStats - итог подготовки.
type setupStats struct {
	Users         int     `json:"users"`
	Registered    int64   `json:"registered"`
	LoggedIn      int64   `json:"logged_in"`
	Cached        int64   `json:"cached"`
	RoomsCreated  int64   `json:"rooms_created"`
	RoomsReused   int64   `json:"rooms_reused"`
	Joins         int64   `json:"joins"`
	Memberships   int     `json:"memberships"`
	Seconds       float64 `json:"seconds"`
	LargestRoom   int     `json:"largest_room"`
	SmallestRoom  int     `json:"smallest_room"`
	EncryptedRoom int     `json:"encrypted_rooms"`
}

// withRetry повторяет подготовительный вызов при 429 (retry_after) и сетевых
// сбоях: подготовка должна дойти до конца и под прод-лимитами.
func withRetry(ctx context.Context, fn func() error) error {
	var err error

	for attempt := range 8 {
		err = fn()
		if err == nil {
			return nil
		}

		wait := time.Duration(attempt+1) * 500 * time.Millisecond

		var (
			ae     *apiError
			urlErr *url.Error
		)

		switch {
		case errors.As(err, &ae):
			if ae.Status != http.StatusTooManyRequests && ae.Status < 500 {
				return err
			}

			if ae.RetryAfter > 0 {
				wait = min(ae.RetryAfter+50*time.Millisecond, 60*time.Second)
			}
		case errors.As(err, &urlErr):
			// Сетевой сбой (обрыв, таймаут) - повторяем.
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	return err
}

// authResponse - ответ register/login.
type authResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

// sharedSecretMAC - mac для /_synapse/admin/v1/register (как register_new_matrix_user).
func sharedSecretMAC(secret, nonce, user, password string, admin bool) string {
	mac := hmac.New(sha1.New, []byte(secret))

	role := "notadmin"
	if admin {
		role = "admin"
	}

	mac.Write([]byte(nonce + "\x00" + user + "\x00" + password + "\x00" + role))

	return hex.EncodeToString(mac.Sum(nil))
}

// errUserInUse - пользователь уже зарегистрирован (дальше - /login).
var errUserInUse = errors.New("user in use")

func (e *engine) registerSharedSecret(ctx context.Context, localpart string) (authResponse, error) {
	var nonce struct {
		Nonce string `json:"nonce"`
	}

	if err := e.setupC.call(ctx, request{
		endpoint: "register_nonce", method: http.MethodGet, path: "/_synapse/admin/v1/register", out: &nonce,
	}); err != nil {
		return authResponse{}, err
	}

	var out authResponse

	err := e.setupC.call(ctx, request{
		endpoint: "register", method: http.MethodPost, path: "/_synapse/admin/v1/register",
		body: map[string]any{
			"nonce": nonce.Nonce, "username": localpart, "password": e.setup.password,
			"displayname": localpart, "admin": false,
			"mac": sharedSecretMAC(e.setup.sharedSecret, nonce.Nonce, localpart, e.setup.password, false),
		},
		out: &out,
	})

	return out, mapInUse(err)
}

func mapInUse(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.ErrCode == "M_USER_IN_USE" {
		return errUserInUse
	}

	return err
}

func (e *engine) registerOpen(ctx context.Context, localpart string) (authResponse, error) {
	body := map[string]any{
		"username": localpart, "password": e.setup.password,
		"initial_device_display_name": "loadgen",
	}

	var out authResponse

	err := e.setupC.call(ctx, request{
		endpoint: "register", method: http.MethodPost, path: "/_matrix/client/v3/register", body: body, out: &out,
	})
	if err == nil {
		return out, nil
	}

	// UIA: первый ответ 401 с session, второй - m.login.dummy в той же сессии.
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
		return authResponse{}, mapInUse(err)
	}

	var uia struct {
		Session string `json:"session"`
	}

	_ = json.Unmarshal(ae.Body, &uia)
	body["auth"] = map[string]any{"type": "m.login.dummy", "session": uia.Session}

	err = e.setupC.call(ctx, request{
		endpoint: "register", method: http.MethodPost, path: "/_matrix/client/v3/register", body: body, out: &out,
	})

	return out, mapInUse(err)
}

func (e *engine) login(ctx context.Context, localpart string) (authResponse, error) {
	var out authResponse

	err := e.setupC.call(ctx, request{
		endpoint: "login", method: http.MethodPost, path: "/_matrix/client/v3/login",
		body: map[string]any{
			"type":                        "m.login.password",
			"identifier":                  map[string]string{"type": "m.id.user", "user": localpart},
			"password":                    e.setup.password,
			"initial_device_display_name": "loadgen",
		},
		out: &out,
	})

	return out, err
}

// ensureUser - пользователь из кеша (проверен /whoami), регистрацией или логином.
func (e *engine) ensureUser(ctx context.Context, a *agent, st *setupStats) error {
	if u, ok := e.cache.get(e.target, a.localpart); ok {
		var who struct {
			UserID   string `json:"user_id"`
			DeviceID string `json:"device_id"`
		}

		err := withRetry(ctx, func() error {
			err := e.setupC.call(ctx, request{
				endpoint: "whoami", method: http.MethodGet, path: "/_matrix/client/v3/account/whoami",
				token: u.Token, out: &who,
			})

			return err
		})
		if err == nil && who.UserID == u.UserID {
			a.userID, a.token, a.deviceID = u.UserID, u.Token, u.DeviceID
			atomic.AddInt64(&st.Cached, 1)

			return nil
		}

		if statusOf(err) != http.StatusUnauthorized && err != nil {
			return fmt.Errorf("%s: whoami: %w", a.localpart, err)
		}
	}

	var (
		resp authResponse
		err  error
	)

	switch e.setup.register {
	case regSharedSecret, regOpen:
		err = withRetry(ctx, func() error {
			if e.setup.register == regOpen {
				resp, err = e.registerOpen(ctx, a.localpart)
			} else {
				resp, err = e.registerSharedSecret(ctx, a.localpart)
			}

			return err
		})
		if err == nil {
			atomic.AddInt64(&st.Registered, 1)

			break
		}

		if !errors.Is(err, errUserInUse) {
			return fmt.Errorf("%s: регистрация: %w", a.localpart, err)
		}

		fallthrough
	case regLogin:
		err = withRetry(ctx, func() error {
			resp, err = e.login(ctx, a.localpart)

			return err
		})
		if err != nil {
			return fmt.Errorf("%s: вход: %w", a.localpart, err)
		}

		atomic.AddInt64(&st.LoggedIn, 1)
	default:
		return fmt.Errorf("register %q: shared-secret, open или login", e.setup.register)
	}

	if resp.AccessToken == "" || resp.UserID == "" {
		return fmt.Errorf("%s: сервер не выдал токен", a.localpart)
	}

	a.userID, a.token, a.deviceID = resp.UserID, resp.AccessToken, resp.DeviceID
	e.cache.put(e.target, a.localpart, cachedUser{UserID: a.userID, Token: a.token, DeviceID: a.deviceID})

	return nil
}

// randomKey - фиктивный ключ (unpadded base64, как у Olm).
func randomKey(n int) string {
	buf := make([]byte, n)
	_, _ = crand.Read(buf)

	return base64.RawStdEncoding.EncodeToString(buf)
}

// oneTimeKeys - n новых signed_curve25519 с уникальными в пределах устройства id.
func (e *engine) oneTimeKeys(a *agent, n int) map[string]any {
	keys := make(map[string]any, n)

	for range n {
		id := fmt.Sprintf("signed_curve25519:%s%06d", e.runID, a.otk.Add(1))
		keys[id] = map[string]any{
			"key": randomKey(32),
			"signatures": map[string]any{
				a.userID: map[string]string{"ed25519:" + a.deviceID: randomKey(64)},
			},
		}
	}

	return keys
}

// uploadDeviceKeys - ключи устройства (один раз на устройство) и запас OTK.
func (e *engine) uploadDeviceKeys(ctx context.Context, a *agent) error {
	cached, _ := e.cache.get(e.target, a.localpart)
	a.curveKey = randomKey(32)

	body := map[string]any{"one_time_keys": e.oneTimeKeys(a, 20)}

	if !cached.KeysUploaded {
		body["device_keys"] = map[string]any{
			"user_id":    a.userID,
			"device_id":  a.deviceID,
			"algorithms": []string{"m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"},
			"keys": map[string]string{
				"curve25519:" + a.deviceID: a.curveKey,
				"ed25519:" + a.deviceID:    randomKey(32),
			},
			"signatures": map[string]any{
				a.userID: map[string]string{"ed25519:" + a.deviceID: randomKey(64)},
			},
		}
	}

	err := withRetry(ctx, func() error {
		err := e.setupC.call(ctx, request{
			endpoint: "keys_upload", method: http.MethodPost, path: "/_matrix/client/v3/keys/upload",
			token: a.token, body: body,
		})

		return err
	})
	if err != nil {
		return fmt.Errorf("%s: keys/upload: %w", a.localpart, err)
	}

	cached.KeysUploaded = true
	cached.UserID, cached.Token, cached.DeviceID = a.userID, a.token, a.deviceID
	e.cache.put(e.target, a.localpart, cached)

	return nil
}

// syncFilter - фильтр как у Element Web: lazy-load участников, timeline 20,
// без presence (Element с выключенным presence на сервере).
func (e *engine) syncFilter() map[string]any {
	room := map[string]any{
		"timeline": map[string]any{"limit": e.prof.TimelineLimit},
		"state":    map[string]any{"lazy_load_members": e.prof.LazyLoadMembers},
	}

	return map[string]any{
		"room":         room,
		"presence":     map[string]any{"not_types": []string{"*"}},
		"event_format": "client",
	}
}

func (e *engine) uploadFilter(ctx context.Context, a *agent) error {
	var out struct {
		FilterID string `json:"filter_id"`
	}

	err := withRetry(ctx, func() error {
		err := e.setupC.call(ctx, request{
			endpoint: "filter", method: http.MethodPost,
			path:  "/_matrix/client/v3/user/" + pathEscape(a.userID) + "/filter",
			token: a.token, body: e.syncFilter(), out: &out,
		})

		return err
	})
	if err != nil {
		return fmt.Errorf("%s: filter: %w", a.localpart, err)
	}

	a.filterID = out.FilterID

	return nil
}

// serverName - домен из user_id (часть после первого ':').
func serverName(userID string) string {
	_, host, _ := strings.Cut(userID, ":")

	return host
}

// ensureRoom - комната профиля по алиасу: есть - переиспользуем, нет - создаем.
func (e *engine) ensureRoom(ctx context.Context, idx int, creator *agent, encrypted bool, st *setupStats) (string, error) {
	aliasLocal := fmt.Sprintf("%s-room-%04d", e.setup.prefix, idx)
	alias := "#" + aliasLocal + ":" + serverName(creator.userID)

	var resolved struct {
		RoomID string `json:"room_id"`
	}

	err := withRetry(ctx, func() error {
		err := e.setupC.call(ctx, request{
			endpoint: "directory", method: http.MethodGet,
			path: "/_matrix/client/v3/directory/room/" + pathEscape(alias), token: creator.token, out: &resolved,
		})

		return err
	})
	if err == nil && resolved.RoomID != "" {
		atomic.AddInt64(&st.RoomsReused, 1)

		return resolved.RoomID, nil
	}

	if statusOf(err) != http.StatusNotFound {
		return "", fmt.Errorf("комната %s: %w", alias, err)
	}

	body := map[string]any{
		"preset": "public_chat", "room_alias_name": aliasLocal, "name": aliasLocal,
		"visibility": "private",
	}
	if encrypted {
		body["initial_state"] = []any{map[string]any{
			"type": "m.room.encryption", "state_key": "",
			"content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2"},
		}}
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	err = withRetry(ctx, func() error {
		err := e.setupC.call(ctx, request{
			endpoint: "create_room", method: http.MethodPost, path: "/_matrix/client/v3/createRoom",
			token: creator.token, body: body, out: &created,
		})

		return err
	})
	if err != nil {
		return "", fmt.Errorf("комната %s: createRoom: %w", alias, err)
	}

	atomic.AddInt64(&st.RoomsCreated, 1)

	return created.RoomID, nil
}

// runSetup готовит пул: пользователи, ключи, фильтры, комнаты, членства, медиа.
func (e *engine) runSetup(ctx context.Context) (setupStats, error) {
	started := time.Now()
	st := setupStats{Users: len(e.w.agents)}
	conc := e.prof.SetupConcurrency

	if err := forEach(ctx, conc, len(e.w.agents), func(i int) error {
		a := e.w.agents[i]
		if err := e.ensureUser(ctx, a, &st); err != nil {
			return err
		}

		if err := e.uploadDeviceKeys(ctx, a); err != nil {
			return err
		}

		return e.uploadFilter(ctx, a)
	}); err != nil {
		_ = e.cache.save()

		return st, fmt.Errorf("пользователи: %w", err)
	}

	if err := e.cache.save(); err != nil {
		return st, err
	}

	rng := rand.New(rand.NewPCG(uint64(e.prof.Seed), 0)) //nolint:gosec // зерно - не секрет
	encrypted := make([]bool, e.prof.Rooms)

	for r := range encrypted {
		encrypted[r] = rng.IntN(100) < e.prof.EncryptedRoomsPercent
		if encrypted[r] {
			st.EncryptedRoom++
		}
	}

	roomIDs := make([]string, e.prof.Rooms)

	if err := forEach(ctx, conc, e.prof.Rooms, func(r int) error {
		creator := e.w.agents[r%len(e.w.agents)]

		id, err := e.ensureRoom(ctx, r, creator, encrypted[r], &st)
		if err != nil {
			return err
		}

		roomIDs[r] = id

		return nil
	}); err != nil {
		return st, fmt.Errorf("комнаты: %w", err)
	}

	for r, id := range roomIDs {
		e.w.addRoom(id, encrypted[r], true)
	}

	assignment := assignRooms(rng, len(e.w.agents), e.prof.Rooms, e.prof.RoomsPerUser, e.prof.RoomSkew)

	type pair struct{ user, room int }

	var joins []pair

	for u, rooms := range assignment {
		for _, r := range rooms {
			joins = append(joins, pair{u, r})
		}
	}

	if err := forEach(ctx, conc, len(joins), func(i int) error {
		a, roomID := e.w.agents[joins[i].user], roomIDs[joins[i].room]

		err := withRetry(ctx, func() error {
			err := e.setupC.call(ctx, request{
				endpoint: "join", method: http.MethodPost, path: "/_matrix/client/v3/join/" + pathEscape(roomID),
				token: a.token, body: map[string]any{},
			})

			return err
		})
		if err != nil {
			return fmt.Errorf("%s -> %s: join: %w", a.localpart, roomID, err)
		}

		atomic.AddInt64(&st.Joins, 1)
		e.w.join(a, roomID)

		return nil
	}); err != nil {
		return st, fmt.Errorf("членства: %w", err)
	}

	// Создатель уже в комнате: join выше идемпотентен, но членство отмечаем явно.
	for r, id := range roomIDs {
		e.w.join(e.w.agents[r%len(e.w.agents)], id)
	}

	// Затравка медиа: скачивания с первых секунд, до первых media_upload.
	for i := range min(5, len(e.w.agents)) {
		if err := e.uploadMedia(ctx, e.setupC, e.w.agents[i]); err != nil {
			return st, fmt.Errorf("медиа: %w", err)
		}
	}

	st.SmallestRoom = len(e.w.agents)

	for _, id := range roomIDs {
		n := len(e.w.members(id, len(e.w.agents)))
		st.Memberships += n
		st.LargestRoom = max(st.LargestRoom, n)
		st.SmallestRoom = min(st.SmallestRoom, n)
	}

	st.Seconds = round3(time.Since(started).Seconds())

	return st, nil
}

// forEach - fn(0..n-1) с параллельностью conc; первая ошибка останавливает остальные.
func forEach(ctx context.Context, conc, n int, fn func(i int) error) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)

	for i := range n {
		if gctx.Err() != nil {
			break
		}

		g.Go(func() error { return fn(i) })
	}

	return g.Wait()
}
