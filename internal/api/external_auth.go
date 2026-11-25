package api

import (
	"context"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/ldapauth"
)

// passwordProviders собирает внешние провайдеры входа из auth.ldap.
// Ошибка конфига провайдера (например, нечитаемый bind_password_file) не роняет
// сервер, но пишется в журнал: локальные пароли продолжают работать.
func (s *Server) passwordProviders(cfg config.Config) []core.PasswordProvider {
	providers, err := ldapauth.FromConfig(cfg.Auth.LDAP)
	if err != nil {
		s.log.Error("auth: LDAP providers are not configured", "error", err)

		return nil
	}

	out := make([]core.PasswordProvider, 0, len(providers))

	for _, provider := range providers {
		s.log.Info("auth: password provider enabled", "provider", provider.Name())
		out = append(out, provider)
	}

	return out
}

// syncExternalProfile обновляет displayname из каталога после входа через провайдер
// и рассылает его по комнатам, как PUT /profile. Новая учетная запись
// уже создана с именем из каталога.
func (s *Server) syncExternalProfile(ctx context.Context, user core.User, identity core.ExternalIdentity, created bool) {
	s.log.InfoContext(ctx, "auth: external login", "provider", identity.Provider,
		"user_id", user.UserID, "created", created)

	if created || !identity.SyncDisplayName || identity.DisplayName == "" ||
		identity.DisplayName == user.DisplayName || s.profiles == nil {
		return
	}

	if err := s.profiles.UpdateDisplayName(ctx, user.UserID, identity.DisplayName); err != nil {
		s.log.WarnContext(ctx, "auth: displayname sync failed", "user_id", user.UserID, "error", err)

		return
	}

	s.propagateProfileAsync(ctx, user.UserID)
}
