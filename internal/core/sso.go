package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Вход через SSO: пользователь, подтвержденный провайдером OIDC,
// сопоставляется учетной записи по (auth_provider, external_id) - как
// user_external_ids Synapse; клиент получает одноразовый loginToken и обменивает его
// на сессию через POST /login m.login.token.

var (
	// ErrInvalidLoginToken - loginToken неизвестен, истек или уже использован.
	ErrInvalidLoginToken = errors.New("core: invalid login token")
	// ErrSSOMapping - из claims провайдера не получается localpart учетной записи.
	ErrSSOMapping = errors.New("core: sso user mapping failed")
	// ErrExternalIDInUse - внешний идентификатор уже привязан к другой учетке.
	ErrExternalIDInUse = errors.New("core: external id is already linked")
)

// LoginTokenTTL - срок жизни loginToken (как в Synapse: 2 минуты).
const LoginTokenTTL = 2 * time.Minute

// ssoMappingAttempts - сколько localpart с числовым суффиксом пробовать, если имя
// занято (_MAP_USERNAME_RETRIES Synapse).
const ssoMappingAttempts = 1000

// LoginToken - запись о loginToken; в хранилище лежит только хеш.
type LoginToken struct {
	TokenHash    []byte
	UserID       string
	AuthProvider string
	ExpiresAt    time.Time
}

// SSOStore - хранилище привязок внешних учеток и loginToken (internal/store).
type SSOStore interface {
	// UserIDByExternalID - учетная запись, привязанная к (provider, externalID);
	// ErrNotFound - привязки нет.
	UserIDByExternalID(ctx context.Context, provider, externalID string) (string, error)
	// AddExternalID привязывает внешний идентификатор; ErrExternalIDInUse - уже
	// привязан (к любой учетке).
	AddExternalID(ctx context.Context, provider, externalID, userID string) error
	// ExternalProviders - провайдеры, к которым привязана учетная запись.
	ExternalProviders(ctx context.Context, userID string) ([]string, error)
	// CreateLoginToken сохраняет loginToken.
	CreateLoginToken(ctx context.Context, token LoginToken) error
	// ConsumeLoginToken атомарно гасит неистекший и неиспользованный токен и
	// возвращает его запись; ErrNotFound - такого токена нет.
	ConsumeLoginToken(ctx context.Context, tokenHash []byte, now time.Time) (LoginToken, error)
}

// SSOIdentity - пользователь, подтвержденный провайдером SSO.
type SSOIdentity struct {
	// Provider - auth_provider (idp_id с префиксом oidc-), Subject - external_id.
	Provider string
	Subject  string
	// Localpart - кандидат localpart для попытки attempt (0 - без суффикса), как
	// map_user_attributes Synapse с failures.
	Localpart func(attempt int) (string, error)
	// DisplayName - имя из claims; пусто - localpart.
	DisplayName string
	// AllowExistingUsers - привязать к существующей учетке с тем же localpart.
	AllowExistingUsers bool
	// UpdateProfile - обновлять displayname при каждом входе.
	UpdateProfile bool
}

// SSO - доменный сервис входа через SSO поверх Auth.
type SSO struct {
	auth  *Auth
	store SSOStore
}

// NewSSO собирает сервис SSO.
func NewSSO(auth *Auth, store SSOStore) *SSO {
	return &SSO{auth: auth, store: store}
}

// ResolveUser находит учетную запись по привязке или создает ее при первом входе
// (как _complete_sso_login / _register_mapped_user Synapse). created - учетная
// запись создана этим входом.
func (s *SSO) ResolveUser(ctx context.Context, identity SSOIdentity) (user User, created bool, err error) {
	if identity.Provider == "" || identity.Subject == "" {
		return User{}, false, fmt.Errorf("%w: пустой провайдер или subject", ErrSSOMapping)
	}

	user, err = s.linkedUser(ctx, identity)
	if err == nil {
		s.afterLogin(ctx, user, identity, false)

		return user, false, nil
	}

	if !errors.Is(err, ErrNotFound) {
		return User{}, false, err
	}

	user, created, err = s.mapNewUser(ctx, identity)
	if err != nil {
		return User{}, false, err
	}

	if err := s.store.AddExternalID(ctx, identity.Provider, identity.Subject, user.UserID); err != nil {
		if !errors.Is(err, ErrExternalIDInUse) {
			return User{}, false, err
		}

		// Параллельный первый вход того же пользователя: привязка уже есть.
		user, err = s.linkedUser(ctx, identity)
		if err != nil {
			return User{}, false, err
		}

		created = false
	}

	if user.Deactivated {
		return User{}, false, ErrUserDeactivated
	}

	s.afterLogin(ctx, user, identity, created)

	return user, created, nil
}

// linkedUser - учетка по привязке; ErrNotFound - привязки нет.
func (s *SSO) linkedUser(ctx context.Context, identity SSOIdentity) (User, error) {
	userID, err := s.store.UserIDByExternalID(ctx, identity.Provider, identity.Subject)
	if err != nil {
		return User{}, err
	}

	user, err := s.auth.store.UserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return User{}, fmt.Errorf("core: external id %s/%s -> %s: учетной записи нет", identity.Provider, identity.Subject, userID)
		}

		return User{}, err
	}

	if user.Deactivated {
		return User{}, ErrUserDeactivated
	}

	return user, nil
}

// mapNewUser подбирает localpart и создает учетку без пароля (или находит
// существующую при allow_existing_users).
func (s *SSO) mapNewUser(ctx context.Context, identity SSOIdentity) (User, bool, error) {
	for attempt := range ssoMappingAttempts {
		candidate, err := identity.Localpart(attempt)
		if err != nil {
			return User{}, false, fmt.Errorf("%w: %w", ErrSSOMapping, err)
		}

		localpart, err := ValidateLocalpart(candidate)
		if err != nil {
			return User{}, false, fmt.Errorf("%w: localpart %q недопустим", ErrSSOMapping, candidate)
		}

		existing, err := s.auth.store.UserByLocalpart(ctx, localpart)
		switch {
		case err == nil && identity.AllowExistingUsers:
			if existing.Deactivated {
				return User{}, false, ErrUserDeactivated
			}

			return existing, false, nil
		case err == nil:
			continue
		case !errors.Is(err, ErrNotFound):
			return User{}, false, err
		}

		displayName := identity.DisplayName
		if displayName == "" {
			displayName = localpart
		}

		user, err := s.auth.store.CreateUser(ctx, User{
			UserID: s.auth.userID(localpart), Localpart: localpart, DisplayName: displayName,
		})
		if errors.Is(err, ErrUserInUse) {
			continue // имя заняли параллельно - следующий кандидат
		}

		if err != nil {
			return User{}, false, err
		}

		return user, true, nil
	}

	return User{}, false, fmt.Errorf("%w: свободный localpart не найден", ErrSSOMapping)
}

// afterLogin - синхронизация профиля после входа (общий хук с LDAP).
func (s *SSO) afterLogin(ctx context.Context, user User, identity SSOIdentity, created bool) {
	s.auth.opts.OnExternalLogin(ctx, user, ExternalIdentity{
		Provider:        identity.Provider,
		Localpart:       user.Localpart,
		DisplayName:     identity.DisplayName,
		SyncDisplayName: identity.UpdateProfile,
	}, created)
}

// IssueLoginToken выдает одноразовый loginToken для userID (после успешного SSO).
func (s *SSO) IssueLoginToken(ctx context.Context, userID, provider string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("core: login token: %w", err)
	}

	token := "syl_" + base64.RawURLEncoding.EncodeToString(buf)

	if err := s.store.CreateLoginToken(ctx, LoginToken{
		TokenHash:    HashToken(token),
		UserID:       userID,
		AuthProvider: provider,
		ExpiresAt:    s.auth.opts.Now().Add(LoginTokenTTL),
	}); err != nil {
		return "", err
	}

	return token, nil
}

// TokenLoginRequest - POST /login m.login.token.
type TokenLoginRequest struct {
	Token               string
	DeviceID            string
	DeviceDisplayName   string
	RequestRefreshToken bool
}

// LoginWithToken обменивает loginToken на сессию. Токен гасится до выдачи сессии:
// повтор того же токена - ErrInvalidLoginToken.
func (s *SSO) LoginWithToken(ctx context.Context, req TokenLoginRequest) (LoginResult, error) {
	if req.Token == "" {
		return LoginResult{}, ErrInvalidLoginToken
	}

	record, err := s.store.ConsumeLoginToken(ctx, HashToken(req.Token), s.auth.opts.Now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrInvalidLoginToken
		}

		return LoginResult{}, err
	}

	user, err := s.auth.store.UserByID(ctx, record.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return LoginResult{}, ErrInvalidLoginToken
		}

		return LoginResult{}, err
	}

	if user.Deactivated {
		return LoginResult{}, ErrUserDeactivated
	}

	return s.auth.issueSession(ctx, user, req.DeviceID, req.DeviceDisplayName, req.RequestRefreshToken)
}

// Providers - провайдеры SSO, к которым привязана учетная запись (стадия UIA m.login.sso).
func (s *SSO) Providers(ctx context.Context, userID string) ([]string, error) {
	return s.store.ExternalProviders(ctx, userID)
}

// UserIDForIdentity - учетная запись, привязанная к (provider, subject); пусто -
// привязки нет. Нужна UIA: SSO должен подтвердить того же пользователя.
func (s *SSO) UserIDForIdentity(ctx context.Context, provider, subject string) (string, error) {
	userID, err := s.store.UserIDByExternalID(ctx, provider, subject)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}

	return userID, err
}

// PasswordUsable сообщает, может ли пользователь подтвердить себя паролем в UIA:
// есть локальный хеш (и локальные пароли включены) или внешний провайдер пароля
// (LDAP). Учетка только SSO пароля не имеет - ей предлагается стадия m.login.sso.
func (a *Auth) PasswordUsable(ctx context.Context, userID string) (bool, error) {
	if len(a.opts.PasswordProviders) > 0 {
		return true, nil
	}

	if a.opts.DisableLocalPasswords {
		return false, nil
	}

	user, err := a.store.UserByID(ctx, userID)
	if err != nil {
		return false, err
	}

	return user.PasswordHash != "", nil
}
