package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	cfg := config.Default()

	if cfg.Server.Addr != ":8008" {
		t.Fatalf("server.addr = %q, want :8008", cfg.Server.Addr)
	}

	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Fatalf("log = %s/%s, want info/json", cfg.Log.Level, cfg.Log.Format)
	}

	// Пул: 20 соединений.
	if cfg.Database.MaxConns != 20 {
		t.Fatalf("database.max_conns = %d, want 20", cfg.Database.MaxConns)
	}

	// Безопасность по умолчанию: окружение prod, server_name не задан, секрета
	// токенов нет - конфиг невалиден.
	if cfg.Env != "prod" {
		t.Fatalf("env = %q, want prod", cfg.Env)
	}

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "matrix.server_name") {
		t.Fatalf("Validate() = %v, ожидали ошибку про matrix.server_name", err)
	}

	cfg.Matrix.ServerName = "matrix.example.org"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "sync.token_secret") {
		t.Fatalf("Validate() = %v, ожидали ошибку про sync.token_secret", err)
	}

	cfg.Env = "dev"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("dev config must be valid: %v", err)
	}
}

// TestEnvAndTokenSecret — матрица правил: секрет обязателен вне dev/test.
func TestEnvAndTokenSecret(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     string
		secret  string
		wantErr bool
	}{
		{name: "пустое окружение как prod", env: "", wantErr: true},
		{name: "prod без секрета", env: "prod", wantErr: true},
		{name: "prod с секретом", env: "prod", secret: prodSecret},
		{name: "prod с коротким секретом", env: "prod", secret: "s3cret", wantErr: true},
		{name: "пустое окружение с коротким секретом", env: "", secret: "s3cret", wantErr: true},
		{name: "dev без секрета", env: "dev"},
		{name: "test без секрета", env: "test"},
		{name: "dev с коротким секретом", env: "dev", secret: "s3cret"},
		{name: "неизвестное окружение", env: "staging", secret: prodSecret, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validProd()
			cfg.Env = tt.env
			cfg.Sync.TokenSecret = tt.secret

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() = nil, ожидали ошибку")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, ошибки не ждали", err)
			}
		})
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	// t.Setenv несовместим с t.Parallel: тест меняет окружение процесса.

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yamlBody := `
server:
  addr: ":9000"
  read_timeout: 5s
matrix:
  server_name: matrix.example.org
database:
  dsn: postgres://user:pass@localhost:5432/syngo?sslmode=disable
  max_conns: 20
log:
  level: debug
  format: text
`
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv(config.EnvServerName, "env.example.org")
	t.Setenv(config.EnvName, "dev") // секрет токенов не задаём: в dev допустим случайный

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Server.Addr != ":9000" {
		t.Fatalf("server.addr = %q, want :9000", cfg.Server.Addr)
	}

	if got := cfg.Server.ReadTimeout.Std(); got != 5*time.Second {
		t.Fatalf("server.read_timeout = %s, want 5s", got)
	}

	// Переменная окружения перекрывает файл.
	if cfg.Matrix.ServerName != "env.example.org" {
		t.Fatalf("matrix.server_name = %q, want env.example.org", cfg.Matrix.ServerName)
	}

	if cfg.Database.MaxConns != 20 {
		t.Fatalf("database.max_conns = %d, want 20", cfg.Database.MaxConns)
	}

	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Fatalf("log = %s/%s, want debug/text", cfg.Log.Level, cfg.Log.Format)
	}
}

func TestLoadMissingExplicitFile(t *testing.T) {
	t.Parallel()

	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("Load() must fail when explicit config file is missing")
	}
}

func TestLoadUnknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addrx: \":1\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := config.Load(path); err == nil {
		t.Fatal("Load() must fail on unknown fields")
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name:    "empty addr",
			mutate:  func(c *config.Config) { c.Server.Addr = " " },
			wantErr: "server.addr is required",
		},
		{
			name:    "empty server_name",
			mutate:  func(c *config.Config) { c.Matrix.ServerName = "" },
			wantErr: "matrix.server_name is required",
		},
		{
			name:    "server_name with scheme",
			mutate:  func(c *config.Config) { c.Matrix.ServerName = "https://matrix.example.org" },
			wantErr: "bare hostname",
		},
		{
			name:    "server_name uppercase",
			mutate:  func(c *config.Config) { c.Matrix.ServerName = "Matrix.Example.org" },
			wantErr: "lowercase",
		},
		{
			name:    "bad log level",
			mutate:  func(c *config.Config) { c.Log.Level = "verbose" },
			wantErr: "log.level",
		},
		{
			name:    "bad log format",
			mutate:  func(c *config.Config) { c.Log.Format = "xml" },
			wantErr: "log.format",
		},
		{
			name:    "negative last_validated_interval",
			mutate:  func(c *config.Config) { c.Auth.LastValidatedInterval = config.Duration(-time.Second) },
			wantErr: "auth.last_validated_interval",
		},
		{
			name:    "negative activity_flush_interval",
			mutate:  func(c *config.Config) { c.Database.ActivityFlushInterval = config.Duration(-time.Second) },
			wantErr: "database.activity_flush_interval",
		},
		{
			name:    "zero read timeout",
			mutate:  func(c *config.Config) { c.Server.ReadTimeout = 0 },
			wantErr: "server.read_timeout must be positive",
		},
		{
			name:    "media backend s3 not implemented",
			mutate:  func(c *config.Config) { c.Media.StorageBackend = "s3" },
			wantErr: "not implemented",
		},
		{
			name:    "media backend unknown",
			mutate:  func(c *config.Config) { c.Media.StorageBackend = "ftp" },
			wantErr: "is not supported",
		},
		{
			name: "media local without dir",
			mutate: func(c *config.Config) {
				c.Media.StorageBackend = "local"
				c.Media.Directory = " "
			},
			wantErr: "media.directory is required",
		},
		{
			name:    "media negative max upload",
			mutate:  func(c *config.Config) { c.Media.MaxUploadBytes = -1 },
			wantErr: "media.max_upload_bytes",
		},
		{
			name:    "media negative max pending",
			mutate:  func(c *config.Config) { c.Media.MaxPendingUploads = -1 },
			wantErr: "media.max_pending_uploads",
		},
		{
			name:    "media bad legacy freeze",
			mutate:  func(c *config.Config) { c.Media.LegacyFreeze = "20.11.2026" },
			wantErr: "media.legacy_freeze",
		},
		{
			name:    "invalid dsn",
			mutate:  func(c *config.Config) { c.Database.DSN = "not-a-dsn" },
			wantErr: "database.dsn is invalid",
		},
		{
			name: "min conns greater than max",
			mutate: func(c *config.Config) {
				c.Database.DSN = "postgres://u:p@localhost:5432/db"
				c.Database.MaxConns = 2
				c.Database.MinConns = 5
			},
			wantErr: "database.min_conns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.Default()
			cfg.Env = "dev" // изолируем проверяемое правило от требования секрета
			cfg.Matrix.ServerName = "test.local"
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestStringHidesDSN(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Database.DSN = "postgres://user:secret@localhost:5432/syngo"

	out := cfg.String()

	if strings.Contains(out, "secret") {
		t.Fatalf("String() leaks credentials: %s", out)
	}

	if !strings.Contains(out, "db=configured") {
		t.Fatalf("String() = %q, want db=configured", out)
	}
}

func TestFederationSchemeValidation(t *testing.T) {
	t.Parallel()

	base := func(scheme string, insecure bool, env string) config.Config {
		cfg := config.Default()
		cfg.Env = env
		cfg.Server.Addr = ":8008"
		cfg.Matrix.ServerName = "a.test"
		cfg.Database.DSN = "postgres://localhost/syngo"
		cfg.Media.StorageBackend = "local"
		cfg.Federation.Enabled = true
		cfg.Federation.SigningKeyFile = "a.key"
		cfg.Federation.Scheme = scheme
		cfg.Federation.InsecureSkipVerify = insecure
		cfg.Sync.TokenSecret = "secret"

		return cfg
	}

	tests := []struct {
		name     string
		scheme   string
		insecure bool
		env      string
		wantErr  bool
	}{
		{name: "http для стенда", scheme: "http", env: "dev"},
		{name: "https по умолчанию", scheme: "", env: "dev"},
		{name: "неизвестная схема", scheme: "ftp", env: "dev", wantErr: true},
		{name: "insecure с http бессмыслен", scheme: "http", insecure: true, env: "dev", wantErr: true},
		{name: "insecure в prod запрещён", scheme: "https", insecure: true, env: "prod", wantErr: true},
		{name: "insecure в dev допустим", scheme: "https", insecure: true, env: "dev"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := base(tc.scheme, tc.insecure, tc.env).Validate()
			if tc.wantErr && err == nil {
				t.Fatal("ожидали ошибку валидации")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("неожидали ошибку: %v", err)
			}
		})
	}
}

// TestFederationIPRanges - ip_range_blacklist/whitelist - CIDR или IP,
// не заданный blacklist остается nil (диапазоны по умолчанию), [] - пустой.
func TestFederationIPRanges(t *testing.T) {
	cfg := validProd()
	cfg.Federation.Enabled = true
	cfg.Federation.SigningKeyFile = "a.key"

	cfg.Federation.IPRangeWhitelist = []string{"10.0.0.0/8", "fd00::1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("валидные диапазоны: %v", err)
	}

	cfg.Federation.IPRangeBlacklist = []string{"not-a-cidr"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ip_range_blacklist") {
		t.Fatalf("невалидный blacklist: %v", err)
	}

	t.Setenv(config.EnvName, "dev")

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "server:\n  addr: \":8008\"\nmatrix:\n  server_name: test.local\nfederation:\n  ip_range_blacklist: []\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Federation.IPRangeBlacklist == nil || len(loaded.Federation.IPRangeBlacklist) != 0 {
		t.Fatalf("[] должен давать пустой, а не nil список: %#v", loaded.Federation.IPRangeBlacklist)
	}

	if loaded.Federation.IPRangeWhitelist != nil {
		t.Fatalf("не заданный whitelist: %#v", loaded.Federation.IPRangeWhitelist)
	}
}

// TestFederationKeysConfig - trusted_key_servers в формате Synapse
// (имя или {server_name, verify_keys}) и old_signing_keys с проверкой ключей.
func TestFederationKeysConfig(t *testing.T) {
	t.Setenv(config.EnvName, "dev")

	const pub = "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik"

	body := "server:\n  addr: \":8008\"\nmatrix:\n  server_name: test.local\nfederation:\n" +
		"  trusted_key_servers:\n    - notary.test\n    - server_name: matrix.org\n      verify_keys:\n        \"ed25519:auto\": " + pub + "\n" +
		"  old_signing_keys:\n    \"ed25519:old\": {key: " + pub + ", expired_ts: 1600000000000}\n"

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	notaries := cfg.Federation.TrustedKeyServers
	if len(notaries) != 2 || notaries[0].ServerName != "notary.test" || notaries[1].VerifyKeys["ed25519:auto"] != pub {
		t.Fatalf("trusted_key_servers = %+v", notaries)
	}

	if old := cfg.Federation.OldSigningKeys["ed25519:old"]; old.Key != pub || old.ExpiredTS != 1_600_000_000_000 {
		t.Fatalf("old_signing_keys = %+v", cfg.Federation.OldSigningKeys)
	}

	bad := validProd()
	bad.Federation.Enabled = true
	bad.Federation.SigningKeyFile = "a.key"
	bad.Federation.OldSigningKeys = map[string]config.OldSigningKey{"ed25519:old": {Key: "short", ExpiredTS: 1}}

	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "old_signing_keys") {
		t.Fatalf("плохой old_signing_keys: %v", err)
	}

	bad.Federation.OldSigningKeys = map[string]config.OldSigningKey{"ed25519:old": {Key: pub}}
	if err := bad.Validate(); err == nil {
		t.Fatal("old_signing_keys без expired_ts принят")
	}

	bad.Federation.OldSigningKeys = nil
	bad.Federation.TrustedKeyServers = []config.TrustedKeyServer{{ServerName: ""}}

	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "trusted_key_servers") {
		t.Fatalf("нотарий без имени: %v", err)
	}
}

// TestInsecureSkipSignatureVerifyForbiddenInProd — стендовая опция не должна
// включаться в проде: иначе можно принимать неподписанные федеративные события.
func TestInsecureSkipSignatureVerifyForbiddenInProd(t *testing.T) {
	cfg := validProd()
	cfg.Federation.Enabled = true
	cfg.Federation.SigningKeyFile = "/tmp/key"
	cfg.Federation.ValidUntilDays = 7
	cfg.Federation.InsecureSkipSignatureVerify = true

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "insecure_skip_signature_verify") {
		t.Fatalf("Validate() = %v, ожидали ошибку про insecure_skip_signature_verify в prod", err)
	}
}

func TestRateLimitDisabledEnv(t *testing.T) {
	t.Setenv("SYNGOD_RATE_LIMIT_DISABLED", "1")

	t.Setenv("SYNGOD_ENV", "dev")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":8008\"\nmatrix:\n  server_name: test.local\nmedia:\n  storage_backend: local\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for name, action := range cfg.RateLimit.Actions() {
		if action.Enabled {
			t.Fatalf("%s включён при SYNGOD_RATE_LIMIT_DISABLED=1", name)
		}
	}
}

// prodSecret - секрет достаточной для prod длины.
const prodSecret = "0123456789abcdef0123456789abcdef"

// validProd - минимальный валидный prod-конфиг: от него тесты меняют
// одно поле и проверяют, что именно оно ломает валидацию.
func validProd() config.Config {
	cfg := config.Default()
	cfg.Env = "prod"
	cfg.Matrix.ServerName = "matrix.example.org"
	cfg.Database.DSN = "postgres://syngo:pw@db:5432/syngo?sslmode=disable"
	cfg.Sync.TokenSecret = prodSecret

	return cfg
}

// TestActivityDefaults - отметки активности пишутся пачкой раз в 10 с,
// last_validated_at токена - не чаще раза в 5 минут.
func TestActivityDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	if cfg.Database.ActivityFlushInterval.Std() != 10*time.Second || cfg.Auth.LastValidatedInterval.Std() != 5*time.Minute {
		t.Fatalf("умолчания: flush=%v last_validated=%v", cfg.Database.ActivityFlushInterval, cfg.Auth.LastValidatedInterval)
	}
}

func TestValidProdBaseline(t *testing.T) {
	t.Parallel()

	if err := validProd().Validate(); err != nil {
		t.Fatalf("Validate() = %v, базовый prod-конфиг должен быть валиден", err)
	}
}

// TestUnsafeFlagsForbiddenOutsideDev - пустой env (= prod) и prod с
// любым небезопасным флагом не проходят валидацию, даже при выключенной федерации.
// До исправления пустой env пропускал insecure_skip_signature_verify, а push.*
// от env не зависели вовсе.
func TestUnsafeFlagsForbiddenOutsideDev(t *testing.T) {
	t.Parallel()

	flags := map[string]func(*config.Config){
		"federation.insecure_skip_verify":           func(c *config.Config) { c.Federation.InsecureSkipVerify = true },
		"federation.insecure_skip_signature_verify": func(c *config.Config) { c.Federation.InsecureSkipSignatureVerify = true },
		"federation.send_full_member_state":         func(c *config.Config) { c.Federation.SendFullMemberState = true },
		"push.allow_http":                           func(c *config.Config) { c.Push.AllowHTTP = true },
		"push.allow_private":                        func(c *config.Config) { c.Push.AllowPrivate = true },
	}

	for flag, set := range flags {
		for _, env := range []string{"", "prod"} {
			for _, federation := range []bool{false, true} {
				cfg := validProd()
				cfg.Env = env
				cfg.Federation.Enabled = federation
				cfg.Federation.SigningKeyFile = "/etc/syngo/signing.key"
				set(&cfg)

				err := cfg.Validate()
				if err == nil || !strings.Contains(err.Error(), flag) {
					t.Errorf("env=%q federation=%t %s: Validate() = %v, ожидали запрет", env, federation, flag, err)
				}
			}
		}

		cfg := validProd()
		cfg.Env = "dev"
		set(&cfg)

		if err := cfg.Validate(); err != nil {
			t.Errorf("dev %s: Validate() = %v, в dev флаг допустим", flag, err)
		}
	}
}

// TestRegistrationClosedByDefault - регистрация закрыта по умолчанию,
// в том числе при загрузке конфига без секции auth.
func TestRegistrationClosedByDefault(t *testing.T) {
	if config.Default().Auth.RegistrationOpen {
		t.Fatal("auth.registration_open по умолчанию true, ожидали false")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("env: dev\nmatrix:\n  server_name: test.local\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if cfg.Auth.RegistrationOpen {
		t.Fatal("конфиг без auth открыл регистрацию")
	}
}

// TestProdServerName - локальные имена сервера вне dev/test запрещены
// (user_id @x:localhost необратимы), дефолта localhost нет.
func TestProdServerName(t *testing.T) {
	t.Parallel()

	if got := config.Default().Matrix.ServerName; got != "" {
		t.Fatalf("server_name по умолчанию %q, ожидали пусто", got)
	}

	for _, name := range []string{"localhost", "LOCALHOST.", "matrix.localhost", "127.0.0.1", "0.0.0.0"} {
		for _, env := range []string{"", "prod"} {
			cfg := validProd()
			cfg.Env = env
			cfg.Matrix.ServerName = name

			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "matrix.server_name") {
				t.Errorf("env=%q server_name=%q: Validate() = %v, ожидали отказ", env, name, err)
			}
		}
	}

	cfg := validProd()
	cfg.Env = "dev"
	cfg.Matrix.ServerName = "localhost"

	if err := cfg.Validate(); err != nil {
		t.Fatalf("dev localhost: Validate() = %v", err)
	}
}

// TestProdSecretsAndDSN - вне dev/test обязательны DSN и секреты не
// короче 32 символов (в том числе registration_shared_secret, если задан).
func TestProdSecretsAndDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{"без DSN", func(c *config.Config) { c.Database.DSN = "" }, "database.dsn is required"},
		{"секрет из одного символа", func(c *config.Config) { c.Sync.TokenSecret = "x" }, "sync.token_secret is too short"},
		{"секрет 31 символ", func(c *config.Config) { c.Sync.TokenSecret = prodSecret[:31] }, "sync.token_secret is too short"},
		{
			"короткий registration_shared_secret",
			func(c *config.Config) { c.Auth.RegistrationSharedSecret = "short" },
			"auth.registration_shared_secret is too short",
		},
	}

	for _, tt := range tests {
		cfg := validProd()
		tt.mutate(&cfg)

		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: Validate() = %v, ожидали %q", tt.name, err, tt.wantErr)
		}

		cfg.Env = "dev"
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s в dev: Validate() = %v, ошибки не ждали", tt.name, err)
		}
	}
}

// TestSecretsFromEnv - секреты задаются через окружение, не только файлом
// (у voip.secret переопределения не было).
func TestSecretsFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("voip:\n  secret: from-file\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("SYNGOD_ENV", "prod")
	t.Setenv("SYNGOD_MATRIX_SERVER_NAME", "matrix.example.org")
	t.Setenv("SYNGOD_DATABASE_DSN", "postgres://syngo:pw@db:5432/syngo")
	t.Setenv("SYNGOD_SYNC_TOKEN_SECRET", prodSecret)
	t.Setenv("SYNGOD_VOIP_SECRET", "voip-from-env")
	t.Setenv("SYNGOD_REGISTRATION_SHARED_SECRET", prodSecret+"-shared")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	if cfg.VoIP.Secret != "voip-from-env" {
		t.Fatalf("voip.secret = %q, ожидали значение из SYNGOD_VOIP_SECRET", cfg.VoIP.Secret)
	}

	if cfg.Auth.RegistrationSharedSecret != prodSecret+"-shared" {
		t.Fatalf("registration_shared_secret = %q, ожидали значение из окружения", cfg.Auth.RegistrationSharedSecret)
	}

	out := cfg.String()
	for _, secret := range []string{prodSecret, "voip-from-env", "pw@"} {
		if strings.Contains(out, secret) {
			t.Fatalf("String() раскрывает секрет %q: %s", secret, out)
		}
	}
}

// TestExampleConfig - config.example.yaml соответствует prod по умолчанию:
// без server_name/секретов не грузится, с ними из окружения - грузится, регистрация закрыта.
func TestExampleConfig(t *testing.T) {
	const example = "../../config.example.yaml"

	if _, err := config.Load(example); err == nil || !strings.Contains(err.Error(), "matrix.server_name") {
		t.Fatalf("Load(example) без окружения = %v, ожидали ошибку про server_name", err)
	}

	t.Setenv("SYNGOD_MATRIX_SERVER_NAME", "matrix.example.org")
	t.Setenv("SYNGOD_DATABASE_DSN", "postgres://syngo:pw@db:5432/syngo")
	t.Setenv("SYNGOD_SYNC_TOKEN_SECRET", prodSecret)

	cfg, err := config.Load(example)
	if err != nil {
		t.Fatalf("Load(example) = %v", err)
	}

	if cfg.Env != "prod" || cfg.Auth.RegistrationOpen {
		t.Fatalf("example: env=%q registration_open=%t, ожидали prod и закрытую регистрацию", cfg.Env, cfg.Auth.RegistrationOpen)
	}
}

// TestOpsListener - служебный листенер задается адресом (файл или
// SYNGOD_OPS_ADDR), pprof без него запрещен, адрес не совпадает с основным.
func TestOpsListener(t *testing.T) {
	cfg := validProd()
	cfg.Server.Pprof = true

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.ops_addr") {
		t.Fatalf("pprof без ops_addr: %v", err)
	}

	cfg.Server.OpsAddr = cfg.Server.Addr
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.ops_addr") {
		t.Fatalf("ops_addr == addr: %v", err)
	}

	cfg.Server.OpsAddr = "no-port"
	if err := cfg.Validate(); err == nil {
		t.Fatal("ops_addr без порта принят")
	}

	cfg.Server.OpsAddr = "127.0.0.1:9108"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("валидный ops_addr: %v", err)
	}

	t.Setenv(config.EnvOpsAddr, "10.0.0.5:9108")
	t.Setenv(config.EnvName, "dev")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":8008\"\nmatrix:\n  server_name: test.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Server.OpsAddr != "10.0.0.5:9108" {
		t.Fatalf("ops_addr из окружения: %q", loaded.Server.OpsAddr)
	}
}

// TestRedactionRetention - срок хранения оригинала удаленных событий
// по умолчанию 7 дней (как Synapse), отрицательный отвергается.
func TestRedactionRetention(t *testing.T) {
	t.Parallel()

	cfg := validProd()
	if got := cfg.Redaction.RetentionPeriod.Std(); got != 7*24*time.Hour {
		t.Fatalf("redaction.retention_period = %s, ожидали 168h", got)
	}

	cfg.Redaction.RetentionPeriod = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("retention_period=0 допустим: %v", err)
	}

	cfg.Redaction.RetentionPeriod = config.Duration(-time.Second)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "redaction.retention_period") {
		t.Fatalf("Validate() = %v, ожидали ошибку про redaction.retention_period", err)
	}
}

// TestLDAPConfig - auth.ldap в формате synapse-ldap3 (uri строкой или списком),
// password_pepper и проверка обязательных полей.
func TestLDAPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `env: dev
matrix:
  server_name: test.local
auth:
  password_pepper: "pep"
  password_localdb_enabled: false
  ldap:
    - uri: "ldap://ldap1.example.org:389"
      start_tls: true
      base: "ou=users,dc=example,dc=org"
      attributes: {uid: "uid", mail: "mail", name: "cn"}
      bind_dn: "cn=reader,dc=example,dc=org"
      bind_password: "test-only"
      filter: "(objectClass=posixAccount)"
    - uri: ["ldaps://a.example.net", "ldaps://b.example.net"]
      base: "dc=example,dc=net"
      attributes: {uid: "uid"}
      sync_displayname: false
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	ldap := cfg.Auth.LDAP
	if len(ldap) != 2 || len(ldap[0].URI) != 1 || len(ldap[1].URI) != 2 || !ldap[0].StartTLS ||
		ldap[0].Attributes.Name != "cn" || !ldap[0].SyncsDisplayName() || ldap[1].SyncsDisplayName() {
		t.Fatalf("auth.ldap = %+v", ldap)
	}

	if cfg.Auth.PasswordPepper != "pep" || cfg.Auth.LocalPasswordsEnabled() {
		t.Fatalf("pepper/localdb = %q %v", cfg.Auth.PasswordPepper, cfg.Auth.LocalPasswordsEnabled())
	}

	bad := cfg
	bad.Auth.LDAP = []config.LDAPProvider{{URI: config.StringList{"http://x"}, Base: "dc=x", Attributes: config.LDAPAttributes{UID: "uid"}}}

	if err := bad.Validate(); err == nil {
		t.Fatal("uri без ldap:// принят")
	}

	bad.Auth.LDAP = []config.LDAPProvider{{URI: config.StringList{"ldap://x"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("ldap без base и uid принят")
	}

	bad.Auth.LDAP = nil
	if err := bad.Validate(); err == nil {
		t.Fatal("password_localdb_enabled: false без LDAP принят")
	}
}
