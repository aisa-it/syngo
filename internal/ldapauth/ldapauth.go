// Package ldapauth - вход по паролю через LDAP, совместимый с модулем
// synapse-ldap3 (ldap_auth_provider.LdapAuthProviderModule), с которым работают
// инсталляции Synapse: те же поля конфига (uri, start_tls, base, attributes, bind_dn,
// bind_password, filter) и те же два режима.
//
//   - simple (bind_dn не задан): DN пользователя - "<uid>=<имя>,<base>", вход - bind
//     этим DN с паролем пользователя;
//   - search (bind_dn задан): bind служебной учеткой, поиск в base по
//     (&(<uid>=<имя>)<filter>), ровно одна запись, затем bind ее DN с паролем
//     пользователя.
//
// Провайдер реализует core.PasswordProvider: учетная запись Matrix создается при первом
// входе (localpart - имя в нижнем регистре), имя берется из атрибута name.
// Не поддержано (отложено): active_directory и default_domain, вход по mail.
package ldapauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
)

// defaultTimeout - соединение и одна операция LDAP, если timeout не задан.
const defaultTimeout = 10 * time.Second

// Conn - операции LDAP, нужные провайдеру (подменяется в тестах).
type Conn interface {
	StartTLS(config *tls.Config) error
	Bind(username, password string) error
	Search(request *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// Dialer открывает соединение с одним сервером.
type Dialer func(ctx context.Context, uri string, tlsConfig *tls.Config, timeout time.Duration) (Conn, error)

// Provider - один каталог LDAP.
type Provider struct {
	name         string
	uris         []string
	startTLS     bool
	base         string
	uidAttr      string
	mailAttr     string
	nameAttr     string
	bindDN       string
	bindPassword string
	filter       string
	timeout      time.Duration
	syncName     bool
	tlsConfig    *tls.Config
	dial         Dialer
}

var _ core.PasswordProvider = (*Provider)(nil)

// New собирает провайдер по конфигу; dial == nil - настоящий LDAP.
func New(cfg config.LDAPProvider, dial Dialer) (*Provider, error) {
	if len(cfg.URI) == 0 || cfg.Base == "" || cfg.Attributes.UID == "" {
		return nil, errors.New("ldapauth: uri, base and attributes.uid are required")
	}

	bindPassword := cfg.BindPassword

	if cfg.BindPasswordFile != "" {
		raw, err := os.ReadFile(cfg.BindPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("ldapauth: read bind_password_file: %w", err)
		}

		bindPassword = strings.TrimRight(string(raw), "\r\n")
	}

	tlsConfig, err := tlsConfigFor(cfg)
	if err != nil {
		return nil, err
	}

	name := cfg.Name
	if name == "" {
		name = "ldap " + strings.Join(cfg.URI, ",")
	}

	timeout := cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if dial == nil {
		dial = dialLDAP
	}

	return &Provider{
		name: name, uris: cfg.URI, startTLS: cfg.StartTLS, base: cfg.Base,
		uidAttr: cfg.Attributes.UID, mailAttr: cfg.Attributes.Mail, nameAttr: cfg.Attributes.Name,
		bindDN: cfg.BindDN, bindPassword: bindPassword, filter: cfg.Filter,
		timeout: timeout, syncName: cfg.SyncsDisplayName(), tlsConfig: tlsConfig, dial: dial,
	}, nil
}

// tlsConfigFor - TLS для ldaps:// и start_tls.
func tlsConfigFor(cfg config.LDAPProvider) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.TLSInsecureSkipVerify {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // явная настройка стенда
	}

	if cfg.TLSCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("ldapauth: read tls_ca_file: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ldapauth: tls_ca_file has no certificates")
		}

		tlsConfig.RootCAs = pool
	}

	return tlsConfig, nil
}

// dialLDAP - настоящее соединение go-ldap.
func dialLDAP(_ context.Context, uri string, tlsConfig *tls.Config, timeout time.Duration) (Conn, error) {
	conn, err := ldap.DialURL(uri,
		ldap.DialWithDialer(&net.Dialer{Timeout: timeout}), ldap.DialWithTLSConfig(tlsConfig))
	if err != nil {
		return nil, err
	}

	conn.SetTimeout(timeout)

	return conn, nil
}

// Name реализует core.PasswordProvider.
func (p *Provider) Name() string {
	return p.name
}

// CheckPassword реализует core.PasswordProvider.
func (p *Provider) CheckPassword(ctx context.Context, username, password string) (core.ExternalIdentity, bool, error) {
	// Пустой пароль - анонимный bind, который многие серверы считают успешным.
	if password == "" || username == "" {
		return core.ExternalIdentity{}, false, nil
	}

	conn, err := p.connect(ctx)
	if err != nil {
		return core.ExternalIdentity{}, false, err
	}

	defer func() { _ = conn.Close() }()

	var entry *ldap.Entry

	if p.bindDN == "" {
		entry, err = p.simpleBind(conn, username, password)
	} else {
		entry, err = p.searchBind(conn, username, password)
	}

	if err != nil || entry == nil {
		return core.ExternalIdentity{}, false, err
	}

	identity := core.ExternalIdentity{
		Provider:        p.name,
		Localpart:       strings.ToLower(username),
		SyncDisplayName: p.syncName,
	}

	if p.nameAttr != "" {
		identity.DisplayName = entry.GetAttributeValue(p.nameAttr)
	}

	if p.mailAttr != "" {
		identity.Email = entry.GetAttributeValue(p.mailAttr)
	}

	return identity, true, nil
}

// connect открывает соединение с первым доступным сервером (как пул ldap3 FIRST).
func (p *Provider) connect(ctx context.Context) (Conn, error) {
	var errs []error

	for _, uri := range p.uris {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		conn, err := p.dial(ctx, uri, p.tlsConfig, p.timeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", uri, err))

			continue
		}

		if p.startTLS && !strings.HasPrefix(uri, "ldaps://") {
			if err := conn.StartTLS(p.tlsConfig); err != nil {
				_ = conn.Close()

				errs = append(errs, fmt.Errorf("%s: start_tls: %w", uri, err))

				continue
			}
		}

		return conn, nil
	}

	return nil, fmt.Errorf("ldapauth: no LDAP server available: %w", errors.Join(errs...))
}

// simpleBind - режим simple: bind DN, собранным из uid и base. Атрибуты читаются
// из собственной записи (если права позволяют), иначе имя не меняется.
func (p *Provider) simpleBind(conn Conn, username, password string) (*ldap.Entry, error) {
	dn := fmt.Sprintf("%s=%s,%s", p.uidAttr, ldap.EscapeDN(username), p.base)

	if err := conn.Bind(dn, password); err != nil {
		return nil, bindError(err)
	}

	result, err := conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, int(p.timeout.Seconds()), false, "(objectClass=*)", p.attributes(), nil))
	if err != nil || len(result.Entries) != 1 {
		return &ldap.Entry{DN: dn}, nil //nolint:nilerr // атрибуты необязательны, вход уже подтвержден
	}

	return result.Entries[0], nil
}

// searchBind - режим search: поиск записи служебной учеткой, затем bind ее DN.
func (p *Provider) searchBind(conn Conn, username, password string) (*ldap.Entry, error) {
	if err := conn.Bind(p.bindDN, p.bindPassword); err != nil {
		return nil, fmt.Errorf("ldapauth: bind %s: %w", p.bindDN, err)
	}

	filter := fmt.Sprintf("(%s=%s)", p.uidAttr, ldap.EscapeFilter(username))
	if p.filter != "" {
		filter = "(&" + filter + p.filter + ")"
	}

	result, err := conn.Search(ldap.NewSearchRequest(p.base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		2, int(p.timeout.Seconds()), false, filter, p.attributes(), nil))
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return nil, nil // несколько записей - как synapse-ldap3, вход отклоняется
		}

		return nil, fmt.Errorf("ldapauth: search %s: %w", filter, err)
	}

	// Ноль или несколько записей: пользователя нет или он неоднозначен.
	if len(result.Entries) != 1 {
		return nil, nil
	}

	entry := result.Entries[0]

	if err := conn.Bind(entry.DN, password); err != nil {
		return nil, bindError(err)
	}

	return entry, nil
}

// bindError: неверный пароль или нет такого DN - не ошибка провайдера, а отказ.
func bindError(err error) error {
	if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) ||
		ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) ||
		ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidDNSyntax) {
		return nil
	}

	return fmt.Errorf("ldapauth: bind: %w", err)
}

// attributes - запрашиваемые атрибуты записи.
func (p *Provider) attributes() []string {
	attrs := []string{p.uidAttr}

	for _, attr := range []string{p.nameAttr, p.mailAttr} {
		if attr != "" {
			attrs = append(attrs, attr)
		}
	}

	return attrs
}

// FromConfig собирает включенные провайдеры из auth.ldap.
func FromConfig(providers []config.LDAPProvider) ([]*Provider, error) {
	out := make([]*Provider, 0, len(providers))

	for i := range providers {
		if !providers[i].IsEnabled() {
			continue
		}

		provider, err := New(providers[i], nil)
		if err != nil {
			return nil, fmt.Errorf("auth.ldap[%d]: %w", i, err)
		}

		out = append(out, provider)
	}

	return out, nil
}
