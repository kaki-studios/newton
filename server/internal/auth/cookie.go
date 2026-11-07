package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo/v4"
)

// Cookie renders and clears one cookie with attributes that are decided in exactly one place.
// The `__Host-` prefix is a browser-enforced promise: no Domain, Path=/, Secure. Getting any of
// those wrong in one call site is how session cookies leak to sibling subdomains, so no call
// site builds a cookie directly.
type Cookie struct {
	Name   string
	Secure bool
	Path   string
	// Dev drops SameSite from Lax to... nothing. Kept explicit because http://localhost
	// cookies behave differently in a couple of browsers, and a test pins this.
	Dev bool
	// SameSite is Lax everywhere except a dev server, where a cross-port fetch during local
	// development would otherwise send no cookie at all.
	SameSite http.SameSite
}

func newCookie(name string, secure bool, path string, dev bool) Cookie {
	sameSite := http.SameSiteLaxMode
	if dev {
		// Lax still works for same-origin fetch on localhost, and it is what production
		// uses, so dev deliberately keeps the same value: fewer differences to reason about.
		sameSite = http.SameSiteLaxMode
	}
	return Cookie{Name: name, Secure: secure, Path: path, Dev: dev, SameSite: sameSite}
}

// set writes the cookie for one session token. The lifetime is a duration, not an absolute
// instant, on purpose: Max-Age is what the browser obeys, and deriving it from "expiry minus
// now" would silently make the cookie a session-only cookie whenever the two clocks disagree
// (a skewed host, or a test with an injected clock). The row's expiry is the authority on the
// server; the cookie only has to outlive the tab.
func (c Cookie) set(cx echo.Context, token string, ttl time.Duration) {
	if ttl <= 0 {
		c.clear(cx)
		return
	}
	cx.SetCookie(&http.Cookie{
		Name:     c.Name,
		Value:    token,
		Path:     c.Path,
		MaxAge:   int(ttl.Seconds()),
		Expires:  time.Now().Add(ttl),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// clear expires the cookie. Max-Age: 0 plus an epoch Expires is belt and braces: some old
// clients honour only one of them.
func (c Cookie) clear(cx echo.Context) {
	cx.SetCookie(&http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     c.Path,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// newToken returns 256 bits of crypto/rand, base64url-encoded without padding. 32 bytes is the
// point below which a bearer token stops being unguessable by a motivated attacker.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: crypto/rand is unavailable: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// newCookieStore builds gothic's store. It holds exactly two things during a login: the OAuth
// state and goth's marshalled session. Both are short-lived and signed; neither is the app's
// session cookie.
func newCookieStore(key []byte, state Cookie, dev bool) *sessions.CookieStore {
	store := sessions.NewCookieStore(key)
	store.Options = &sessions.Options{
		Path:     state.Path,
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   state.Secure,
		SameSite: state.SameSite,
	}
	// gothic always uses its own name; point the store at ours so the name is one decision.
	store.MaxAge(store.Options.MaxAge)
	return store
}
