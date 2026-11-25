package build

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSemverAndString(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v1.165.0"
	if got := Semver(); got != "1.165.0" {
		t.Fatalf("Semver = %q", got)
	}

	if got := String(); !strings.HasPrefix(got, "syngo v1.165.0 (Synapse "+SynapseCompat+" compatible") {
		t.Fatalf("String = %q", got)
	}

	Version = "dev"
	if got := Semver(); got != "dev" {
		t.Fatalf("Semver(dev) = %q", got)
	}
}

func TestSourceURL(t *testing.T) {
	old := Commit
	t.Cleanup(func() { Commit = old })

	Commit = "abc123"
	if got, want := SourceURL(), RepositoryURL+"/tree/abc123"; got != want {
		t.Fatalf("SourceURL = %q, want %q", got, want)
	}

	for _, commit := range []string{"unknown", "UNKNOWN", "", "  "} {
		Commit = commit
		if got := SourceURL(); got != RepositoryURL {
			t.Fatalf("SourceURL(%q) = %q, want %q", commit, got, RepositoryURL)
		}
	}
}

// TestSynapseCompatMatchesStands - версия совместимости совпадает с образом Synapse
// интероп- и нагрузочного стендов и с фикстурами импорта: сменить SynapseCompat,
// не перепроверив совместимость на новой версии Synapse, нельзя.
func TestSynapseCompatMatchesStands(t *testing.T) {
	root := filepath.Join("..", "..")
	image := regexp.MustCompile(`matrixdotorg/synapse:v(\d+\.\d+)\.\d+`)

	for _, f := range []string{"deploy/stand/docker-compose.synapse.yml", "deploy/loadtest/docker-compose.synapse.yml"} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}

		m := image.FindSubmatch(data)
		if len(m) < 2 || string(m[1]) != SynapseCompat {
			t.Fatalf("%s: образ Synapse %q, SynapseCompat %s", f, m, SynapseCompat)
		}
	}

	for _, dir := range []string{"cmd/syngo-import/testdata", "internal/importevents/testdata"} {
		matches, err := filepath.Glob(filepath.Join(root, dir, "synapse-"+SynapseCompat+"*.sql"))
		if err != nil || len(matches) == 0 {
			t.Fatalf("%s: нет фикстур synapse-%s*.sql", dir, SynapseCompat)
		}
	}
}
