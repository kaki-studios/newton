// Package web serves the static app and the security headers around it, and owns the shape of
// every error response.
//
// One origin is the point: cookies with the `__Host-` prefix work, no CORS exists, and nothing
// sits between the browser and the SSE stream to buffer it. That is why nginx is gone rather than
// staying in front of the API.
package web

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"
)

// Static is the file server. Two specifics, both about failing visibly:
//
//   - Unknown paths return 404, never index.html. This app has no client-side routes, so a typo
//     in a `/dist/...` URL must be a 404 rather than a page that looks fine and does nothing.
//   - `.mjs` must be served as text/javascript. Go's builtin table does that today (verified),
//     and a test pins it, because the importmap in index.html depends on it and an nginx
//     mime.types once broke exactly this.
type Static struct {
	// Root is the directory to serve. Empty disables static serving (the API-only mode used by
	// the end-to-end tests). It may be relative: `npm run dev:full` passes `-static .`, which is
	// the documented development loop, so relative roots have to work.
	Root string
}

// Handler returns the echo handler for the static tree.
func (s Static) Handler() echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.Root == "" {
			return echo.ErrNotFound
		}
		req := c.Request()

		// Only GET and HEAD can be static files. A POST to a file path is a mistake, and
		// answering 405 is clearer than a 404 that suggests the file is missing.
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return echo.NewHTTPError(http.StatusMethodNotAllowed, "static files are read-only")
		}

		// Clean the path before touching the filesystem: `..` in a URL must not walk out of
		// Root. path.Clean on the URL path (not filepath) is the right tool; it also collapses
		// duplicate slashes, which http.FileServer would otherwise treat as distinct.
		urlPath := path.Clean("/" + req.URL.Path)
		if strings.Contains(urlPath, "\x00") {
			return echo.ErrNotFound
		}

		// filepath.Join cleans the result. The containment check below then compares two
		// *absolute* paths, because a relative root ("." in dev) would otherwise fail a prefix
		// test and 404 every file. Resolving once per request is cheap next to the stat that
		// follows, and correctness here is worth more than the microseconds.
		root, err := filepath.Abs(s.Root)
		if err != nil {
			return echo.ErrNotFound
		}
		name, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(urlPath)))
		if err != nil {
			return echo.ErrNotFound
		}
		if err := contains(root, name); err != nil {
			return echo.ErrNotFound
		}

		info, err := os.Stat(name)
		if err != nil {
			return echo.ErrNotFound
		}
		if info.IsDir() {
			// No directory listings and no index fallback: this app has exactly one page and it
			// is fetched by path.
			name = filepath.Join(name, "index.html")
			info, err = os.Stat(name)
			if err != nil || info.IsDir() {
				return echo.ErrNotFound
			}
		}

		f, err := os.Open(name)
		if err != nil {
			return echo.ErrNotFound
		}
		defer f.Close()

		// No-cache, not no-store. Every name under this root is stable across deploys
		// (`/dist/main.js`, never `main.<hash>.js`), so without an instruction the edge keeps a
		// copy past the build that wrote it. That is not hypothetical: Cloudflare's default 4h
		// TTL served a pre-auth `dist/main.js` beside a fresh `index.html`, and the account bar
		// stayed on "checking…" because the bundle that would have filled it was never fetched.
		// `no-cache` still allows storing a copy but forces revalidation, and ServeContent's
		// ETag/Last-Modified make that a 304, so a deploy is never half-applied. `no-store`
		// would give up the 304s and fix nothing extra.
		c.Response().Header().Set("Cache-Control", "no-cache")
		http.ServeContent(c.Response(), req, info.Name(), info.ModTime(), f)
		return nil
	}
}

// contains reports whether target is root or inside it. Both must be absolute and clean.
//
// filepath.Rel is the auditable form of this check: a prefix comparison looks right and then fails
// on `/app/public-other` matching a root of `/app/public`, or on a root of `.`.
func contains(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errOutsideRoot
	}
	return nil
}

var errOutsideRoot = errors.New("web: path escapes the static root")

// SecurityHeaders applies the headers this app needs and nothing more. No CSP is set here: the
// app loads ES modules by an importmap and has inline <style>, so a real policy needs the hashes
// and a nonce, which is a separate piece of work. Saying so is better than shipping
// `unsafe-inline` and calling it a policy.
func SecurityHeaders() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			// The app is served over HTTPS only (Traefik + a cert resolver), so a year-long
			// HSTS is safe. It is deliberately not set on API-only deployments behind a
			// different hostname; the header is per-response and harmless there.
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("X-Frame-Options", "DENY")
			// The OAuth callback needs a cross-site top-level navigation, which is what `Lax`
			// cookies allow; nothing here needs opener access.
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			return next(c)
		}
	}
}

// CSRF returns the middleware that makes cookie-authenticated state changes safe without a token.
//
// The argument: this API only accepts JSON bodies (Echo's binder returns 415 without
// Content-Type: application/json, and a cross-site HTML form cannot set that header), and the
// session cookie is SameSite=Lax. On top of that, every state-changing request must come from
// this origin, which is checked here via Sec-Fetch-Site when the browser sends it and Origin
// otherwise. Requests with neither header (curl, a healthcheck, a Go test) are allowed: they
// cannot be cross-site browser requests, which is the only threat being defended against.
func CSRF() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			switch req.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				return next(c)
			}

			if site := req.Header.Get("Sec-Fetch-Site"); site != "" {
				// same-origin: the app itself. same-site: a sibling subdomain, which is not
				// this origin and must not be able to drive this session. none: a typed URL
				// or a bookmark; a POST from that is not a forged request.
				switch site {
				case "same-origin", "none":
					return next(c)
				default:
					return echo.NewHTTPError(http.StatusForbidden,
						"cross-site requests are not allowed").SetInternal(errors.New("web: Sec-Fetch-Site " + site))
				}
			}

			if origin := req.Header.Get("Origin"); origin != "" {
				if !sameOrigin(origin, req) {
					return echo.NewHTTPError(http.StatusForbidden,
						"cross-origin requests are not allowed").SetInternal(errors.New("web: Origin " + origin))
				}
			}
			return next(c)
		}
	}
}

// sameOrigin compares an Origin header with the request's own scheme and host.
//
// Behind Traefik the container sees http and an internal host, so the *forwarded* headers are what
// describe the browser's origin. They are trusted because the only way to reach the container is
// through the proxy: a request that sets X-Forwarded-Proto itself has to arrive through Traefik
// first, which overwrites them.
func sameOrigin(origin string, req *http.Request) bool {
	scheme := req.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
		if req.TLS != nil {
			scheme = "https"
		}
	}
	host := req.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = req.Host
	}
	return origin == scheme+"://"+host
}

// ErrorHandler renders every error as the same small JSON object. Two things it fixes about
// Echo's default: it logs at a level that matches the status (a 404 is not an error worth waking
// up for), and it never leaks an internal message to the client while still putting it in the log.
func ErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	if log == nil {
		log = slog.Default()
	}
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			// A streamed response already sent its status line. The handler owns its own
			// error frames; there is nothing left to write here.
			return
		}

		status := http.StatusInternalServerError
		message := "something went wrong"
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) {
			status = httpErr.Code
			switch m := httpErr.Message.(type) {
			case string:
				message = m
			default:
				if httpErr.Message != nil {
					message = http.StatusText(status)
				}
			}
		}

		// Echo's Recover middleware reports a panic as a 500 with the panic text as the message;
		// that must never reach a client.
		if status >= http.StatusInternalServerError && message == "" {
			message = http.StatusText(status)
		}

		requestID := c.Response().Header().Get(echo.HeaderXRequestID)
		attrs := []any{
			"status", status,
			"method", c.Request().Method,
			"path", c.Request().URL.Path,
		}
		if requestID != "" {
			attrs = append(attrs, "request_id", requestID)
		}
		switch {
		case status >= http.StatusInternalServerError:
			log.ErrorContext(c.Request().Context(), "request failed", append(attrs, "err", err)...)
			// Only a deliberate 5xx (an *echo.HTTPError with an Internal cause) keeps its
			// message; a panic does not.
			if httpErr == nil || httpErr.Internal == nil {
				message = "something went wrong"
			}
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			log.InfoContext(c.Request().Context(), "request refused", append(attrs, "err", err)...)
		default:
			log.DebugContext(c.Request().Context(), "request rejected", append(attrs, "err", err)...)
		}

		if c.Request().Method == http.MethodHead {
			_ = c.NoContent(status)
			return
		}
		_ = c.JSON(status, map[string]any{"error": message})
	}
}
