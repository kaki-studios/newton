package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func newStaticServer(t *testing.T, files map[string]string) *echo.Echo {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Any("/api/*", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "no such API endpoint")
	})
	e.GET("/*", Static{Root: root}.Handler())
	return e
}

func get(t *testing.T, e *echo.Echo, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServesIndexAndAssets(t *testing.T) {
	e := newStaticServer(t, map[string]string{
		"index.html":              "<!doctype html><title>Newton</title>",
		"dist/main.js":            "export {};",
		"node_modules/planck.mjs": "export const planck = 1;",
	})

	for _, tc := range []struct {
		path     string
		contains string
	}{
		{"/", "Newton"},
		{"/index.html", "Newton"},
		{"/dist/main.js", "export"},
	} {
		rec := get(t, e, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("GET %s body = %q, want it to contain %q", tc.path, rec.Body.String(), tc.contains)
		}
	}
}

// The importmap in index.html resolves `planck` to a .mjs file. If the server ever labels that
// text/plain, the module fails to load with a MIME-type error and the app is blank. nginx's
// mime.types broke exactly this once, hence Dockerfile.nginx's comment and this test.
func TestMJSIsJavaScript(t *testing.T) {
	e := newStaticServer(t, map[string]string{"node_modules/planck.mjs": "export {};"})
	rec := get(t, e, "/node_modules/planck.mjs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET .mjs = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Fatalf(".mjs Content-Type = %q, want text/javascript", got)
	}
}

// Every static file must tell a cache to revalidate. Asset names are stable across deploys, so
// without this header an edge keeps yesterday's bundle beside today's index.html; that shipped a
// pre-auth dist/main.js and left the account bar on "checking…" forever. no-cache (not no-store)
// is the requirement: a stored copy is fine, one served without asking the origin is not.
func TestStaticFilesRevalidateWithTheOrigin(t *testing.T) {
	e := newStaticServer(t, map[string]string{
		"index.html":   "<!doctype html>",
		"dist/main.js": "export {};",
	})

	for _, path := range []string{"/", "/index.html", "/dist/main.js"} {
		rec := get(t, e, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want \"no-cache\": a stable filename may not be "+
				"served from a cache without revalidating", path, got)
		}
	}
}

// A typo in a /dist/... URL must fail visibly. This app has no client-side routes, so falling back
// to index.html would serve a page that looks fine and does nothing.
func TestUnknownPathIs404NotIndex(t *testing.T) {
	e := newStaticServer(t, map[string]string{"index.html": "<title>Newton</title>", "dist/main.js": "x"})

	for _, path := range []string{"/dist/mian.js", "/nope", "/dist/"} {
		rec := get(t, e, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "Newton") {
			t.Errorf("GET %s served index.html on a miss: %q", path, rec.Body.String())
		}
	}
}

func TestTraversalIsRefused(t *testing.T) {
	// The gap between the root and a file next to it is what traversal would expose. The root is
	// a temp dir, so the neighbour is a real file that exists and must stay unreachable.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}
	serve := filepath.Join(root, "public")
	if err := os.MkdirAll(serve, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serve, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.GET("/*", Static{Root: serve}.Handler())

	for _, path := range []string{
		"/../go.mod",
		"/..%2fgo.mod",
		"/dist/../../go.mod",
		"/%2e%2e/go.mod",
	} {
		rec := get(t, e, path)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want a refusal: %q", path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "do not read me") {
			t.Fatalf("GET %s read a file outside the static root", path)
		}
	}
}

func TestDotFilesAndDirectoriesAreNotListed(t *testing.T) {
	e := newStaticServer(t, map[string]string{"index.html": "ok", "dist/main.js": "x"})
	if rec := get(t, e, "/dist"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /dist = %d, want 404: directory listings are off", rec.Code)
	}
}

func TestWriteMethodsAreRefused(t *testing.T) {
	e := newStaticServer(t, map[string]string{"index.html": "ok"})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/index.html", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST a static file = %d, want 405", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := echo.New()
	e.Use(SecurityHeaders())
	e.GET("/", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	rec := get(t, e, "/")
	for header, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "strict-origin-when-cross-origin",
		"X-Frame-Options":            "DENY",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestCSRFAllowsSafeAndSameOriginRequests(t *testing.T) {
	e := echo.New()
	e.Use(CSRF())
	e.POST("/api/chat", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET("/api/me", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })

	for _, tc := range []struct {
		name   string
		method string
		path   string
		site   string
		origin string
		proto  string
		host   string
		want   int
	}{
		{name: "GET has no body to forge", method: http.MethodGet, path: "/api/me", want: 200},
		{name: "no fetch metadata is allowed", method: http.MethodPost, want: 200},
		{name: "same-origin fetch", method: http.MethodPost, site: "same-origin", want: 200},
		{name: "typed url", method: http.MethodPost, site: "none", want: 200},
		{name: "cross-site form", method: http.MethodPost, site: "cross-site", want: 403},
		{name: "sibling subdomain", method: http.MethodPost, site: "same-site", want: 403},
		{
			name: "matching Origin behind the proxy", method: http.MethodPost,
			origin: "https://newton.example.com", proto: "https", host: "newton.example.com", want: 200,
		},
		{
			name: "mismatched Origin", method: http.MethodPost,
			origin: "https://evil.example.com", proto: "https", host: "newton.example.com", want: 403,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.path
			if target == "" {
				target = "/api/chat"
			}
			req := httptest.NewRequest(tc.method, target, strings.NewReader("{}"))
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.host != "" {
				req.Header.Set("X-Forwarded-Host", tc.host)
				req.Host = tc.host
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestErrorHandlerHidesInternalDetail(t *testing.T) {
	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))

	e.GET("/boom", func(c echo.Context) error {
		panic("a stack trace with a path in it")
	})
	e.GET("/teapot", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusTeapot, "I am a teapot")
	})
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	e.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teapot", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "teapot") {
		t.Errorf("body = %s, want the deliberate message to survive", rec.Body.String())
	}

	// The default (no Recover middleware) lets a panic escape, which is the router's job; what
	// matters here is that an *echo.HTTPError carrying a real 500 does not leak its cause.
	e2 := echo.New()
	e2.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	e2.GET("/internal", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusInternalServerError, "db password is hunter2")
	})
	rec2 := httptest.NewRecorder()
	e2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/internal", nil))
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec2.Code)
	}
	if strings.Contains(rec2.Body.String(), "hunter2") {
		t.Fatalf("the error handler leaked an internal message: %s", rec2.Body.String())
	}
}

// `npm run dev:full` passes `-static .`, so a relative root must work. The containment check
// compares absolute paths for exactly this reason; a naive prefix test silently 404s every file.
func TestRelativeRootServesFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("Newton"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Run the handler from inside the root, the way `-static .` does from the repo root.
	t.Chdir(root)

	e := echo.New()
	e.HTTPErrorHandler = ErrorHandler(slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.GET("/*", Static{Root: "."}.Handler())

	rec := get(t, e, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with -static . = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Newton") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestDisabledStaticRootIs404(t *testing.T) {
	e := echo.New()
	e.GET("/*", Static{Root: ""}.Handler())
	rec := get(t, e, "/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when static serving is off", rec.Code)
	}
}
