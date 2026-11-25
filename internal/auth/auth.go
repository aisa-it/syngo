// Package auth реализует криптографические примитивы аутентификации:
// хеширование паролей argon2id (PHC-строка) и выдачу непредсказуемых токенов.
//
// Пакет зависит только от internal/core (реализует его интерфейсы), поэтому его
// можно подменять в тестах предсказуемыми реализациями.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/unicode/norm"

	"github.com/aisa-it/syngo/internal/core"
)

// Параметры argon2id. m=64 МиБ на хеш — как recommended в спеке Matrix
// (Synapse использует argon2id с сопоставимыми параметрами); p зависит от CPU.
const (
	argonMemoryKiB  = 64 * 1024
	argonTime       = 3
	argonKeyLength  = 32
	argonSaltLength = 16
	// TokenBytes — длина случайной части токена (256 бит).
	TokenBytes = 32
	// tokenPrefix — префикс access-токена в стиле Synapse (syt_).
	tokenPrefix = "syt_"
)

// maxConcurrentArgon — сколько вычислений argon2id идет одновременно. Каждое берет
// 64 МиБ: без ограничения 16 параллельных входов при лимите контейнера 1 ГБ давали
// OOM. Остальные ждут в очереди.
const maxConcurrentArgon = 4

var (
	argonSlots    = make(chan struct{}, maxConcurrentArgon)
	argonInFlight atomic.Int32
	argonPeak     atomic.Int32
)

// argonKey вычисляет argon2id, занимая слот семафора.
func argonKey(password, salt []byte, time, memoryKiB uint32, threads uint8, keyLength uint32) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()

	n := argonInFlight.Add(1)
	defer argonInFlight.Add(-1)

	for {
		peak := argonPeak.Load()
		if n <= peak || argonPeak.CompareAndSwap(peak, n) {
			break
		}
	}

	return argon2.IDKey(password, salt, time, memoryKiB, threads, keyLength)
}

// ErrInvalidHash — строка не является корректной PHC-записью argon2id.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// Argon2Hasher — реализация core.PasswordHasher на argon2id.
type Argon2Hasher struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	keyLength uint32
}

// NewArgon2Hasher создаёт хешер с параметрами по умолчанию.
func NewArgon2Hasher() *Argon2Hasher {
	threads := uint8(min(runtime.NumCPU(), 4)) //nolint:gosec // NumCPU не превышает 255 в разумных пределах
	if threads == 0 {
		threads = 1
	}

	return &Argon2Hasher{
		memoryKiB: argonMemoryKiB,
		time:      argonTime,
		threads:   threads,
		keyLength: argonKeyLength,
	}
}

// Hash возвращает PHC-строку вида $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>.
func (h *Argon2Hasher) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	key := argonKey(
		[]byte(password), salt,
		h.time, h.memoryKiB, h.threads, h.keyLength,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.memoryKiB, h.time, h.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// Verify сравнивает пароль с PHC-строкой за постоянное время.
func (h *Argon2Hasher) Verify(encodedHash, password string) (bool, error) {
	params, salt, key, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	computed := argonKey(
		[]byte(password), salt,
		params.time, params.memoryKiB, params.threads, uint32(len(key)), //nolint:gosec // len(key) — небольшая константа из PHC
	)

	return subtle.ConstantTimeCompare(key, computed) == 1, nil
}

// hashParams — параметры, вычитанные из PHC-строки.
type hashParams struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
}

// decodeHash разбирает PHC-строку argon2id.
func decodeHash(encoded string) (params hashParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return hashParams{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return hashParams{}, nil, nil, ErrInvalidHash
	}

	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memoryKiB, &params.time, &params.threads); err != nil {
		return hashParams{}, nil, nil, ErrInvalidHash
	}

	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return hashParams{}, nil, nil, ErrInvalidHash
	}

	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return hashParams{}, nil, nil, ErrInvalidHash
	}

	return params, salt, key, nil
}

// TokenSource — реализация core.TokenSource: crypto/rand + SHA-256.
type TokenSource struct{}

// NewTokenSource создаёт источник токенов.
func NewTokenSource() *TokenSource {
	return &TokenSource{}
}

// NewToken возвращает открытый токен (с префиксом syt_) и его SHA-256.
func (t *TokenSource) NewToken() (plain string, hash []byte, err error) {
	buf := make([]byte, TokenBytes)
	if _, err = rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}

	plain = tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)

	return plain, core.HashToken(plain), nil
}

// maxBcryptCost ограничивает стоимость проверки: хеш с cost>max — отвергаем, иначе
// один запрос с подделанным хешем занимает CPU на секунды (DoS).
const maxBcryptCost = 15

// MultiHasher проверяет пароли в обоих форматах: argon2id (наш) и bcrypt (Synapse).
//
// Формат определяется по префиксу строки, поэтому миграция не требует схемы:
// старые bcrypt-хеши продолжают работать, а после успешного входа пере-хешируются
// в argon2id (NeedsRehash + core.Auth).
//
// Пароль перед хешированием приводится к NFKC, к bcrypt дописывается pepper - как в
// Synapse (handlers/auth.py: hash, validate_hash; password_config.pepper). Без этого
// пароли с pepper или с не-NFKC символами после переезда не подходили.
type MultiHasher struct {
	argon *Argon2Hasher
	// bcryptPepper - password_config.pepper Synapse, участвует только в bcrypt.
	bcryptPepper string
}

// NewMultiHasher создаёт хешер с argon2id для новых паролей.
func NewMultiHasher() *MultiHasher {
	return &MultiHasher{argon: NewArgon2Hasher()}
}

// WithBcryptPepper задает pepper для проверки bcrypt-хешей Synapse.
func (h *MultiHasher) WithBcryptPepper(pepper string) *MultiHasher {
	h.bcryptPepper = pepper

	return h
}

// Hash всегда возвращает argon2id от NFKC-формы пароля: новые пароли пишем только
// в современном формате.
func (h *MultiHasher) Hash(password string) (string, error) {
	return h.argon.Hash(norm.NFKC.String(password))
}

// Verify проверяет пароль в формате, определённом по префиксу хеша.
func (h *MultiHasher) Verify(encodedHash, password string) (bool, error) {
	normalized := norm.NFKC.String(password)

	switch {
	case strings.HasPrefix(encodedHash, "$argon2id$"):
		ok, err := h.argon.Verify(encodedHash, normalized)
		if err != nil || ok || normalized == password {
			return ok, err
		}

		// Хеши, созданные до нормализации, - от пароля как есть.
		return h.argon.Verify(encodedHash, password)
	case strings.HasPrefix(encodedHash, "$2a$"), strings.HasPrefix(encodedHash, "$2b$"), strings.HasPrefix(encodedHash, "$2y$"):
		return verifyBcrypt(encodedHash, normalized+h.bcryptPepper)
	default:
		// Пустой хеш (пользователь LDAP или SSO без локального пароля) и неизвестные форматы.
		return false, ErrInvalidHash
	}
}

// NeedsRehash сообщает, что хеш надо заменить на argon2id при следующем входе.
func (h *MultiHasher) NeedsRehash(encodedHash string) bool {
	if strings.HasPrefix(encodedHash, "$argon2id$") {
		// argon2id с нашими параметрами — не трогаем; с другими (старые настройки) — обновим.
		params, _, _, err := decodeHash(encodedHash)
		if err != nil {
			return false
		}

		return params.memoryKiB != argonMemoryKiB || params.time != argonTime
	}

	return strings.HasPrefix(encodedHash, "$2a$") ||
		strings.HasPrefix(encodedHash, "$2b$") ||
		strings.HasPrefix(encodedHash, "$2y$")
}

// bcryptMaxPassword - bcrypt учитывает только первые 72 байта; Synapse обрезает
// пароль с pepper до 72 байт явно, x/crypto на длинном пароле возвращает ошибку.
const bcryptMaxPassword = 72

// verifyBcrypt проверяет bcrypt-хеш Synapse с ограничением стоимости. password -
// уже NFKC-форма с pepper.
func verifyBcrypt(encodedHash, password string) (bool, error) {
	cost, err := bcrypt.Cost([]byte(encodedHash))
	if err != nil {
		return false, fmt.Errorf("auth: invalid bcrypt hash: %w", err)
	}

	if cost > maxBcryptCost {
		return false, fmt.Errorf("auth: bcrypt cost %d exceeds limit %d", cost, maxBcryptCost)
	}

	secret := []byte(password)
	if len(secret) > bcryptMaxPassword {
		secret = secret[:bcryptMaxPassword]
	}

	if err := bcrypt.CompareHashAndPassword([]byte(encodedHash), secret); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}

		return false, fmt.Errorf("auth: verify bcrypt: %w", err)
	}

	return true, nil
}
