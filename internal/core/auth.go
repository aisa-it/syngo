package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Ошибки аутентификации. Транспорт отображает их в коды Matrix
// (M_FORBIDDEN, M_UNKNOWN_TOKEN, M_USER_IN_USE, M_INVALID_USERNAME, M_WEAK_PASSWORD).
var (
	// ErrRegistrationDisabled — регистрация запрещена конфигурацией.
	ErrRegistrationDisabled = errors.New("core: registration is disabled")
	// ErrInvalidLocalpart — localpart не проходит валидацию спеки.
	ErrInvalidLocalpart = errors.New("core: invalid localpart")
	// ErrWeakPassword — пароль короче минимальной длины из конфигурации.
	ErrWeakPassword = errors.New("core: password is too weak")
	// ErrUserInUse — localpart уже занят.
	ErrUserInUse = errors.New("core: user already exists")
	// ErrInvalidCredentials — пользователь не найден либо пароль неверный.
	ErrInvalidCredentials = errors.New("core: invalid credentials")
	// ErrUnsupportedLogin — тип логина или идентификатора не поддерживается.
	ErrUnsupportedLogin = errors.New("core: unsupported login type")
	// ErrInvalidToken — токен отсутствует, отозван или истёк.
	ErrInvalidToken = errors.New("core: invalid access token")
	// ErrUserDeactivated — пользователь деактивирован.
	ErrUserDeactivated = errors.New("core: user is deactivated")
)

// localpartPattern — допустимые символы localpart по спеке CS-API:
// строчные латинские буквы, цифры и ._=-/+.
var localpartPattern = regexp.MustCompile(`^[a-z0-9._=\-/+]+$`)

// LocalpartMaxLength — предел длины localpart, который принимает спека.
const LocalpartMaxLength = 255

// User — учётная запись homeserver'а. Пароль хранится только как PHC-строка argon2id.
type User struct {
	UserID       string
	Localpart    string
	PasswordHash string
	DisplayName  string
	AvatarURL    string
	Deactivated  bool
	// IsAdmin — роль администратора сервера (admin-контур, ADR-0008).
	IsAdmin bool
}

// Device — устройство (клиентская сессия) пользователя.
type Device struct {
	UserID      string
	DeviceID    string
	DisplayName string
}

// TokenRecord — запись о выданном токене. В БД лежат только хеши.
type TokenRecord struct {
	TokenHash   []byte
	RefreshHash []byte
	UserID      string
	DeviceID    string
	ExpiresAt   *time.Time
	// Scope — назначение токена: "" (обычный CS-API), "admin" или "admin:read".
	Scope string
}

// Session — результат проверки access-токена: кто и с какого устройства.
type Session struct {
	UserID      string
	Localpart   string
	DisplayName string
	DeviceID    string
	Deactivated bool
	ExpiresAt   *time.Time
	// IssuedAt — когда выдана пара токенов (для срока жизни refresh-токена).
	IssuedAt time.Time
	// Scope — назначение токена (см. TokenRecord.Scope).
	Scope string
}

// PasswordHasher скрывает алгоритм хеширования пароля (argon2id).
type PasswordHasher interface {
	// Hash возвращает PHC-строку с солью и параметрами.
	Hash(password string) (string, error)
	// Verify сравнивает пароль с сохранённым хешем (argon2id или bcrypt).
	Verify(hash, password string) (bool, error)
}

// TokenSource выдаёт токены: открытое значение уходит клиенту, хеш — в БД.
type TokenSource interface {
	// NewToken возвращает открытый токен и его SHA-256.
	NewToken() (plain string, hash []byte, err error)
}

// AuthStore — минимальный контракт хранилища для аутентификации.
// Реализация: internal/store (pgx). Интерфейс объявлен здесь, чтобы домен не знал про SQL.
// PasswordProvider - внешняя проверка пароля: LDAP и аналоги
// password_providers/modules Synapse. Провайдеры опрашиваются по порядку до первого
// успеха, затем проверяется локальный хеш (как в Synapse: модули, потом
// password_config.localdb_enabled).
type PasswordProvider interface {
	// Name - имя провайдера для журнала.
	Name() string
	// CheckPassword проверяет пароль пользователя username (как его ввели, без "@" и
	// домена). ok=false без ошибки - пользователя у провайдера нет или пароль неверный;
	// ошибка - провайдер недоступен, тогда опрашивается следующий.
	CheckPassword(ctx context.Context, username, password string) (identity ExternalIdentity, ok bool, err error)
}

// ExternalIdentity - пользователь, подтвержденный внешним провайдером.
type ExternalIdentity struct {
	// Provider - имя провайдера.
	Provider string
	// Localpart - localpart учетной записи Matrix (приводится к нижнему регистру).
	Localpart string
	// DisplayName - имя из каталога; пусто - не менять.
	DisplayName string
	// Email - адрес из каталога (пока только для журнала).
	Email string
	// SyncDisplayName - обновлять имя существующей учетной записи при входе.
	SyncDisplayName bool
}

// Rehasher — необязательная возможность хешера: сообщить, что хеш устарел
// (например, bcrypt из Synapse) и его надо заменить при успешном входе.
type Rehasher interface {
	NeedsRehash(encodedHash string) bool
}

type AuthStore interface {
	CreateUser(ctx context.Context, user User) (User, error)
	// UpdatePasswordHash заменяет хеш пароля (прозрачная миграция bcrypt → argon2id).
	UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error
	UserByLocalpart(ctx context.Context, localpart string) (User, error)
	UserByID(ctx context.Context, userID string) (User, error)
	UpsertDevice(ctx context.Context, device Device) error
	CreateToken(ctx context.Context, token TokenRecord) error
	SessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error)
	RevokeToken(ctx context.Context, tokenHash []byte) error
	// SessionByRefreshHash — сессия по хешу refresh-token.
	SessionByRefreshHash(ctx context.Context, refreshHash []byte) (Session, error)
	// RevokeRefresh отзывает строку токена по хешу refresh-token.
	RevokeRefresh(ctx context.Context, refreshHash []byte) error
	RevokeAllTokens(ctx context.Context, userID string) (int64, error)
	// DeactivateUser помечает учётную запись деактивированной.
	DeactivateUser(ctx context.Context, userID string) error
	// SearchUsers ищет локальных пользователей по localpart/display_name
	// (user_directory).
	SearchUsers(ctx context.Context, term string, limit int) ([]User, error)
}

// AuthOptions — параметры сервиса аутентификации.
type AuthOptions struct {
	ServerName        string
	RegistrationOpen  bool
	MinPasswordLength int
	RefreshTokens     bool
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	// Now подменяется в тестах; по умолчанию time.Now.
	Now func() time.Time
	// OnPasswordRehash вызывается после попытки пере-хеширования пароля
	// (err == nil — успешно переписали bcrypt в argon2id).
	OnPasswordRehash func(userID string, err error)
	// PasswordProviders - внешние проверки пароля (LDAP), опрашиваются до локальной базы.
	PasswordProviders []PasswordProvider
	// DisableLocalPasswords - не проверять локальные хеши (password_config.localdb_enabled: false).
	DisableLocalPasswords bool
	// OnProviderError - провайдер вернул ошибку (недоступен); вход пробует следующий.
	OnProviderError func(provider string, err error)
	// OnExternalLogin вызывается после входа через провайдер (синхронизация профиля);
	// created - учетная запись создана этим входом.
	OnExternalLogin func(ctx context.Context, user User, identity ExternalIdentity, created bool)
}

// Auth — доменный сервис аутентификации: регистрация, логин, проверка токена, выход.
type Auth struct {
	store  AuthStore
	hasher PasswordHasher
	tokens TokenSource
	opts   AuthOptions
}

// NewAuth собирает сервис аутентификации.
func NewAuth(store AuthStore, hasher PasswordHasher, tokens TokenSource, opts AuthOptions) *Auth {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.OnPasswordRehash == nil {
		opts.OnPasswordRehash = func(string, error) {}
	}

	if opts.MinPasswordLength <= 0 {
		opts.MinPasswordLength = 8
	}

	if opts.OnProviderError == nil {
		opts.OnProviderError = func(string, error) {}
	}

	if opts.OnExternalLogin == nil {
		opts.OnExternalLogin = func(context.Context, User, ExternalIdentity, bool) {}
	}

	return &Auth{store: store, hasher: hasher, tokens: tokens, opts: opts}
}

// RegisterRequest — параметры POST /register (без UIA: регистрация открыта конфигом).
type RegisterRequest struct {
	Localpart           string
	Password            string
	DisplayName         string
	DeviceID            string
	DeviceDisplayName   string
	InhibitLogin        bool
	RequestRefreshToken bool
	// Operator - регистрацию выполняет оператор через registration_shared_secret:
	// разрешена и при закрытой регистрации.
	Operator bool
	// Admin - создать пользователя администратором сервера (только для Operator).
	Admin bool
}

// LoginRequest — параметры POST /login (тип m.login.password, идентификатор — user).
type LoginRequest struct {
	Identifier          string
	Password            string
	DeviceID            string
	DeviceDisplayName   string
	RequestRefreshToken bool
}

// LoginResult — выданная сессия. RefreshToken пустой, если ротация не включена.
type LoginResult struct {
	UserID       string
	DeviceID     string
	AccessToken  string
	RefreshToken string
	// ExpiresInMS — срок жизни access-токена в миллисекундах; nil — бессрочный.
	ExpiresInMS *int64
}

// RegistrationAllowed сообщает, разрешена ли регистрация конфигурацией.
func (a *Auth) RegistrationAllowed() bool {
	return a.opts.RegistrationOpen
}

// Register создаёт пользователя и (если не InhibitLogin) сразу выдаёт сессию.
func (a *Auth) Register(ctx context.Context, req RegisterRequest) (LoginResult, error) {
	if !a.opts.RegistrationOpen && !req.Operator {
		return LoginResult{}, ErrRegistrationDisabled
	}

	localpart, err := ValidateLocalpart(req.Localpart)
	if err != nil {
		return LoginResult{}, err
	}

	if len(req.Password) < a.opts.MinPasswordLength {
		return LoginResult{}, ErrWeakPassword
	}

	hash, err := a.hasher.Hash(req.Password)
	if err != nil {
		return LoginResult{}, fmt.Errorf("core: hash password: %w", err)
	}

	// displayname по умолчанию - localpart, как в Synapse (register_user): иначе
	// m.room.member и joined_members идут без имени (Complement
	// TestRoomState).
	displayName := req.DisplayName
	if displayName == "" {
		displayName = localpart
	}

	user := User{
		UserID:       a.userID(localpart),
		Localpart:    localpart,
		PasswordHash: hash,
		DisplayName:  displayName,
		IsAdmin:      req.Operator && req.Admin,
	}

	user, err = a.store.CreateUser(ctx, user)
	if err != nil {
		return LoginResult{}, err
	}

	if req.InhibitLogin {
		return LoginResult{UserID: user.UserID}, nil
	}

	return a.issueSession(ctx, user, req.DeviceID, req.DeviceDisplayName, req.RequestRefreshToken)
}

// Login проверяет пароль и выдаёт новую сессию (устройство создаётся или обновляется).
func (a *Auth) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	user, err := a.authenticate(ctx, req.Identifier, req.Password)
	if err != nil {
		return LoginResult{}, err
	}

	return a.issueSession(ctx, user, req.DeviceID, req.DeviceDisplayName, req.RequestRefreshToken)
}

// authenticate проверяет пару логин/пароль: внешние провайдеры по порядку, затем
// локальный хеш. Пользователь без локального пароля (LDAP, SSO, пустой хеш из
// Synapse) получает тот же отказ, что и при неверном пароле: раньше пустой хеш
// давал 500 и раскрывал существование учетной записи.
func (a *Auth) authenticate(ctx context.Context, identifier, password string) (User, error) {
	username, err := a.usernameFromIdentifier(identifier)
	if err != nil {
		return User{}, err
	}

	if password == "" {
		return User{}, ErrInvalidCredentials
	}

	for _, provider := range a.opts.PasswordProviders {
		identity, ok, err := provider.CheckPassword(ctx, username, password)
		if err != nil {
			a.opts.OnProviderError(provider.Name(), err)

			continue
		}

		if ok {
			if identity.Provider == "" {
				identity.Provider = provider.Name()
			}

			return a.externalUser(ctx, identity)
		}
	}

	if a.opts.DisableLocalPasswords {
		return User{}, ErrInvalidCredentials
	}

	user, err := a.store.UserByLocalpart(ctx, strings.ToLower(username))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return User{}, ErrInvalidCredentials
		}

		return User{}, err
	}

	if user.Deactivated {
		return User{}, ErrUserDeactivated
	}

	if err := a.verifyLocalPassword(user, password); err != nil {
		return User{}, err
	}

	a.rehashPassword(ctx, user, password)

	return user, nil
}

// verifyLocalPassword сверяет пароль с локальным хешем пользователя.
func (a *Auth) verifyLocalPassword(user User, password string) error {
	if user.PasswordHash == "" {
		return ErrInvalidCredentials
	}

	ok, err := a.hasher.Verify(user.PasswordHash, password)
	if err != nil {
		return fmt.Errorf("core: verify password: %w", err)
	}

	if !ok {
		return ErrInvalidCredentials
	}

	return nil
}

// externalUser находит или создает (первый вход) учетную запись пользователя,
// подтвержденного провайдером, как synapse-ldap3: localpart в нижнем регистре,
// без локального пароля, имя - из каталога.
func (a *Auth) externalUser(ctx context.Context, identity ExternalIdentity) (User, error) {
	localpart, err := ValidateLocalpart(identity.Localpart)
	if err != nil {
		return User{}, fmt.Errorf("core: provider %s: %w", identity.Provider, ErrInvalidCredentials)
	}

	identity.Localpart = localpart
	created := false

	user, err := a.store.UserByLocalpart(ctx, localpart)
	if errors.Is(err, ErrNotFound) {
		displayName := identity.DisplayName
		if displayName == "" {
			displayName = localpart
		}

		user, err = a.store.CreateUser(ctx, User{
			UserID: a.userID(localpart), Localpart: localpart, DisplayName: displayName,
		})
		if errors.Is(err, ErrUserInUse) {
			// Параллельный первый вход того же пользователя.
			user, err = a.store.UserByLocalpart(ctx, localpart)
		} else {
			created = err == nil
		}
	}

	if err != nil {
		return User{}, err
	}

	if user.Deactivated {
		return User{}, ErrUserDeactivated
	}

	a.opts.OnExternalLogin(ctx, user, identity, created)

	return user, nil
}

// rehashPassword прозрачно переносит пароль в актуальный формат (bcrypt → argon2id).
//
// Ошибки не ломают вход: пользователь уже аутентифицирован, просто в следующий раз
// попробуем ещё раз. Отсутствие Rehasher у хешера — тоже норма (тесты, кастомные хешеры).
func (a *Auth) rehashPassword(ctx context.Context, user User, password string) {
	rehasher, ok := a.hasher.(Rehasher)
	if !ok || !rehasher.NeedsRehash(user.PasswordHash) {
		return
	}

	newHash, err := a.hasher.Hash(password)
	if err != nil {
		a.opts.OnPasswordRehash(user.UserID, err)

		return
	}

	if err := a.store.UpdatePasswordHash(ctx, user.UserID, newHash); err != nil {
		a.opts.OnPasswordRehash(user.UserID, err)

		return
	}

	a.opts.OnPasswordRehash(user.UserID, nil)
}

// WhoAmI проверяет access-токен и возвращает сессию.
func (a *Auth) WhoAmI(ctx context.Context, accessToken string) (Session, error) {
	if accessToken == "" {
		return Session{}, ErrInvalidToken
	}

	session, err := a.store.SessionByTokenHash(ctx, HashToken(accessToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Session{}, ErrInvalidToken
		}

		return Session{}, err
	}

	if session.Deactivated {
		return Session{}, ErrUserDeactivated
	}

	if session.ExpiresAt != nil && !session.ExpiresAt.After(a.opts.Now()) {
		return Session{}, ErrInvalidToken
	}

	return session, nil
}

// Logout отзывает один токен.
func (a *Auth) Logout(ctx context.Context, accessToken string) error {
	if accessToken == "" {
		return ErrInvalidToken
	}

	return a.store.RevokeToken(ctx, HashToken(accessToken))
}

// LogoutAll отзывает все токены пользователя и возвращает их число.
func (a *Auth) LogoutAll(ctx context.Context, userID string) (int64, error) {
	return a.store.RevokeAllTokens(ctx, userID)
}

// issueSession создаёт устройство, токены и возвращает результат логина.
func (a *Auth) issueSession(
	ctx context.Context,
	user User,
	deviceID, deviceDisplayName string,
	requestRefresh bool,
) (LoginResult, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		deviceID = newDeviceID()
	}

	if err := a.store.UpsertDevice(ctx, Device{
		UserID:      user.UserID,
		DeviceID:    deviceID,
		DisplayName: deviceDisplayName,
	}); err != nil {
		return LoginResult{}, err
	}

	accessToken, accessHash, err := a.tokens.NewToken()
	if err != nil {
		return LoginResult{}, fmt.Errorf("core: generate access token: %w", err)
	}

	record := TokenRecord{
		TokenHash: accessHash,
		UserID:    user.UserID,
		DeviceID:  deviceID,
	}

	result := LoginResult{
		UserID:      user.UserID,
		DeviceID:    deviceID,
		AccessToken: accessToken,
	}

	if ttl := a.opts.AccessTokenTTL; ttl > 0 {
		expires := a.opts.Now().Add(ttl)
		record.ExpiresAt = &expires
		ms := ttl.Milliseconds()
		result.ExpiresInMS = &ms
	}

	if a.opts.RefreshTokens && requestRefresh {
		refreshToken, refreshHash, refreshErr := a.tokens.NewToken()
		if refreshErr != nil {
			return LoginResult{}, fmt.Errorf("core: generate refresh token: %w", refreshErr)
		}

		record.RefreshHash = refreshHash
		result.RefreshToken = refreshToken
	}

	if err := a.store.CreateToken(ctx, record); err != nil {
		return LoginResult{}, err
	}

	return result, nil
}

// userID собирает полный Matrix user_id.
func (a *Auth) userID(localpart string) string {
	return "@" + localpart + ":" + a.opts.ServerName
}

// usernameFromIdentifier принимает localpart или полный user_id локального сервера и
// возвращает имя как его ввели (без "@" и домена). Регистр не меняется: внешний
// провайдер получает имя как есть, локальный поиск регистронезависим (как в Synapse).
func (a *Auth) usernameFromIdentifier(identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", ErrInvalidCredentials
	}

	if !strings.HasPrefix(identifier, "@") {
		return identifier, nil
	}

	localpart, serverName, found := strings.Cut(strings.TrimPrefix(identifier, "@"), ":")
	if !found || localpart == "" || !strings.EqualFold(serverName, a.opts.ServerName) {
		return "", ErrInvalidCredentials
	}

	return localpart, nil
}

// LoginLimitKey - ключ лимита неудачных входов по учетной записи
// (rate_limit.login_account). "alice", "Alice", " alice" и
// "@alice:<server_name>" - одна учетка и одно ведро: иначе лимит обходится сменой
// регистра или формы идентификатора. Чужой или некорректный идентификатор
// входит в ключ как есть (без пробелов, в нижнем регистре). Пустая строка - не
// по чему ограничивать.
func (a *Auth) LoginLimitKey(identifier string) string {
	username, err := a.usernameFromIdentifier(identifier)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(identifier))
	}

	return strings.ToLower(username)
}

// ValidateLocalpart проверяет localpart по правилам спеки и возвращает его в нижнем регистре.
func ValidateLocalpart(localpart string) (string, error) {
	localpart = strings.ToLower(strings.TrimSpace(localpart))

	if localpart == "" || len(localpart) > LocalpartMaxLength {
		return "", ErrInvalidLocalpart
	}

	if !localpartPattern.MatchString(localpart) {
		return "", ErrInvalidLocalpart
	}

	return localpart, nil
}

// HashToken считает SHA-256 от открытого токена: в БД попадает только он.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))

	return sum[:]
}

// newDeviceID генерирует device_id в стиле Matrix: 10 символов верхнего регистра.
func newDeviceID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand в Go не возвращает ошибок на практике; при сбое ID всё равно нужен.
		return "DEVICE0000"
	}

	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}

	return string(buf)
}

// SearchUsers ищет локальных пользователей по localpart или display_name
// (GET /user_directory/search).
func (a *Auth) SearchUsers(ctx context.Context, term string, limit int) ([]User, error) {
	return a.store.SearchUsers(ctx, term, limit)
}

// Available сообщает, свободен ли localpart (для GET /register/available).
func (a *Auth) Available(ctx context.Context, localpart string) (bool, error) {
	normalized, err := ValidateLocalpart(localpart)
	if err != nil {
		return false, err
	}

	_, err = a.store.UserByLocalpart(ctx, normalized)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}

		return false, err
	}

	return false, nil
}

// ChangePassword меняет пароль после UIA-проверки.
// logoutDevices отзывает все access/refresh-токены пользователя.
func (a *Auth) ChangePassword(ctx context.Context, userID, newPassword string, logoutDevices bool) error {
	if len([]rune(newPassword)) < a.opts.MinPasswordLength {
		return ErrWeakPassword
	}

	user, err := a.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}

	if user.Deactivated {
		return ErrUserDeactivated
	}

	hash, err := a.hasher.Hash(newPassword)
	if err != nil {
		return fmt.Errorf("core: hash password: %w", err)
	}

	if err := a.store.UpdatePasswordHash(ctx, userID, hash); err != nil {
		return err
	}

	if logoutDevices {
		if _, err := a.store.RevokeAllTokens(ctx, userID); err != nil {
			return err
		}
	}

	return nil
}

// DeactivateAccount деактивирует учётную запись и отзывает все токены.
func (a *Auth) DeactivateAccount(ctx context.Context, userID string) error {
	if err := a.store.DeactivateUser(ctx, userID); err != nil {
		return err
	}

	if _, err := a.store.RevokeAllTokens(ctx, userID); err != nil {
		return err
	}

	return nil
}

// VerifyPassword проверяет пароль пользователя (для UIA-шагов вроде /delete_devices).
// Пользователь LDAP подтверждает пароль через провайдер.
func (a *Auth) VerifyPassword(ctx context.Context, userID, password string) error {
	user, err := a.store.UserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrInvalidCredentials
		}

		return err
	}

	if user.Deactivated {
		return ErrUserDeactivated
	}

	if password == "" {
		return ErrInvalidCredentials
	}

	for _, provider := range a.opts.PasswordProviders {
		identity, ok, err := provider.CheckPassword(ctx, user.Localpart, password)
		if err != nil {
			a.opts.OnProviderError(provider.Name(), err)

			continue
		}

		if ok && strings.EqualFold(identity.Localpart, user.Localpart) {
			return nil
		}
	}

	if a.opts.DisableLocalPasswords {
		return ErrInvalidCredentials
	}

	return a.verifyLocalPassword(user, password)
}

// Refresh обменивает refresh-token на новый access-token (старый отзывается).
func (a *Auth) Refresh(ctx context.Context, refreshToken string) (LoginResult, error) {
	if !a.opts.RefreshTokens {
		return LoginResult{}, fmt.Errorf("core: refresh tokens are disabled: %w", ErrUnsupportedLogin)
	}

	if refreshToken == "" {
		return LoginResult{}, ErrInvalidToken
	}

	session, err := a.store.SessionByRefreshHash(ctx, HashToken(refreshToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrInvalidToken
		}

		return LoginResult{}, err
	}

	if session.Deactivated {
		return LoginResult{}, ErrUserDeactivated
	}

	// Срок жизни refresh-токена (auth.refresh_token_ttl): раньше опция не
	// применялась, и refresh-токены были бессрочными.
	if a.opts.RefreshTokenTTL > 0 && !session.IssuedAt.IsZero() && time.Since(session.IssuedAt) > a.opts.RefreshTokenTTL {
		return LoginResult{}, ErrInvalidToken
	}

	// Инвалидируем старую пару access+refresh: оба хеша в одной строке access_tokens.
	if err := a.store.RevokeRefresh(ctx, HashToken(refreshToken)); err != nil {
		return LoginResult{}, err
	}

	user, err := a.store.UserByID(ctx, session.UserID)
	if err != nil {
		return LoginResult{}, err
	}

	return a.issueSession(ctx, user, session.DeviceID, "", true)
}

// AdminDeviceID - служебное устройство admin-токенов. В списке устройств
// пользователя не показывается, пока на нем нет обычных токенов.
const AdminDeviceID = "ADMIN"

// IssueToken выдаёт токен с явным scope и TTL (admin-контур, ADR-0008).
// В отличие от Login не проверяет пароль: вызывающий уже аутентифицировал клиента.
func (a *Auth) IssueToken(
	ctx context.Context, user User, deviceID, deviceDisplayName, scope string, ttl time.Duration,
) (LoginResult, error) {
	existing, err := a.store.UserByID(ctx, user.UserID)
	if err == nil {
		user = existing
	}

	if user.Deactivated {
		return LoginResult{}, ErrUserDeactivated
	}

	if deviceID == "" {
		deviceID = AdminDeviceID
	}

	if err := a.store.UpsertDevice(ctx, Device{UserID: user.UserID, DeviceID: deviceID, DisplayName: deviceDisplayName}); err != nil {
		return LoginResult{}, err
	}

	accessToken, accessHash, err := a.tokens.NewToken()
	if err != nil {
		return LoginResult{}, fmt.Errorf("core: generate access token: %w", err)
	}

	record := TokenRecord{
		TokenHash: accessHash,
		UserID:    user.UserID,
		DeviceID:  deviceID,
		Scope:     scope,
	}

	result := LoginResult{UserID: user.UserID, DeviceID: deviceID, AccessToken: accessToken}

	if ttl > 0 {
		expires := a.opts.Now().Add(ttl)
		record.ExpiresAt = &expires

		ms := ttl.Milliseconds()
		result.ExpiresInMS = &ms
	}

	if err := a.store.CreateToken(ctx, record); err != nil {
		return LoginResult{}, err
	}

	return result, nil
}

// VerifyCredentials проверяет пару логин/пароль и возвращает пользователя, НЕ создавая
// сессию и токен. Нужен admin-контуру: логин админа не должен порождать пользовательский
// токен (ADR-0008 п.2).
func (a *Auth) VerifyCredentials(ctx context.Context, identifier, password string) (User, error) {
	return a.authenticate(ctx, identifier, password)
}

// UserByID - учетная запись по user_id (is_admin нужен admin API).
func (a *Auth) UserByID(ctx context.Context, userID string) (User, error) {
	return a.store.UserByID(ctx, userID)
}

// UserByLogin - учетная запись по логину admin-входа без выдачи токена: user_id
// (начинается с @) или localpart как есть.
func (a *Auth) UserByLogin(ctx context.Context, login string) (User, error) {
	if login != "" && login[0] == '@' {
		return a.store.UserByID(ctx, login)
	}

	return a.store.UserByLocalpart(ctx, login)
}

// HashPassword считает argon2id-хеш для внешнего сброса пароля (admin API):
// валидация длины та же, что при регистрации.
func (a *Auth) HashPassword(password string) (string, error) {
	if len([]rune(password)) < a.opts.MinPasswordLength {
		return "", ErrWeakPassword
	}

	return a.hasher.Hash(password)
}
