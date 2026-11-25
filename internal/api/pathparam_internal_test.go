package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestPathParamsDecoded — все параметры пути проходят через pathParamValue:
// chi матчит по RawPath и отдаёт сырое значение, поэтому %3A/%2F/пробел/кириллица
// обязаны доезжать до обработчика уже декодированными.
func TestPathParamsDecoded(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	router.Get("/_matrix/client/v3/rooms/{roomId}/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pathParamValue(r, "roomId") + "|" + pathParamValue(r, "tag")))
	})
	router.Get("/_matrix/client/v3/profile/{userId}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pathParamValue(r, "userId")))
	})

	cases := []struct {
		name string
		path string
		want string
	}{
		{"room id", "/_matrix/client/v3/rooms/%21room%3Aserver/tags/x", "!room:server|x"},
		{"user id", "/_matrix/client/v3/profile/%40alice%3Aserver", "@alice:server"},
		{"slash in tag", "/_matrix/client/v3/rooms/%21r%3As/tags/a%2Fb", "!r:s|a/b"},
		{"space", "/_matrix/client/v3/rooms/%21r%3As/tags/a%20b", "!r:s|a b"},
		{"cyrillic", "/_matrix/client/v3/profile/%D1%80%D1%83%D1%81", "рус"},
		{"plain", "/_matrix/client/v3/profile/@bob:test", "@bob:test"},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, http.NoBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, rec.Code)
		}

		if got := rec.Body.String(); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNoRawURLParam — статический гейт: в internal/api не должно остаться прямых
// chi.URLParam, кроме единственного места внутри pathParamValue.
func TestNoRawURLParam(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	allowed := map[string]bool{"rooms_extra.go": true}

	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}

		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "chi" || sel.Sel.Name != "URLParam" {
				return true
			}

			if !allowed[name] {
				t.Errorf("%s:%d: прямой chi.URLParam — используйте pathParamValue",
					name, fset.Position(call.Pos()).Line)
			}

			return true
		})
	}
}

// TestStateKeySingleDecode — двойного декодирования нет: state_key с литеральным
// процентом (%2541) доезжает как "%41", а не "A".
func TestStateKeySingleDecode(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	router.Get("/_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(pathParamValue(r, "eventType") + "|" + pathParamValue(r, "stateKey")))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/_matrix/client/v3/rooms/!room:test.local/state/com.example.key/%2541", http.NoBody)
	router.ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "com.example.key|%41" {
		t.Fatalf("state_key = %q, ожидали %q (одинарное декодирование)", got, "com.example.key|%41")
	}
}
