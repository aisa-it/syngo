// Package signing реализует ключи сервера и подписи Matrix: ed25519, канонический JSON,
// заголовок X-Matrix и подписанный ответ /_matrix/key/v2/server (ADR-0007 §3).
//
// Сервер не проверяет криптографию чужих ключей глубже подписи: он хранит и раздаёт
// то, что прислали другие серверы, и подписывает собственные ответы.
package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Ошибки подписи/проверки.
var (
	ErrInvalidKey    = errors.New("signing: invalid key")
	ErrInvalidSig    = errors.New("signing: invalid signature")
	ErrKeyExpired    = errors.New("signing: key expired")
	ErrBadMatrixAuth = errors.New("signing: bad X-Matrix authorization")
)

// Key — пара ключей сервера: key_id вида ed25519:<version> и приватный ключ.
type Key struct {
	KeyID   string
	Private ed25519.PrivateKey
}

// Public возвращает публичный ключ в base64 (без паддинга, как в Matrix).
func (k Key) Public() string {
	public, ok := k.Private.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}

	return base64.RawStdEncoding.EncodeToString(public)
}

// Sign подписывает сообщение.
func (k Key) Sign(msg []byte) string {
	return base64.RawStdEncoding.EncodeToString(ed25519.Sign(k.Private, msg))
}

// Generate создаёт новый ключ с указанной версией (например, "a_1").
func Generate(version string) (Key, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return Key{}, fmt.Errorf("signing: generate: %w", err)
	}

	_ = pub

	return Key{KeyID: "ed25519:" + version, Private: priv}, nil
}

// EncodeKeyFile сериализует ключ в формат Synapse: «ed25519 <version> <base64-seed>»
// (unpadded base64, как пишет Synapse generate_signing_key).
func EncodeKeyFile(key Key) (string, error) {
	version, ok := strings.CutPrefix(key.KeyID, "ed25519:")
	if !ok {
		return "", fmt.Errorf("signing: key id %q must be ed25519:<version>", key.KeyID)
	}

	seed := key.Private.Seed()

	return fmt.Sprintf("ed25519 %s %s\n", version, base64.RawStdEncoding.EncodeToString(seed)), nil
}

// LoadKeyFile читает ключ из файла формата `ed25519 <version> <base64-seed>`.
//
// Формат совместим с Synapse signing key file, поэтому оператор может перенести
// существующий ключ сервера: Synapse пишет seed без паддинга, файл может содержать
// несколько строк-ключей - используется первая (как signing_key[0] в Synapse).
// Файл обязан быть недоступен другим (chmod 600).
func LoadKeyFile(path string) (Key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Key{}, fmt.Errorf("signing: stat key %s: %w", path, err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("signing: key %s must not be readable by group/others (chmod 600)", path)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // путь задаёт оператор через конфиг
	if err != nil {
		return Key{}, fmt.Errorf("signing: read key: %w", err)
	}

	return ParseKeyFile(string(raw))
}

// ParseKeyFile разбирает содержимое файла ключа (первая непустая строка).
func ParseKeyFile(content string) (Key, error) {
	var fields []string

	for _, line := range strings.Split(content, "\n") {
		if fields = strings.Fields(line); len(fields) > 0 {
			break
		}
	}

	if len(fields) != 3 || fields[0] != "ed25519" || fields[1] == "" {
		return Key{}, fmt.Errorf("signing: %w: want «ed25519 <version> <base64-seed>»", ErrInvalidKey)
	}

	seed, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(fields[2], "="))
	if err != nil || len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("signing: %w: bad seed", ErrInvalidKey)
	}

	return Key{KeyID: "ed25519:" + fields[1], Private: ed25519.NewKeyFromSeed(seed)}, nil
}

// VerifyEd25519 проверяет подпись по публичному ключу (base64 без паддинга).
func VerifyEd25519(publicBase64, message, signatureBase64 string) error {
	public, err := base64.RawStdEncoding.DecodeString(publicBase64)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return fmt.Errorf("signing: %w: bad public key", ErrInvalidKey)
	}

	signature, err := base64.RawStdEncoding.DecodeString(signatureBase64)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signing: %w", ErrInvalidSig)
	}

	if !ed25519.Verify(ed25519.PublicKey(public), []byte(message), signature) {
		return ErrInvalidSig
	}

	return nil
}
