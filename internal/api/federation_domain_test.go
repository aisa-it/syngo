package api

import "testing"

// TestDomainOfServerNameWithPort - server_name идентификатора - все
// после первого двоеточия, порт входит в него.
func TestDomainOfServerNameWithPort(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"@a:example.com:8448": "example.com:8448",
		"@a:example.com":      "example.com",
		"!room:[::1]:8448":    "[::1]:8448",
		"$ev:1.2.3.4":         "1.2.3.4",
		"no-colon":            "",
	}

	for id, want := range cases {
		if got := domainOf(id); got != want {
			t.Errorf("domainOf(%q) = %q, ожидали %q", id, got, want)
		}

		if got := domainOfID(id); got != want {
			t.Errorf("domainOfID(%q) = %q, ожидали %q", id, got, want)
		}

		if got := domainOfIdentifier(id); got != want {
			t.Errorf("domainOfIdentifier(%q) = %q, ожидали %q", id, got, want)
		}
	}
}
