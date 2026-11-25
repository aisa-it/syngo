package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aisa-it/syngo/internal/build"
)

// TestFederationVersionIsImplementation - /_matrix/federation/v1/version отдает имя,
// версию, лицензию и ссылку на соответствующий исходный код.
func TestFederationVersionIsImplementation(t *testing.T) {
	oldVersion, oldCommit := build.Version, build.Commit
	t.Cleanup(func() { build.Version, build.Commit = oldVersion, oldCommit })

	build.Version = "v1.165.0"
	build.Commit = "abc123"

	rec := httptest.NewRecorder()
	(&Server{}).handleFederationVersion(rec, httptest.NewRequest(http.MethodGet, "/_matrix/federation/v1/version", http.NoBody))

	var body struct {
		Server struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			License   string `json:"license"`
			SourceURL string `json:"source_url"`
		} `json:"server"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело: %v %s", err, rec.Body.String())
	}

	if body.Server.Name != "syngo" || body.Server.Version != "1.165.0" {
		t.Fatalf("server = %+v", body.Server)
	}

	if body.Server.License != "AGPL-3.0-or-later" {
		t.Fatalf("license = %q", body.Server.License)
	}

	if want := build.RepositoryURL + "/tree/abc123"; body.Server.SourceURL != want {
		t.Fatalf("source_url = %q, want %q", body.Server.SourceURL, want)
	}
}

func TestSourceOfferAvailableWhenFederationDisabled(t *testing.T) {
	srv := newTestServer(t) // federationKey nil: федерация выключена.
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_syngo/v1/source", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Server struct {
			License   string `json:"license"`
			SourceURL string `json:"source_url"`
		} `json:"server"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело: %v %s", err, rec.Body.String())
	}

	if body.Server.License != build.LicenseID || body.Server.SourceURL != build.SourceURL() {
		t.Fatalf("source offer = %+v", body.Server)
	}
}
