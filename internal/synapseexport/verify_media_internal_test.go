package synapseexport

import (
	"strings"
	"testing"
)

// TestMediaDiff - сверка медиа ловит размер, карантин, sha256 и
// неперенесенные байты; sha256, которого Synapse не считал, не сравнивается.
func TestMediaDiff(t *testing.T) {
	t.Parallel()

	cases := []struct {
		src, dst, want string
	}{
		{"10|false|aa", "10|false|aa", ""},
		{"10|false|", "10|false|bb", ""},
		{absent, "10|false|aa", "нет в Synapse"},
		{"10|false|aa", absent, "нет у нас"},
		{"10|true|aa", "12|false|aa", "размер synapse=10 syngo=12, карантин synapse=true syngo=false"},
		{"10|false|aa", "10|false|bb", "sha256 synapse=aa syngo=bb"},
		{"10|false|aa", "10|false|", "байты не перенесены"},
		{"broken", "10|false|aa", "synapse=broken"},
	}

	for _, c := range cases {
		got := mediaDiff(c.src, c.dst)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("mediaDiff(%q, %q) = %q, ожидали %q", c.src, c.dst, got, c.want)
		}
	}
}

// TestVerifyReportText - отчет показывает медиа, пользователей и алиасы.
func TestVerifyReportText(t *testing.T) {
	t.Parallel()

	report := VerifyReport{MediaChecked: true, Media: 3, MediaURLCache: 2, Users: 4, Aliases: 1}
	text := report.Text()

	for _, want := range []string{"media: rows=3 diffs=0 url_cache_skipped=2", "users: rows=4", "aliases: rows=1", "result: OK"} {
		if !strings.Contains(text, want) {
			t.Fatalf("в отчете нет %q:\n%s", want, text)
		}
	}

	report = VerifyReport{}
	report.problem("x")

	if text := report.Text(); !strings.Contains(text, "media: not checked") || !strings.Contains(text, "result: MISMATCH (1)") {
		t.Fatalf("отчет:\n%s", text)
	}
}
