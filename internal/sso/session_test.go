package sso_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/sso"
)

// TestSessionCodec - cookie сессии нельзя подделать, переиспользовать другим
// ключом или после срока.
func TestSessionCodec(t *testing.T) {
	t.Parallel()

	now := time.Now()
	codec := newCodec(t, func() time.Time { return now })

	session, err := codec.NewSession("oidc-kc", "https://app.example/", "uia-1")
	if err != nil {
		t.Fatal(err)
	}

	if session.State == "" || session.Nonce == "" || session.Verifier == "" || session.State == session.Nonce {
		t.Fatalf("сессия: %+v", session)
	}

	other, _ := codec.NewSession("oidc-kc", "", "")
	if other.State == session.State || other.Nonce == session.Nonce {
		t.Fatal("state/nonce повторяются")
	}

	value, err := codec.Encode(session)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := codec.Decode(value)
	if err != nil || decoded != session {
		t.Fatalf("Decode: %+v %v", decoded, err)
	}

	// Подмена любого байта - отказ.
	raw := []byte(value)
	raw[len(raw)/2] ^= 1

	for name, bad := range map[string]string{
		"подмена":       string(raw),
		"не base64":     "!!!",
		"короткая":      "AAAA",
		"пустая":        "",
		"чужой ключ":    mustEncode(t, newCodec(t, nil), session),
		"дописан хвост": value + "AA",
	} {
		if _, err := codec.Decode(bad); !errors.Is(err, sso.ErrSession) {
			t.Errorf("%s: %v", name, err)
		}
	}

	now = now.Add(sso.SessionTTL + time.Second)

	if _, err := codec.Decode(value); !errors.Is(err, sso.ErrSession) {
		t.Fatalf("истекшая сессия: %v", err)
	}

	if _, err := sso.NewSessionCodec([]byte("short"), nil); err == nil {
		t.Fatal("короткий ключ принят")
	}
}

func mustEncode(t *testing.T, codec *sso.SessionCodec, session sso.Session) string {
	t.Helper()

	value, err := codec.Encode(session)
	if err != nil {
		t.Fatal(err)
	}

	return value
}

// TestRedirectURL - адрес возврата клиента: опасные схемы отклоняются, whitelist
// сравнивается по границе пути, loginToken дописывается в query.
func TestRedirectURL(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"https://app.element.io/#/login", "element://vector/webapp/", "http://localhost:8080/"} {
		if _, err := sso.CheckRedirectURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}

	for _, bad := range []string{
		"", "/relative", "javascript:alert(1)", "JavaScript:alert(1)", "data:text/html,x",
		"vbscript:x", "file:///etc/passwd", "%zz", "https://x/" + strings.Repeat("a", sso.MaxRedirectURLLength),
	} {
		if _, err := sso.CheckRedirectURL(bad); err == nil {
			t.Errorf("%q принят", bad)
		}
	}

	whitelist := []string{"https://app.element.io/", "https://chat.example.org", ""}

	cases := map[string]bool{
		"https://app.element.io/#/login":            true,
		"https://chat.example.org":                  true,
		"https://chat.example.org/x":                true,
		"https://chat.example.org?x=1":              true,
		"https://chat.example.org.evil.com/":        false,
		"https://app.element.io.evil.com/":          false,
		"https://evil.com/?https://app.element.io/": false,
	}

	for target, want := range cases {
		if got := sso.Whitelisted(target, whitelist); got != want {
			t.Errorf("Whitelisted(%q) = %v", target, got)
		}
	}

	target, _ := url.Parse("https://app.element.io/?a=1#/login")
	if got := sso.AddLoginToken(target, "syl_x"); got != "https://app.element.io/?a=1&loginToken=syl_x#/login" {
		t.Fatalf("AddLoginToken = %s", got)
	}
}
