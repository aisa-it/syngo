package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReviewMediaHTMLInline — загруженный HTML/SVG не должен
// отображаться в origin homeserver. До правки download отдавал Content-Type
// загрузившего с `Content-Disposition: inline` и без CSP/nosniff.
func TestReviewMediaHTMLInline(t *testing.T) {
	t.Parallel()

	server := newMediaServer(t, 1<<20)
	alice := registerUser(t, server, "alice", "1234567890")

	cases := []struct {
		name        string
		mime        string
		payload     string
		wantAttach  bool
		contentType string
	}{
		{"html", "text/html", "<script>alert(1)</script>", true, "text/html"},
		{"svg", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, true, "image/svg+xml"},
		{"png inline", "image/png", "\x89PNG\r\n\x1a\n", false, "image/png"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost,
			"/_matrix/media/v3/upload?filename=x", strings.NewReader(tc.payload))
		req.Header.Set("Content-Type", tc.mime)
		req.Header.Set("Authorization", "Bearer "+alice.AccessToken)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: upload status %d, body %s", tc.name, rec.Code, rec.Body.String())
		}

		var uploaded struct {
			ContentURI string `json:"content_uri"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
			t.Fatalf("%s: decode upload: %v", tc.name, err)
		}

		mediaID := strings.TrimPrefix(uploaded.ContentURI, "mxc://test.local/")

		dl := httptest.NewRequest(http.MethodGet,
			"/_matrix/client/v1/media/download/test.local/"+mediaID, http.NoBody)
		dl.Header.Set("Authorization", "Bearer "+alice.AccessToken)
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, dl)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: download status %d", tc.name, rec.Code)
		}

		cd := rec.Header().Get("Content-Disposition")
		if tc.wantAttach && !strings.HasPrefix(cd, "attachment") {
			t.Errorf("%s: content-disposition = %q, ожидали attachment", tc.name, cd)
		}

		if !tc.wantAttach && !strings.HasPrefix(cd, "inline") {
			t.Errorf("%s: content-disposition = %q, ожидали inline", tc.name, cd)
		}

		if got := rec.Header().Get("Content-Type"); got != tc.contentType {
			t.Errorf("%s: content-type = %q, ожидали %q", tc.name, got, tc.contentType)
		}

		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, ожидали nosniff", tc.name, got)
		}

		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP = %q, ожидали sandbox и default-src 'none'", tc.name, csp)
		}
	}
}
