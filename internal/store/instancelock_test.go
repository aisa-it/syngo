package store

import (
	"strings"
	"testing"
)

func TestInstanceApplicationName(t *testing.T) {
	t.Parallel()

	for _, ident := range []string{"host=node pid=1 id=abc", strings.Repeat("x", 63-len(instanceLockAppPrefix))} {
		if got := instanceApplicationName(ident); got != instanceLockAppPrefix+ident {
			t.Fatalf("метка в пределах лимита изменилась: %q", got)
		}
	}

	for _, host := range []string{strings.Repeat("node-", 13), "узел", strings.Repeat("узел", 16)} {
		prefix := "host=" + host + " pid=1 id="
		name := instanceApplicationName(prefix + "first")
		if len(name) > 63 || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r > '~' }) || !strings.HasPrefix(name, instanceLockAppPrefix+"host=") {
			t.Fatalf("некорректная метка PostgreSQL: %q", name)
		}
		if name != instanceApplicationName(prefix+"first") || name == instanceApplicationName(prefix+"second") {
			t.Fatalf("метка не идентифицирует исходный экземпляр: %q", name)
		}
	}
}
