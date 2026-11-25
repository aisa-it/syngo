// Package config загружает конфигурацию сервиса из YAML-файла и переменных
// окружения и валидирует её до старта.
//
// Порядок применения: значения по умолчанию → YAML → переменные окружения.
// Переменные окружения имеют приоритет, что позволяет не хранить секреты
// (DSN) в файле.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"
)

// Имена переменных окружения, переопределяющих файл конфигурации.
const (
	EnvConfig = "SYNGOD_CONFIG"
	// EnvName — окружение (dev | test | prod); см. Config.Env.
	EnvName       = "SYNGOD_ENV"
	EnvServerAddr = "SYNGOD_SERVER_ADDR"
	// EnvOpsAddr - адрес служебного листенера.
	EnvOpsAddr          = "SYNGOD_OPS_ADDR"
	EnvServerName       = "SYNGOD_MATRIX_SERVER_NAME"
	EnvDatabaseDSN      = "SYNGOD_DATABASE_DSN"
	EnvAuthRegistration = "SYNGOD_AUTH_REGISTRATION"
	//nolint:gosec // это имя переменной окружения, а не значение секрета
	EnvSyncTokenSecret    = "SYNGOD_SYNC_TOKEN_SECRET"
	EnvMediaDirectory     = "SYNGOD_MEDIA_DIRECTORY"
	EnvFederationOn       = "SYNGOD_FEDERATION_ENABLED"
	EnvFederationKey      = "SYNGOD_FEDERATION_SIGNING_KEY"
	EnvLogLevel           = "SYNGOD_LOG_LEVEL"
	EnvLogFormat          = "SYNGOD_LOG_FORMAT"
	EnvFederationScheme   = "SYNGOD_FEDERATION_SCHEME"
	EnvFederationInsecure = "SYNGOD_FEDERATION_INSECURE_SKIP_VERIFY"
	EnvTLSCertFile        = "SYNGOD_TLS_CERT_FILE"
	EnvTLSKeyFile         = "SYNGOD_TLS_KEY_FILE"
	EnvRateLimitDisabled  = "SYNGOD_RATE_LIMIT_DISABLED"
	// EnvTrustedProxies - server.trusted_proxies через запятую.
	EnvTrustedProxies = "SYNGOD_TRUSTED_PROXIES"
	//nolint:gosec // это имя переменной окружения, а не значение секрета
	EnvRegistrationSharedSecret = "SYNGOD_REGISTRATION_SHARED_SECRET"
	//nolint:gosec // это имя переменной окружения, а не значение секрета
	EnvVoIPSecret = "SYNGOD_VOIP_SECRET"
)

// MinSecretLength — минимальная длина секретов (sync.token_secret,
// auth.registration_shared_secret) вне dev/test: 32 символа, как
// у `openssl rand -hex 16`.
const MinSecretLength = 32

// DefaultPath — путь к конфигу, который читается, если не задан EnvConfig.
const DefaultPath = "config.yaml"

// Config — корень конфигурации сервиса.
type Config struct {
	// Env — окружение: dev | test | prod. По умолчанию prod; пустое
	// значение тоже трактуется как prod.
	Env        string     `yaml:"env"`
	Server     Server     `yaml:"server"`
	Matrix     Matrix     `yaml:"matrix"`
	Database   Database   `yaml:"database"`
	Auth       Auth       `yaml:"auth"`
	Sync       Sync       `yaml:"sync"`
	Media      Media      `yaml:"media"`
	Federation Federation `yaml:"federation"`
	RateLimit  RateLimit  `yaml:"rate_limit"`
	VoIP       VoIP       `yaml:"voip"`
	Admin      Admin      `yaml:"admin"`
	Push       Push       `yaml:"push"`
	Redaction  Redaction  `yaml:"redaction"`
	Log        Log        `yaml:"log"`
}

// Redaction - хранение оригинала удаленных событий.
type Redaction struct {
	// RetentionPeriod - сколько хранить оригинальный content удаленного события
	// (в events сразу лежит вырезанный, оригинал - в redactions.original_content),
	// как redaction_retention_period в Synapse (по умолчанию 7 дней). 0 - оригинал
	// не сохраняется вовсе.
	RetentionPeriod Duration `yaml:"retention_period"`
}

// Push — доставка push-уведомлений на http-pushers.
type Push struct {
	// AllowHTTP разрешает http-pushers (только стенд/Complement): в проде
	// допустим лишь https (защита от SSRF).
	AllowHTTP bool `yaml:"allow_http"`
	// AllowPrivate разрешает приватные/loopback адреса pusher'ов — ТОЛЬКО для
	// интеграционных тестов и локального стенда (в проде выключен).
	AllowPrivate bool `yaml:"allow_private"`
	// Timeout — таймаут одного push-запроса.
	Timeout Duration `yaml:"timeout"`
}

// Admin — admin-контур /_syngo/admin/v1 (ADR-0008).
type Admin struct {
	// Enabled включает admin API; по умолчанию выключен.
	Enabled bool `yaml:"enabled"`
	// TokenTTL — срок жизни admin-токена (access-токена админа).
	TokenTTL Duration `yaml:"token_ttl"`
	// AuditRetention — сколько хранить записи admin_audit (0 — не чистить).
	AuditRetention Duration `yaml:"audit_retention"`
}

// VoIP — TURN-сервер для звонков.
type VoIP struct {
	// URIs — список TURN-адресов, отдаётся клиентам в /voip/turnServer.
	URIs []string `yaml:"uris"`
	// Secret — общий секрет coturn (static-auth-secret); из него считаются
	// username/password по схеме «timestamp:user».
	Secret string `yaml:"secret"`
	// TTL — срок жизни креденшелов (по умолчанию 1 час).
	TTL Duration `yaml:"ttl"`
}

// Server описывает параметры HTTP-сервера и graceful shutdown.
type Server struct {
	Addr string `yaml:"addr"`
	// TLSCertFile/TLSKeyFile включают HTTPS-листенер (интероп-стенд, hardening).
	TLSCertFile       string   `yaml:"tls_cert_file"`
	TLSKeyFile        string   `yaml:"tls_key_file"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	// OpsAddr - отдельный служебный листенер: /metrics, подробные
	// /health и /health/ready, pprof. На основном листенере остаются только
	// краткие /health и /health/ready (для балансировщика и healthcheck), а
	// /metrics там нет (вне dev/test). Адрес держать во внутренней сети.
	OpsAddr string `yaml:"ops_addr"`
	// Pprof включает /debug/pprof/ на служебном листенере (нужен ops_addr).
	Pprof bool `yaml:"pprof"`
	// TrustedProxies - адреса доверенных прокси (CIDR или IP). Только
	// от них принимаются X-Forwarded-For и X-Real-IP: IP клиента для лимитов,
	// логов и аудита берется из заголовка. Пусто - заголовки игнорируются, IP
	// клиента - адрес соединения.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// PublicBaseurl - внешний адрес сервера для клиентов (public_baseurl Synapse):
	// из него собирается адрес callback OIDC
	// <public_baseurl>_synapse/client/oidc/callback. Обязателен при auth.oidc_providers.
	PublicBaseurl string `yaml:"public_baseurl"`
}

// TrustedProxyPrefixes разбирает server.trusted_proxies. Одиночный
// IP - сеть из одного адреса; IPv4 в IPv6-записи (::ffff:a.b.c.d) приводится к IPv4.
// Сеть /0 запрещена: доверять всем - то же, что принимать подделанный
// X-Forwarded-For от любого клиента.
func (s Server) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(s.TrustedProxies))

	for i, raw := range s.TrustedProxies {
		value := strings.TrimSpace(raw)

		prefix, err := parseTrustedProxy(value)
		if err != nil {
			return nil, fmt.Errorf("config: server.trusted_proxies[%d] %q: ожидается CIDR (10.0.0.0/8) или IP (10.0.0.1)", i, raw)
		}

		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("config: server.trusted_proxies[%d] %q: сеть /0 запрещена - "+
				"X-Forwarded-For подделывался бы любым клиентом; укажите адреса прокси", i, raw)
		}

		out = append(out, prefix)
	}

	return out, nil
}

func parseTrustedProxy(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, err
		}

		// ::ffff:10.0.0.0/104 - то же, что 10.0.0.0/8: адрес соединения
		// сравнивается в IPv4-форме.
		if addr := prefix.Addr(); addr.Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
		}

		return prefix.Masked(), nil
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}

	addr = addr.Unmap()

	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Matrix содержит параметры, идентифицирующие homeserver в федерации.
type Matrix struct {
	ServerName string `yaml:"server_name"`
}

// Database описывает подключение к PostgreSQL.
type Database struct {
	DSN string `yaml:"dsn"`
	// MaxConns - предел пула; по умолчанию 20: около 2-3 соединений
	// на ядро сервера БД и с запасом до max_connections PostgreSQL (100 по
	// умолчанию) для миграций, бэкапа и администрирования. Сборки /sync берут
	// не больше половины (sync.max_concurrent_builds).
	MaxConns       int32    `yaml:"max_conns"`
	MinConns       int32    `yaml:"min_conns"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	// ActivityFlushInterval - как часто отметки активности (last_validated_at
	// токенов, last_accessed_at медиа) пишутся в БД пачкой; 0 - 10s.
	ActivityFlushInterval Duration `yaml:"activity_flush_interval"`
}

// Auth описывает параметры аутентификации.
type Auth struct {
	// RegistrationOpen разрешает POST /register без UIA-провайдера (регистрация «открыта»).
	// По умолчанию false: учетные записи создает оператор через
	// registration_shared_secret.
	RegistrationOpen bool `yaml:"registration_open"`
	// RegistrationSharedSecret включает регистрацию через
	// /_synapse/admin/v1/register (HMAC, как registration_shared_secret в Synapse):
	// так оператор создает пользователей и первого администратора при закрытой
	// регистрации. Пусто - эндпоинт выключен.
	RegistrationSharedSecret string `yaml:"registration_shared_secret"`
	// MinPasswordLength — минимальная длина пароля при регистрации.
	MinPasswordLength int `yaml:"min_password_length"`
	// RefreshTokens включает выдачу refresh_token при запросе (флаг в теле /login и /register).
	RefreshTokens bool `yaml:"refresh_tokens"`
	// AccessTokenTTL — срок жизни access-токена; 0 — бессрочный.
	AccessTokenTTL Duration `yaml:"access_token_ttl"`
	// RefreshTokenTTL — зарезервировано под ротацию refresh-токенов (P1).
	RefreshTokenTTL Duration `yaml:"refresh_token_ttl"`
	// LastValidatedInterval - не чаще какого интервала обновлять
	// access_tokens.last_validated_at одного токена; 0 - 5m.
	LastValidatedInterval Duration `yaml:"last_validated_interval"`
	// LimitProfileToSharedRooms отдает чужой профиль только при общей комнате
	// (Synapse limit_profile_requests_to_users_who_share_rooms, по умолчанию false).
	// Выключено: профиль любого существующего пользователя виден авторизованному
	// клиенту, иначе Element считает адресата несуществующим и срывает приглашение.
	LimitProfileToSharedRooms bool `yaml:"limit_profile_to_shared_rooms"`
	// PasswordPepper - password_config.pepper Synapse: дописывается к паролю при
	// проверке перенесенных bcrypt-хешей. Задается тем же значением, что в
	// homeserver.yaml, иначе пароли пользователей Synapse с pepper не подойдут.
	PasswordPepper string `yaml:"password_pepper"`
	// PasswordLocalDB - проверять локальные хеши паролей (password_config.localdb_enabled
	// Synapse, по умолчанию true). false - вход только через внешние провайдеры (LDAP).
	PasswordLocalDB *bool `yaml:"password_localdb_enabled"`
	// LDAP - провайдеры входа по LDAP, опрашиваются по порядку до первого
	// успеха, как модули ldap_auth_provider.LdapAuthProviderModule в Synapse; поля
	// совпадают с config модуля synapse-ldap3.
	LDAP []LDAPProvider `yaml:"ldap"`
	// OIDCProviders - вход через OpenID Connect (oidc_providers Synapse).
	OIDCProviders []OIDCProvider `yaml:"oidc_providers"`
	// SSO - общие настройки SSO (секция sso Synapse).
	SSO SSO `yaml:"sso"`
}

// LocalPasswordsEnabled - включена ли проверка локальных хешей (по умолчанию да).
func (a Auth) LocalPasswordsEnabled() bool {
	return a.PasswordLocalDB == nil || *a.PasswordLocalDB
}

// LDAPProvider - один каталог LDAP. Поля как в synapse-ldap3: при заданном
// bind_dn пользователь ищется (режим search), иначе DN собирается из uid и base
// (режим simple).
type LDAPProvider struct {
	// Name - имя провайдера в журнале; по умолчанию uri.
	Name string `yaml:"name"`
	// Enabled - как enabled у модуля; nil - включен.
	Enabled *bool `yaml:"enabled"`
	// URI - ldap:// или ldaps://; несколько - пробуются по порядку (пул серверов ldap3).
	URI StringList `yaml:"uri"`
	// StartTLS - перейти на TLS по ldap:// (start_tls ldap3).
	StartTLS bool `yaml:"start_tls"`
	// Base - база поиска и суффикс DN в режиме simple.
	Base string `yaml:"base"`
	// Attributes - имена атрибутов uid, mail, name.
	Attributes LDAPAttributes `yaml:"attributes"`
	// BindDN, BindPassword, BindPasswordFile - служебная учетка для поиска.
	BindDN           string `yaml:"bind_dn"`
	BindPassword     string `yaml:"bind_password"`
	BindPasswordFile string `yaml:"bind_password_file"`
	// Filter - дополнительный фильтр поиска, например (objectClass=posixAccount).
	Filter string `yaml:"filter"`
	// Timeout - соединение и операция; по умолчанию 10s.
	Timeout Duration `yaml:"timeout"`
	// TLSCAFile - CA для ldaps/start_tls; TLSInsecureSkipVerify - только для стенда.
	TLSCAFile             string `yaml:"tls_ca_file"`
	TLSInsecureSkipVerify bool   `yaml:"tls_insecure_skip_verify"`
	// SyncDisplayName - обновлять displayname из атрибута name при каждом входе
	// (в synapse-ldap3 имя берется только при создании); nil - да.
	SyncDisplayName *bool `yaml:"sync_displayname"`
}

// LDAPAttributes - соответствие атрибутов каталога полям пользователя.
type LDAPAttributes struct {
	UID  string `yaml:"uid"`
	Mail string `yaml:"mail"`
	Name string `yaml:"name"`
}

// IsEnabled - провайдер включен (enabled не задан или true).
func (p LDAPProvider) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// SyncsDisplayName - обновлять ли displayname при входе (по умолчанию да).
func (p LDAPProvider) SyncsDisplayName() bool {
	return p.SyncDisplayName == nil || *p.SyncDisplayName
}

// StringList - строка или список строк в YAML (uri synapse-ldap3 допускает оба вида).
type StringList []string

// UnmarshalYAML реализует yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*l = StringList{node.Value}

		return nil
	}

	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}

	*l = list

	return nil
}

// Media описывает медиа-репозиторий: где лежат байты и какой лимит загрузки.
type Media struct {
	// StorageBackend — "local" (каталог) или "s3" (не реализован, ошибка валидации).
	StorageBackend string `yaml:"storage_backend"`
	// Directory — каталог локального хранилища (для local).
	Directory string `yaml:"directory"`
	// MaxUploadBytes — предельный размер одного файла (0 — без лимита, не рекомендуется).
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	// MaxPendingUploads - предел незаполненных placeholder async-загрузки
	// (/_matrix/media/v1/create) на пользователя; 0 - без предела
	// (Synapse max_pending_media_uploads, по умолчанию 5).
	MaxPendingUploads int `yaml:"max_pending_uploads"`
	// UnusedExpiration - за сколько placeholder должен быть заполнен
	// (Synapse unused_expiration_time, по умолчанию 24h).
	UnusedExpiration Duration `yaml:"unused_expiration"`
	// EnableAuthenticatedMedia - неаутентифицированные /_matrix/media/v3/download|thumbnail
	// отдают только медиа, созданные до LegacyFreeze (Synapse enable_authenticated_media,
	// по умолчанию true). false - legacy-пути отдают все медиа.
	EnableAuthenticatedMedia bool `yaml:"enable_authenticated_media"`
	// LegacyFreeze - момент freeze (RFC 3339, MSC3916): медиа, созданные позже,
	// доступны только через /_matrix/client/v1/media с токеном. Пусто - freeze
	// с начала времен (legacy не отдает ничего при enable_authenticated_media).
	LegacyFreeze string `yaml:"legacy_freeze"`
}

// DefaultMediaLegacyFreeze - freeze по умолчанию: релиз аутентифицированного медиа.
// Медиа, загруженные раньше, старые клиенты продолжают открывать.
const DefaultMediaLegacyFreeze = "2026-11-20T00:00:00Z"

// LegacyFreezeTime разбирает media.legacy_freeze (после Validate ошибки нет).
func (m Media) LegacyFreezeTime() time.Time {
	if strings.TrimSpace(m.LegacyFreeze) == "" {
		return time.Time{}
	}

	freeze, err := time.Parse(time.RFC3339, strings.TrimSpace(m.LegacyFreeze))
	if err != nil {
		return time.Time{}
	}

	return freeze
}

// Federation описывает параметры Server-Server API (ADR-0007): ключ сервера и доверие.
type Federation struct {
	// Enabled включает федерацию: без signing_key_file старт невозможен.
	Enabled bool `yaml:"enabled"`
	// SigningKeyFile — путь к ed25519-ключу в формате Synapse (chmod 600).
	SigningKeyFile string `yaml:"signing_key_file"`
	// ValidUntilDays — срок действия публикуемого ключа (по умолчанию 7 дней).
	ValidUntilDays int `yaml:"valid_until_days"`
	// TrustedKeyServers — нотарии для проверки чужих ключей (пусто — запрос напрямую),
	// формат trusted_key_servers Synapse: имя или {server_name, verify_keys}.
	TrustedKeyServers []TrustedKeyServer `yaml:"trusted_key_servers"`
	// OldSigningKeys - прежние ключи сервера для old_verify_keys, как
	// old_signing_keys Synapse: {"ed25519:id": {key: base64, expired_ts: ms}}.
	OldSigningKeys map[string]OldSigningKey `yaml:"old_signing_keys"`
	// AllowHosts/DenyHosts — политика исходящих федеративных запросов.
	AllowHosts []string `yaml:"allow_hosts"`
	DenyHosts  []string `yaml:"deny_hosts"`
	// IPRangeBlacklist - IP-диапазоны (CIDR или IP), закрытые для исходящих
	// федеративных запросов, как ip_range_blacklist Synapse (SSRF).
	// Не задан - частные и служебные диапазоны (transport.DefaultIPRangeBlacklist);
	// [] - без запретов.
	IPRangeBlacklist []string `yaml:"ip_range_blacklist"`
	// IPRangeWhitelist - исключения из IPRangeBlacklist (стенд в docker-сети,
	// внутренняя федерация), как ip_range_whitelist Synapse.
	IPRangeWhitelist []string `yaml:"ip_range_whitelist"`
	// MaxTransactionBytes — предел тела федеративного запроса (по умолчанию 1 MiB).
	MaxTransactionBytes int64 `yaml:"max_transaction_bytes"`
	// ConnectTimeout/RequestTimeout — таймауты федеративного клиента.
	ConnectTimeout Duration `yaml:"connect_timeout"`
	RequestTimeout Duration `yaml:"request_timeout"`
	// RetryMin/RetryMax — границы экспоненциального backoff.
	RetryMin Duration `yaml:"retry_min"`
	RetryMax Duration `yaml:"retry_max"`
	// IncomingRatePerSecond/IncomingBurst — rate limit входящих по origin (0 — выключено).
	IncomingRatePerSecond float64 `yaml:"incoming_rate_per_second"`
	IncomingBurst         int     `yaml:"incoming_burst"`
	// Scheme — схема федеративных запросов: https (по умолчанию) или http (локальный стенд).
	Scheme string `yaml:"scheme"`
	// InsecureSkipVerify — не проверять TLS-сертификат пира (стенд с self-signed CA).
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// InsecureSkipSignatureVerify — не проверять подписи входящих федеративных
	// событий. Только для интероп-стенда, где Synapse присылает события с подписью,
	// которую не удаётся воспроизвести его же ключом; в prod запрещено.
	InsecureSkipSignatureVerify bool `yaml:"insecure_skip_signature_verify"`
	// SendFullMemberState — игнорировать omit_members пира и всегда отдавать полный
	// member-state в send_join. Нужно стенду: Synapse после partial state join
	// держит комнату в partial state, и сверка состояния в interop не видит событий.
	// В prod не включать — это отступление от MSC3706.
	SendFullMemberState bool `yaml:"send_full_member_state"`
}

// TrustedKeyServer - нотарий ключей (trusted_key_servers Synapse).
type TrustedKeyServer struct {
	ServerName string `yaml:"server_name"`
	// VerifyKeys - ключи нотария {key_id: base64}; пусто - запрашиваются у него.
	VerifyKeys map[string]string `yaml:"verify_keys"`
}

// UnmarshalYAML принимает и строку (имя нотария), и объект.
func (t *TrustedKeyServer) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*t = TrustedKeyServer{ServerName: node.Value}

		return nil
	}

	type plain TrustedKeyServer

	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}

	*t = TrustedKeyServer(decoded)

	return nil
}

// OldSigningKey - прежний публичный ключ сервера и момент, когда он перестал действовать.
type OldSigningKey struct {
	Key       string `yaml:"key"`
	ExpiredTS int64  `yaml:"expired_ts"`
}

// RateLimitAction — настройки одного лимита (модель Synapse: leaky bucket as meter).
type RateLimitAction struct {
	Enabled   bool    `yaml:"enabled"`
	PerSecond float64 `yaml:"per_second"`
	Burst     int     `yaml:"burst"`
}

// Sync описывает параметры /sync (ADR-0005).
type Sync struct {
	// TokenSecret — секрет HMAC для токенов next_batch. В проде задаётся явно:
	// без него токены невалидны после перезапуска (мы генерируем случайный).
	TokenSecret string `yaml:"token_secret"`
	// MaxTimeoutMilliseconds — верхняя граница long-poll, мс (sync.max_timeout в Synapse).
	MaxTimeoutMilliseconds int `yaml:"max_timeout_ms"`
	// PerRoomLimit — сколько событий комнаты отдавать в одной порции /sync.
	PerRoomLimit int `yaml:"per_room_limit"`
}

// RateLimit описывает лимиты по классам запросов. Ключ лимита:
// login/register — IP клиента (за доверенным прокси - из X-Forwarded-For),
// login_account — учетная запись (localpart без регистра), send/media — пользователь,
// sync — устройство пользователя. Defaults повторяют Synapse, sync — наше решение.
type RateLimit struct {
	LoginIP      RateLimitAction `yaml:"login_ip"`
	LoginAccount RateLimitAction `yaml:"login_account"`
	RegisterIP   RateLimitAction `yaml:"register_ip"`
	Send         RateLimitAction `yaml:"send"`
	MediaUpload  RateLimitAction `yaml:"media_upload"`
	Sync         RateLimitAction `yaml:"sync"`
}

// Actions возвращает лимиты по именам для валидации и логов.
func (r RateLimit) Actions() map[string]RateLimitAction {
	return map[string]RateLimitAction{
		"rate_limit.login_ip":      r.LoginIP,
		"rate_limit.login_account": r.LoginAccount,
		"rate_limit.register_ip":   r.RegisterIP,
		"rate_limit.send":          r.Send,
		"rate_limit.media_upload":  r.MediaUpload,
		"rate_limit.sync":          r.Sync,
	}
}

// Log описывает формат и уровень структурированных логов.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Duration — обёртка над time.Duration, которая читается из YAML как строка
// ("10s", "1m") и как целое число наносекунд.
type Duration time.Duration

// UnmarshalYAML реализует yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string

	if err := node.Decode(&raw); err == nil {
		parsed, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("config: invalid duration %q: %w", raw, err)
		}

		*d = Duration(parsed)

		return nil
	}

	var nanos int64
	if err := node.Decode(&nanos); err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", node.Value, err)
	}

	*d = Duration(time.Duration(nanos))

	return nil
}

// MarshalYAML реализует yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std возвращает значение как time.Duration.
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

// Default возвращает конфигурацию по умолчанию: сервис слушает :8008,
// логирует в JSON на уровне info, БД не подключена.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8008",
			ReadHeaderTimeout: Duration(10 * time.Second),
			ReadTimeout:       Duration(30 * time.Second),
			WriteTimeout:      Duration(60 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			ShutdownTimeout:   Duration(15 * time.Second),
		},
		// Env prod и пустой server_name: прод-конфиг без явных настроек не стартует,
		// а не создает необратимые @user:localhost.
		Env: "prod",
		Database: Database{
			MaxConns:       20,
			MinConns:       0,
			ConnectTimeout: Duration(5 * time.Second),
			// В Synapse client_ips сбрасываются раз в 5 с, last_access_ts медиа - раз в 60 с.
			ActivityFlushInterval: Duration(10 * time.Second),
		},
		Sync: Sync{
			MaxTimeoutMilliseconds: 30_000,
			PerRoomLimit:           20,
		},
		Admin: Admin{
			Enabled:        false,
			TokenTTL:       Duration(1 * time.Hour),
			AuditRetention: Duration(90 * 24 * time.Hour),
		},
		Auth: Auth{
			RegistrationOpen:      false,
			MinPasswordLength:     8,
			RefreshTokens:         false,
			AccessTokenTTL:        0,
			RefreshTokenTTL:       Duration(30 * 24 * time.Hour),
			LastValidatedInterval: Duration(5 * time.Minute),
		},
		Federation: Federation{
			Enabled:               false,
			ValidUntilDays:        7,
			MaxTransactionBytes:   1 << 20,
			ConnectTimeout:        Duration(5 * time.Second),
			RequestTimeout:        Duration(30 * time.Second),
			RetryMin:              Duration(5 * time.Second),
			RetryMax:              Duration(time.Hour),
			IncomingRatePerSecond: 10,
			IncomingBurst:         50,
			Scheme:                "https",
		},
		Push: Push{
			Timeout: Duration(5 * time.Second),
		},
		Redaction: Redaction{
			RetentionPeriod: Duration(7 * 24 * time.Hour),
		},
		Media: Media{
			StorageBackend: "local",
			Directory:      "var/media",
			MaxUploadBytes: 50 << 20, // 50 MiB, как default max_upload_size в Synapse
			// Значения Synapse по умолчанию.
			MaxPendingUploads:        5,
			UnusedExpiration:         Duration(24 * time.Hour),
			EnableAuthenticatedMedia: true,
			LegacyFreeze:             DefaultMediaLegacyFreeze,
		},
		RateLimit: RateLimit{
			// Значения Synapse по умолчанию (rc_login.address/account, rc_registration,
			// rc_message, rc_media_create). sync — наш мягкий лимит, в Synapse его нет.
			LoginIP:      RateLimitAction{Enabled: true, PerSecond: 0.003, Burst: 5},
			LoginAccount: RateLimitAction{Enabled: true, PerSecond: 0.003, Burst: 5},
			RegisterIP:   RateLimitAction{Enabled: true, PerSecond: 0.17, Burst: 3},
			Send:         RateLimitAction{Enabled: true, PerSecond: 0.2, Burst: 10},
			MediaUpload:  RateLimitAction{Enabled: true, PerSecond: 10, Burst: 50},
			Sync:         RateLimitAction{Enabled: true, PerSecond: 2, Burst: 20},
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
	}
}

// Load собирает конфигурацию: Default → файл → окружение → Validate.
//
// Файл читается, если path непустой. Отсутствие файла по пути из EnvConfig
// считается ошибкой, отсутствие файла по DefaultPath — нет.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		path = os.Getenv(EnvConfig)
	}

	if path != "" {
		if err := loadFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func loadFile(path string, cfg *Config) error {
	//nolint:gosec // путь к конфигу задаёт оператор через -config/$SYNGOD_CONFIG, это не пользовательский ввод
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && path == DefaultPath {
			return nil
		}

		return fmt.Errorf("config: read %s: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}

	return nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvName); v != "" {
		cfg.Env = v
	}

	if v := os.Getenv(EnvServerAddr); v != "" {
		cfg.Server.Addr = v
	}

	if v := os.Getenv(EnvServerName); v != "" {
		cfg.Matrix.ServerName = v
	}

	if v := os.Getenv(EnvFederationOn); v != "" {
		cfg.Federation.Enabled = v == "1" || strings.EqualFold(v, "true")
	}

	if v := os.Getenv(EnvFederationKey); v != "" {
		cfg.Federation.SigningKeyFile = v
	}

	if v := os.Getenv(EnvMediaDirectory); v != "" {
		cfg.Media.Directory = v
	}

	if v := os.Getenv(EnvDatabaseDSN); v != "" {
		cfg.Database.DSN = v
	}

	if v := os.Getenv(EnvSyncTokenSecret); v != "" {
		cfg.Sync.TokenSecret = v
	}

	if v := os.Getenv(EnvRegistrationSharedSecret); v != "" {
		cfg.Auth.RegistrationSharedSecret = v
	}

	if v := os.Getenv(EnvVoIPSecret); v != "" {
		cfg.VoIP.Secret = v
	}

	if v := os.Getenv(EnvAuthRegistration); v != "" {
		cfg.Auth.RegistrationOpen = v == "1" || strings.EqualFold(v, "true")
	}

	if v := os.Getenv(EnvLogLevel); v != "" {
		cfg.Log.Level = v
	}

	if v := os.Getenv(EnvLogFormat); v != "" {
		cfg.Log.Format = v
	}

	if v := os.Getenv(EnvFederationScheme); v != "" {
		cfg.Federation.Scheme = v
	}

	if v := os.Getenv(EnvRateLimitDisabled); v == "1" || strings.EqualFold(v, "true") {
		// Тестовые контуры (Complement, стенд): лимиты выключаются целиком.
		cfg.RateLimit = RateLimit{}
	}

	if v := os.Getenv(EnvTLSCertFile); v != "" {
		cfg.Server.TLSCertFile = v
	}

	if v := os.Getenv(EnvTLSKeyFile); v != "" {
		cfg.Server.TLSKeyFile = v
	}

	if v := os.Getenv(EnvOpsAddr); v != "" {
		cfg.Server.OpsAddr = v
	}

	// Список через запятую или пробелы; заданная переменная заменяет список из файла.
	if v := os.Getenv(EnvTrustedProxies); v != "" {
		cfg.Server.TrustedProxies = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		})
	}

	if v := os.Getenv(EnvFederationInsecure); v != "" {
		cfg.Federation.InsecureSkipVerify = v == "1" || strings.EqualFold(v, "true")
	}
}

// IsDev сообщает, что окружение разрешает небезопасные для прода упрощения
// (случайный секрет токенов /sync, открытая регистрация в примерах).
func (c Config) IsDev() bool {
	return c.Env == "dev" || c.Env == "test"
}

// Validate проверяет конфигурацию и возвращает первую найденную ошибку.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return errors.New("config: server.addr is required")
	}

	if err := validateServerName(c.Matrix.ServerName); err != nil {
		return err
	}

	if c.Redaction.RetentionPeriod < 0 {
		return errors.New("config: redaction.retention_period must be >= 0")
	}

	switch c.Media.StorageBackend {
	case "local":
		if strings.TrimSpace(c.Media.Directory) == "" {
			return errors.New("config: media.directory is required for local storage")
		}
	case "s3":
		return errors.New("config: media.storage_backend \"s3\" is not implemented yet (use local)")
	case "":
		return errors.New("config: media.storage_backend is required (local)")
	default:
		return fmt.Errorf("config: media.storage_backend %q is not supported", c.Media.StorageBackend)
	}

	if c.Federation.Enabled {
		if strings.TrimSpace(c.Federation.SigningKeyFile) == "" {
			return errors.New("config: federation.signing_key_file is required when federation is enabled")
		}

		if c.Federation.ValidUntilDays <= 0 {
			return errors.New("config: federation.valid_until_days must be positive")
		}

		switch c.Federation.Scheme {
		case "", "https":
			// https по умолчанию (пустое значение = дефолт)
		case "http":
			// Локальный стенд: без TLS, insecure_skip_verify допустим только здесь.
		default:
			return fmt.Errorf("config: federation.scheme %q is not supported (https|http)", c.Federation.Scheme)
		}

		if c.Federation.InsecureSkipVerify && c.Federation.Scheme == "http" {
			return errors.New("config: federation.insecure_skip_verify несовместим со scheme http")
		}

		if err := validateIPRanges("federation.ip_range_blacklist", c.Federation.IPRangeBlacklist); err != nil {
			return err
		}

		if err := c.Federation.validateKeys(); err != nil {
			return err
		}

		if err := validateIPRanges("federation.ip_range_whitelist", c.Federation.IPRangeWhitelist); err != nil {
			return err
		}
	}

	if err := c.validateUnsafeFlags(); err != nil {
		return err
	}

	if err := c.validateOps(); err != nil {
		return err
	}

	if _, err := c.Server.TrustedProxyPrefixes(); err != nil {
		return err
	}

	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		return errors.New("config: server.tls_cert_file и server.tls_key_file задаются вместе")
	}

	if c.Media.MaxUploadBytes < 0 {
		return errors.New("config: media.max_upload_bytes must be >= 0")
	}

	if c.Media.MaxPendingUploads < 0 {
		return errors.New("config: media.max_pending_uploads must be >= 0")
	}

	if c.Media.UnusedExpiration < 0 {
		return errors.New("config: media.unused_expiration must be >= 0")
	}

	if freeze := strings.TrimSpace(c.Media.LegacyFreeze); freeze != "" {
		if _, err := time.Parse(time.RFC3339, freeze); err != nil {
			return fmt.Errorf("config: media.legacy_freeze must be RFC 3339: %w", err)
		}
	}

	for name, action := range c.RateLimit.Actions() {
		if !action.Enabled {
			continue
		}

		if action.PerSecond <= 0 {
			return fmt.Errorf("config: %s.per_second must be positive when enabled", name)
		}

		if action.Burst < 1 {
			return fmt.Errorf("config: %s.burst must be >= 1 when enabled", name)
		}
	}

	for name, d := range map[string]Duration{
		"server.read_header_timeout": c.Server.ReadHeaderTimeout,
		"server.read_timeout":        c.Server.ReadTimeout,
		"server.write_timeout":       c.Server.WriteTimeout,
		"server.idle_timeout":        c.Server.IdleTimeout,
		"server.shutdown_timeout":    c.Server.ShutdownTimeout,
	} {
		if d.Std() <= 0 {
			return fmt.Errorf("config: %s must be positive", name)
		}
	}

	switch c.Env {
	case "", "prod", "dev", "test":
	default:
		return fmt.Errorf("config: env %q is not one of dev|test|prod", c.Env)
	}

	if err := c.validateProdRequirements(); err != nil {
		return err
	}

	if c.Sync.MaxTimeoutMilliseconds < 0 || c.Sync.MaxTimeoutMilliseconds > 300_000 {
		return fmt.Errorf("config: sync.max_timeout_ms must be between 0 and 300000")
	}

	if c.Sync.PerRoomLimit <= 0 {
		return fmt.Errorf("config: sync.per_room_limit must be positive")
	}

	if c.Auth.MinPasswordLength < 8 {
		return fmt.Errorf("config: auth.min_password_length must be at least 8")
	}

	if c.Auth.AccessTokenTTL.Std() < 0 {
		return errors.New("config: auth.access_token_ttl must not be negative")
	}

	if c.Auth.RefreshTokenTTL.Std() < 0 {
		return errors.New("config: auth.refresh_token_ttl must not be negative")
	}

	if c.Auth.LastValidatedInterval.Std() < 0 {
		return errors.New("config: auth.last_validated_interval must not be negative")
	}

	if c.Database.ActivityFlushInterval.Std() < 0 {
		return errors.New("config: database.activity_flush_interval must not be negative")
	}

	for i := range c.Auth.LDAP {
		provider := &c.Auth.LDAP[i]
		if !provider.IsEnabled() {
			continue
		}

		if len(provider.URI) == 0 || provider.Base == "" || provider.Attributes.UID == "" {
			return fmt.Errorf("config: auth.ldap[%d]: uri, base and attributes.uid are required", i)
		}

		for _, uri := range provider.URI {
			if !strings.HasPrefix(uri, "ldap://") && !strings.HasPrefix(uri, "ldaps://") {
				return fmt.Errorf("config: auth.ldap[%d]: uri %q must start with ldap:// or ldaps://", i, uri)
			}
		}

		if provider.BindPassword != "" && provider.BindPasswordFile != "" {
			return fmt.Errorf("config: auth.ldap[%d]: bind_password and bind_password_file are mutually exclusive", i)
		}

		if provider.Timeout.Std() < 0 {
			return fmt.Errorf("config: auth.ldap[%d]: timeout must not be negative", i)
		}
	}

	if !c.Auth.LocalPasswordsEnabled() && len(c.Auth.LDAP) == 0 && len(c.Auth.EnabledOIDCProviders()) == 0 {
		return errors.New("config: auth.password_localdb_enabled: false requires at least one auth.ldap or auth.oidc_providers provider")
	}

	if err := c.validateOIDC(); err != nil {
		return err
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log.level %q is not one of debug|info|warn|error", c.Log.Level)
	}

	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("config: log.format %q is not one of json|text", c.Log.Format)
	}

	if c.Database.DSN != "" {
		if c.Database.MaxConns < 1 {
			return errors.New("config: database.max_conns must be >= 1 when dsn is set")
		}

		if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
			return errors.New("config: database.min_conns must be in [0, max_conns]")
		}

		if c.Database.ConnectTimeout.Std() <= 0 {
			return errors.New("config: database.connect_timeout must be positive")
		}

		if _, err := pgxpool.ParseConfig(c.Database.DSN); err != nil {
			return fmt.Errorf("config: database.dsn is invalid: %w", err)
		}
	}

	return nil
}

// validateKeys проверяет trusted_key_servers и old_signing_keys.
func (f Federation) validateKeys() error {
	for i, notary := range f.TrustedKeyServers {
		if strings.TrimSpace(notary.ServerName) == "" {
			return fmt.Errorf("config: federation.trusted_key_servers[%d]: server_name is required", i)
		}

		for keyID, key := range notary.VerifyKeys {
			if !validPublicKey(keyID, key) {
				return fmt.Errorf("config: federation.trusted_key_servers[%d]: bad verify key %q", i, keyID)
			}
		}
	}

	for keyID, old := range f.OldSigningKeys {
		if !validPublicKey(keyID, old.Key) {
			return fmt.Errorf("config: federation.old_signing_keys: bad key %q", keyID)
		}

		if old.ExpiredTS <= 0 {
			return fmt.Errorf("config: federation.old_signing_keys[%q]: expired_ts is required", keyID)
		}
	}

	return nil
}

// validPublicKey - key_id ed25519:<version> и 32 байта ключа в base64.
func validPublicKey(keyID, key string) bool {
	if !strings.HasPrefix(keyID, "ed25519:") || len(keyID) == len("ed25519:") {
		return false
	}

	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(key, "="))

	return err == nil && len(decoded) == ed25519.PublicKeySize
}

// validateIPRanges проверяет список CIDR/IP (ip_range_blacklist/whitelist).
func validateIPRanges(name string, ranges []string) error {
	for _, item := range ranges {
		if _, err := netip.ParsePrefix(item); err == nil {
			continue
		}

		if _, err := netip.ParseAddr(item); err != nil {
			return fmt.Errorf("config: %s: %q is not a CIDR or IP", name, item)
		}
	}

	return nil
}

// validateUnsafeFlags запрещает отладочные флаги вне dev/test. Сравнение именно
// через IsDev: пустой env - это prod, и `c.Env == "prod"` пропускал конфиг без
// env с выключенной проверкой подписей.
func (c Config) validateUnsafeFlags() error {
	if c.IsDev() {
		return nil
	}

	for _, f := range []struct {
		name string
		on   bool
	}{
		{"federation.insecure_skip_verify", c.Federation.InsecureSkipVerify},
		{"federation.insecure_skip_signature_verify", c.Federation.InsecureSkipSignatureVerify},
		{"federation.send_full_member_state", c.Federation.SendFullMemberState},
		{"push.allow_http", c.Push.AllowHTTP},
		{"push.allow_private", c.Push.AllowPrivate},
	} {
		if f.on {
			return fmt.Errorf("config: %s запрещен вне dev/test (env=%q)", f.name, c.envName())
		}
	}

	return nil
}

// validateProdRequirements - обязательные вне dev/test настройки:
// БД, секреты достаточной длины, публичный server_name.
func (c Config) validateProdRequirements() error {
	if c.IsDev() {
		return nil
	}

	// Секрет HMAC для токенов /sync: случайный секрет ломает токены при рестарте
	// и между инстансами, короткий подбирается.
	if c.Sync.TokenSecret == "" {
		return fmt.Errorf("config: sync.token_secret is required outside dev/test "+
			"(set %s, e.g. `openssl rand -hex 32`, or SYNGOD_ENV=dev for a random one)", EnvSyncTokenSecret)
	}

	if len(c.Sync.TokenSecret) < MinSecretLength {
		return fmt.Errorf("config: sync.token_secret is too short: need at least %d characters outside dev/test (%s)",
			MinSecretLength, EnvSyncTokenSecret)
	}

	if s := c.Auth.RegistrationSharedSecret; s != "" && len(s) < MinSecretLength {
		return fmt.Errorf("config: auth.registration_shared_secret is too short: need at least %d characters outside dev/test (%s)",
			MinSecretLength, EnvRegistrationSharedSecret)
	}

	if isLocalServerName(c.Matrix.ServerName) {
		return fmt.Errorf("config: matrix.server_name %q is not allowed outside dev/test: "+
			"user IDs @user:%s cannot be renamed later, set the public domain (%s)",
			c.Matrix.ServerName, c.Matrix.ServerName, EnvServerName)
	}

	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("config: database.dsn is required outside dev/test (set %s)", EnvDatabaseDSN)
	}

	return nil
}

// envName - окружение для сообщений об ошибках: пустое показываем как prod.
func (c Config) envName() string {
	if c.Env == "" {
		return "prod"
	}

	return c.Env
}

// isLocalServerName сообщает, что имя сервера локальное (localhost, *.localhost,
// loopback-адрес): в проде такие user_id необратимы.
func isLocalServerName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}

	ip := net.ParseIP(name)

	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func validateServerName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("config: matrix.server_name is required (set matrix.server_name or %s)", EnvServerName)
	}

	if strings.ContainsAny(name, "/:#") || strings.Contains(name, "://") {
		return fmt.Errorf("config: matrix.server_name %q must be a bare hostname, without scheme, port or path", name)
	}

	if name != strings.ToLower(name) {
		return fmt.Errorf("config: matrix.server_name %q must be lowercase", name)
	}

	return nil
}

// validateOps проверяет служебный листенер.
func (c Config) validateOps() error {
	ops := strings.TrimSpace(c.Server.OpsAddr)

	if c.Server.Pprof && ops == "" {
		return errors.New("config: server.pprof требует server.ops_addr (pprof только на служебном листенере)")
	}

	if ops == "" {
		return nil
	}

	if _, _, err := net.SplitHostPort(ops); err != nil {
		return fmt.Errorf("config: server.ops_addr %q: %w", ops, err)
	}

	if ops == strings.TrimSpace(c.Server.Addr) {
		return errors.New("config: server.ops_addr должен отличаться от server.addr")
	}

	return nil
}

// String реализует fmt.Stringer, чтобы удобно логировать итоговую конфигурацию
// без секретов.
func (c Config) String() string {
	dsn := "not configured"
	if c.Database.DSN != "" {
		dsn = "configured"
	}

	return fmt.Sprintf("addr=%s ops_addr=%s server_name=%s db=%s log=%s/%s trusted_proxies=%d",
		c.Server.Addr, c.Server.OpsAddr, c.Matrix.ServerName, dsn, c.Log.Level, c.Log.Format,
		len(c.Server.TrustedProxies))
}
