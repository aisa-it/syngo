package auth_test

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/aisa-it/syngo/internal/auth"
)

func TestHashAndVerify(t *testing.T) {
	t.Parallel()

	hasher := auth.NewArgon2Hasher()

	hash, err := hasher.Hash("s3cret-Passw0rd")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=") {
		t.Fatalf("Hash() = %q, want PHC argon2id с параметрами", hash)
	}

	ok, err := hasher.Verify(hash, "s3cret-Passw0rd")
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if !ok {
		t.Fatal("Verify() = false для верного пароля")
	}

	ok, err = hasher.Verify(hash, "wrong")
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if ok {
		t.Fatal("Verify() = true для неверного пароля")
	}
}

func TestHashUsesUniqueSalt(t *testing.T) {
	t.Parallel()

	hasher := auth.NewArgon2Hasher()

	first, err := hasher.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}

	second, err := hasher.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash() error: %v", err)
	}

	if first == second {
		t.Fatal("хеши одинаковых паролей совпали: соль не уникальна")
	}
}

func TestVerifyRejectsBrokenHash(t *testing.T) {
	t.Parallel()

	hasher := auth.NewArgon2Hasher()

	for _, hash := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=1$!!!$a2V5",
		"$argon2id$v=18$m=65536,t=3,p=1$c2FsdA$a2V5",
	} {
		if _, err := hasher.Verify(hash, "password"); err == nil {
			t.Fatalf("Verify(%q) ожидал ошибку", hash)
		}
	}
}

func TestTokenSource(t *testing.T) {
	t.Parallel()

	source := auth.NewTokenSource()

	plain, hash, err := source.NewToken()
	if err != nil {
		t.Fatalf("NewToken() error: %v", err)
	}

	if !strings.HasPrefix(plain, "syt_") {
		t.Fatalf("токен = %q, ожидали префикс syt_", plain)
	}

	if len(hash) != 32 {
		t.Fatalf("len(hash) = %d, ожидали 32 байта SHA-256", len(hash))
	}

	other, _, err := source.NewToken()
	if err != nil {
		t.Fatalf("NewToken() error: %v", err)
	}

	if plain == other {
		t.Fatal("два вызова NewToken() вернули одинаковый токен")
	}
}

// TestMultiHasherBcrypt — реальный bcrypt-хеш (как в Synapse) проверяется и требует rehash.
func TestMultiHasherBcrypt(t *testing.T) {
	t.Parallel()

	hasher := auth.NewMultiHasher()

	bcryptHash, err := bcrypt.GenerateFromPassword([]byte("synapse-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	ok, err := hasher.Verify(string(bcryptHash), "synapse-pass")
	if err != nil || !ok {
		t.Fatalf("bcrypt verify = %v, err = %v", ok, err)
	}

	ok, err = hasher.Verify(string(bcryptHash), "wrong")
	if err != nil {
		t.Fatalf("bcrypt verify (wrong): %v", err)
	}

	if ok {
		t.Fatal("bcrypt verify принял неверный пароль")
	}

	if !hasher.NeedsRehash(string(bcryptHash)) {
		t.Fatal("bcrypt-хеш должен требовать rehash")
	}
}

func TestMultiHasherArgonUnchanged(t *testing.T) {
	t.Parallel()

	hasher := auth.NewMultiHasher()

	hash, err := hasher.Hash("new-pass")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	if hasher.NeedsRehash(hash) {
		t.Fatalf("argon2id с нашими параметрами не должен требовать rehash: %s", hash)
	}

	ok, err := hasher.Verify(hash, "new-pass")
	if err != nil || !ok {
		t.Fatalf("argon verify = %v, err = %v", ok, err)
	}
}

func TestMultiHasherRejectsUnknownAndExpensive(t *testing.T) {
	t.Parallel()

	hasher := auth.NewMultiHasher()

	if _, err := hasher.Verify("plain:123", "123"); err == nil {
		t.Fatal("неизвестный формат хеша должен давать ошибку")
	}

	if _, err := hasher.Verify("", "123"); err == nil {
		t.Fatal("пустой хеш должен давать ошибку")
	}

	// bcrypt с cost 16 > лимита 15: отвергаем до вычисления (защита от DoS).
	expensive, err := bcrypt.GenerateFromPassword([]byte("x"), 16)
	if err != nil {
		t.Fatalf("bcrypt cost 16: %v", err)
	}

	if _, err := hasher.Verify(string(expensive), "x"); err == nil {
		t.Fatal("bcrypt с cost > 15 должен отвергаться")
	}
}

// synapseVectors - хеши, посчитанные кодом Synapse 1.165 (AuthHandler.hash,
// bcrypt_rounds=4): NFKC-нормализация, pepper в конце, обрезка до 72 байт.
var synapseVectors = []struct {
	name, pepper, password, hash string
	// alsoValid - другие пароли, которые Synapse принимает для того же хеша.
	alsoValid []string
	invalid   []string
}{
	{
		name: "без pepper", password: "plain-password",
		hash:    "$2b$04$3C4k4nW.yHOWJzwAxPiTweN.CLuiRvza6Cli8Cj1XBEaX4gi.fZzy",
		invalid: []string{"plain-passwordX"},
	},
	{
		name: "pepper", pepper: "fixture-pepper", password: "plain-password",
		hash: "$2b$04$rL/f8mgvzRG3UJHrbYnkFeMQVZilUO0GHlT8d.GKbyopw383rfQpi",
	},
	{
		name: "NFKC лигатура", password: "ﬁxture-Пароль-1",
		hash:      "$2b$04$MVoHLnTy2eyXRFOUCSqgk.mefLLwkxqwoiQQfPEITCpSQBEhLOpMK",
		alsoValid: []string{"fixture-Пароль-1"},
		invalid:   []string{"fixture-пароль-1"},
	},
	{
		name: "NFKC полноширинные и pepper", pepper: "pep", password: strings.Repeat("Ａ", 30),
		hash:      "$2b$04$gQMMwzHPBZRaxPpbnAMlUuL6UfJ3kxOqsF9HnTQsggcx8xpttEe86",
		alsoValid: []string{strings.Repeat("A", 30)},
	},
	{
		// 70 байт пароля + pepper: учитываются только первые 72 байта.
		name: "длиннее 72 байт", pepper: "pepper-tail", password: strings.Repeat("x", 70),
		hash: "$2b$04$lAgjB7Z5BwUaFJWE/490Cezj3ACd4JHRjHx9a/GWzmQiAfEohI9BK",
	},
}

// TestMultiHasherSynapseVectors - пароли Synapse с pepper и не-NFKC символами
// проверяются так же, как в Synapse.
func TestMultiHasherSynapseVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range synapseVectors {
		hasher := auth.NewMultiHasher().WithBcryptPepper(vec.pepper)

		for _, password := range append([]string{vec.password}, vec.alsoValid...) {
			ok, err := hasher.Verify(vec.hash, password)
			if err != nil || !ok {
				t.Errorf("%s: Verify(%q) = %v, %v; Synapse принимает этот пароль", vec.name, password, ok, err)
			}
		}

		for _, password := range vec.invalid {
			if ok, _ := hasher.Verify(vec.hash, password); ok {
				t.Errorf("%s: Verify(%q) = true, ожидали отказ", vec.name, password)
			}
		}

		if vec.pepper != "" {
			if ok, _ := auth.NewMultiHasher().Verify(vec.hash, vec.password); ok {
				t.Errorf("%s: без pepper пароль не должен подходить", vec.name)
			}
		}
	}
}

// TestMultiHasherNFKCAfterRehash - после пере-хеширования в argon2id пароль
// по-прежнему принимается в любой NFKC-эквивалентной форме, как в Synapse.
func TestMultiHasherNFKCAfterRehash(t *testing.T) {
	t.Parallel()

	hasher := auth.NewMultiHasher()

	hash, err := hasher.Hash("ﬁxture-Пароль-1")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	for _, password := range []string{"ﬁxture-Пароль-1", "fixture-Пароль-1"} {
		if ok, err := hasher.Verify(hash, password); err != nil || !ok {
			t.Fatalf("Verify(%q) = %v, %v", password, ok, err)
		}
	}

	// Хеш, созданный до нормализации (пароль как есть), продолжает работать.
	legacy, err := auth.NewArgon2Hasher().Hash("ﬁxture")
	if err != nil {
		t.Fatalf("legacy Hash: %v", err)
	}

	if ok, err := hasher.Verify(legacy, "ﬁxture"); err != nil || !ok {
		t.Fatalf("Verify(legacy) = %v, %v", ok, err)
	}
}
