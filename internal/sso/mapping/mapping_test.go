package mapping_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/sso/mapping"
)

// TestTemplateRender - шаблоны user_mapping_provider Synapse.
func TestTemplateRender(t *testing.T) {
	t.Parallel()

	claims := map[string]any{
		"preferred_username": "Ivan.Petrov",
		"email":              "ivan@example.org",
		"name":               "Иван Петров",
		"sub":                float64(12345),
		"address":            map[string]any{"locality": "Москва"},
		"admin":              true,
		"big":                json.Number("12345678901234567890"),
	}

	cases := map[string]string{
		"{{ user.preferred_username }}":                 "Ivan.Petrov",
		"{{user.preferred_username|lower}}":             "ivan.petrov",
		"{{ user.email | localpart_from_email }}":       "ivan",
		"{{ user.name }} ({{ user.address.locality }})": "Иван Петров (Москва)",
		"id-{{ user.sub }}":                             "id-12345",
		"{{ user.missing }}":                            "",
		"{{ user.name.first }}":                         "",
		"{{ user.admin | upper }}":                      "TRUE",
		"{{ user.big }}":                                "12345678901234567890",
		"  {{ user.email | trim }}  ":                   "  ivan@example.org  ",
		"":                                              "",
	}

	for source, want := range cases {
		tmpl, err := mapping.Parse(source)
		if err != nil {
			t.Fatalf("Parse(%q): %v", source, err)
		}

		if got := tmpl.Render(claims); got != want {
			t.Errorf("Render(%q) = %q, want %q", source, got, want)
		}
	}

	if empty, _ := mapping.Parse(""); !empty.Empty() {
		t.Fatal("пустой шаблон не Empty")
	}

	var nilTemplate *mapping.Template
	if nilTemplate.Render(claims) != "" || !nilTemplate.Empty() {
		t.Fatal("nil-шаблон")
	}

	if mapping.Claim(claims, "address.locality") != "Москва" || mapping.Claim(claims, "sub") != "12345" {
		t.Fatal("Claim")
	}

	if mapping.ClaimString([]any{"x"}) != "" {
		t.Fatal("список не строка")
	}
}

// TestTemplateParseErrors - неподдержанный синтаксис Jinja отклоняется при разборе
// конфига, а не рендерится в пустой localpart.
func TestTemplateParseErrors(t *testing.T) {
	t.Parallel()

	for _, source := range []string{
		"{{ user.email",
		"{{ email }}",
		"{{ user.email.split('@')[0] }}",
		"{{ user.email | replace('a','b') }}",
		"{% if user.x %}x{% endif %}",
		"x }}",
		"{{ user.a }} }}",
		"{{ user['x'] }}",
	} {
		if _, err := mapping.Parse(source); !errors.Is(err, mapping.ErrTemplate) {
			t.Errorf("Parse(%q) = %v, ожидали ErrTemplate", source, err)
		}
	}
}

// TestMXIDLocalpart - векторы map_username_to_mxid_localpart Synapse
// (tests/test_types.py MapUsernameTestCase).
func TestMXIDLocalpart(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"test1234":         "test1234",
		"test-1234":        "test-1234",
		"TEST":             "test",
		"test=1234":        "test=3d1234",
		"_test":            "=5ftest",
		"test_":            "test_",
		"têst":             "t=c3=aast",
		"Ivan.Petrov":      "ivan.petrov",
		"ivan@example.org": "ivan=40example.org",
		"ÄBC":              "=c3=84bc",
		"a b":              "a=20b",
		"user/x+y":         "user/x+y",
		"":                 "",
		"Ünïcode-Ä_Ok1":    "=c3=9cn=c3=afcode-=c3=84_ok1",
	}

	for in, want := range cases {
		if got := mapping.MXIDLocalpart(in); got != want {
			t.Errorf("MXIDLocalpart(%q) = %q, want %q", in, got, want)
		}
	}
}
