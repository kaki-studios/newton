// Package auth turns an OAuth callback into a session cookie, and a cookie into a user.
//
// Three deliberate choices, each of which removes a whole class of bugs:
//
//   - OAuth only. No passwords means no hashing parameters, reset flow, email-verification
//     loop or credential-stuffing surface. The provider also hands us a *verified* email,
//     which is the actual anti-farming control.
//   - Opaque session tokens, not JWTs. Revocation is a DELETE, a redeploy signs nobody out,
//     and the session table the quota query already needs is the same table.
//   - goth for the provider dance instead of a hand-written OAuth client. It is where the
//     `state` comparison lives (gothic.validateState), and re-implementing that correctly is
//     the kind of thing that looks easy and is not.
package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"

	"github.com/kaki-studios/newton/server/internal/id"
	"github.com/kaki-studios/newton/server/internal/store"
)

// Config is everything the auth subsystem needs. Missing provider credentials are not fatal:
// the server still serves the simulator, and /api/auth lists only what is configured.
type Config struct {
	// BaseURL is the absolute origin, used to build OAuth callback URLs. Required.
	BaseURL string
	// Dev relaxes cookie flags for http://localhost: no __Host- prefix, no Secure.
	Dev bool
	// SessionTTL is how long a login lasts. Sliding, see store.SessionUser.
	SessionTTL time.Duration
	// CookieName overrides the derived cookie name. Mostly for tests.
	CookieName string
	// Providers are the OAuth providers with credentials. May be empty.
	Providers []Provider
	// CookieKey signs the short-lived OAuth state cookie, and nothing else.
	CookieKey []byte
	// Logger receives auth events (no tokens, no emails).
	Logger *slog.Logger
	// NewID mints user-visible ids; nil means crypto/rand hex.
	NewID id.Newer
	// MakeProvider builds a goth provider from a config entry. Nil uses the built-in
	// github/google constructors. Tests inject goth's faux provider here so the whole
	// handshake can be exercised without a network or real credentials.
	MakeProvider func(p Provider, callbackURL string) (goth.Provider, error)
}

// Provider is one configured OAuth provider.
type Provider struct {
	// Name is the goth provider name and the URL segment: "github", "google".
	Name string
	// ClientID and ClientSecret come from Swarm secrets.
	ClientID     string
	ClientSecret string
	// Scopes override the defaults. Empty means the provider's recommended set.
	Scopes []string
}

// SessionCookieName derives the cookie name. In production it is `__Host-newton`, which
// browsers refuse to accept from a subdomain (no Domain attribute allowed, Path must be /,
// Secure required), so a sibling subdomain cannot overwrite a login cookie. In dev the prefix
// rules cannot be satisfied over http://localhost, so it is dropped rather than faked.
func SessionCookieName(dev bool) string {
	if dev {
		return "newton_session"
	}
	return "__Host-newton"
}

// StateCookieName is the short-lived cookie gothic's signed store writes. It carries the OAuth
// state and nothing worth stealing; it is separate from the session cookie so that signing out
// does not depend on clearing it.
func StateCookieName(dev bool) string {
	if dev {
		return "newton_oauth"
	}
	return "__Host-newton_oauth"
}

// Auth is the auth subsystem. It is a value, not a package-level singleton: goth's provider
// registry is global, so all of that mutation happens once in New and never again.
type Auth struct {
	cfg       Config
	store     *store.Store
	providers map[string]bool
	log       *slog.Logger
	cookie    Cookie
	state     Cookie
	now       func() time.Time
	oauth     *oauthFlow
}

// New wires providers into goth and returns the subsystem. It fails only on configuration that
// cannot work at all, naming the setting to fix: a server that starts with half a provider and
// fails at the login click is worse than one that refuses to start.
func New(cfg Config, st *store.Store) (*Auth, error) {
	if st == nil {
		return nil, errors.New("auth: a store is required")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("auth: BaseURL is required (set BASE_URL, e.g. https://newton.example.com)")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("auth: BASE_URL %q is not an absolute URL (want e.g. https://newton.example.com)", cfg.BaseURL)
	}
	if cfg.Dev && base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("auth: BASE_URL %q has scheme %q; want http or https", cfg.BaseURL, base.Scheme)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if len(cfg.CookieKey) < 32 {
		return nil, fmt.Errorf("auth: the cookie signing key is %d bytes; it needs at least 32 (the SESSION_KEY secret)", len(cfg.CookieKey))
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.NewID == nil {
		cfg.NewID = id.New
	}
	if cfg.CookieName == "" {
		cfg.CookieName = SessionCookieName(cfg.Dev)
	}
	if cfg.MakeProvider == nil {
		cfg.MakeProvider = newProvider
	}

	a := &Auth{
		cfg:       cfg,
		store:     st,
		providers: make(map[string]bool, len(cfg.Providers)),
		log:       cfg.Logger,
		cookie:    newCookie(cfg.CookieName, base.Scheme == "https" || !cfg.Dev, "/", cfg.Dev),
		state:     newCookie(StateCookieName(cfg.Dev), base.Scheme == "https" || !cfg.Dev, "/", cfg.Dev),
		now:       st.Now,
	}

	// goth keeps a process-global provider registry. Registering here, once, is fine; the
	// alternative (a registry per request) is what goth does not support.
	goth.UseProviders()
	for _, p := range cfg.Providers {
		if p.Name == "" || p.ClientID == "" || p.ClientSecret == "" {
			return nil, fmt.Errorf("auth: provider %q needs a name, client id and client secret (check the Swarm secrets)", p.Name)
		}
		callback := strings.TrimSuffix(cfg.BaseURL, "/") + "/api/auth/" + p.Name + "/callback"
		provider, err := cfg.MakeProvider(p, callback)
		if err != nil {
			return nil, err
		}
		goth.UseProviders(provider)
		a.providers[p.Name] = true
	}

	// gothic's default cookie store is keyed from SESSION_SECRET in an init(). Override it
	// with a store built from our own key so the OAuth handshake does not depend on an
	// environment variable being spelled correctly at process start.
	gothic.Store = newCookieStore(cfg.CookieKey, a.state, cfg.Dev)
	a.oauth = newOAuthFlow(a.state.Name, cfg.Dev)

	if len(a.providers) == 0 {
		a.log.Warn("no OAuth providers configured: sign-in is unavailable, so /api/chat will refuse every request",
			"fix", "set the provider client id/secret secrets (github_client_id, github_client_secret)")
	}
	return a, nil
}

// Providers lists the configured provider names, for the sign-in UI and for /api/me.
func (a *Auth) Providers() []string {
	out := make([]string, 0, len(a.providers))
	for _, p := range a.cfg.Providers {
		out = append(out, p.Name)
	}
	return out
}

// Enabled reports whether at least one provider can be used to sign in.
func (a *Auth) Enabled() bool { return len(a.providers) > 0 }

// User is the authenticated principal attached to a request.
type User struct {
	ID          string
	Email       string
	DisplayName string
}

// Middleware resolves the session cookie on every request and stores the user (if any) in the
// context. It never refuses: the simulator must work anonymously, and only /api/chat has to
// demand a session. RequireUser is the gate.
func (a *Auth) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user, expiry, err := a.session(c)
			switch {
			case err == nil:
				c.Set(ctxUserKey, user)
				c.Set(ctxExpiryKey, expiry)
			case errors.Is(err, store.ErrNotFound):
				// Anonymous. Not an error: most requests from a fresh browser are this.
			default:
				// A database problem must not turn the simulator into a 500 page. Log it and
				// treat the request as anonymous; /api/chat will then 401, which is honest.
				a.log.ErrorContext(c.Request().Context(), "resolving session", "err", err)
			}
			return next(c)
		}
	}
}

// RequireUser rejects anonymous requests with 401 and an actionable body.
func (a *Auth) RequireUser() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if _, ok := UserFrom(c); !ok {
				return echo.NewHTTPError(http.StatusUnauthorized, "sign in to use the AI").
					SetInternal(errNoSession)
			}
			return next(c)
		}
	}
}

var errNoSession = errors.New("auth: no session cookie")

// Echo's context is a map[string]any, so these have to be strings. They are namespaced to make
// a collision with another package's key impossible.
const (
	ctxUserKey   = "newton.auth.user"
	ctxExpiryKey = "newton.auth.expiry"
)

// UserFrom returns the authenticated user, if the request carried a valid session.
func UserFrom(c echo.Context) (User, bool) {
	u, ok := c.Get(ctxUserKey).(User)
	return u, ok
}

// MustUser is for handlers behind RequireUser, where the absence of a user is a programming
// error rather than a client condition.
func MustUser(c echo.Context) User {
	u, ok := UserFrom(c)
	if !ok {
		panic("auth: MustUser called on a route without RequireUser")
	}
	return u
}

// session reads the cookie and resolves it. Anything that is not a live session is
// store.ErrNotFound.
func (a *Auth) session(c echo.Context) (User, time.Time, error) {
	cookie, err := c.Cookie(a.cookie.Name)
	if err != nil || cookie.Value == "" {
		return User{}, time.Time{}, store.ErrNotFound
	}
	sess, err := a.store.SessionUser(c.Request().Context(), cookie.Value, a.cfg.SessionTTL)
	if err != nil {
		return User{}, time.Time{}, err
	}
	if sess.User.Disabled {
		// A banned user's cookie is still valid; refusing here means the ban takes effect on
		// the next request instead of waiting for the session to expire.
		_ = a.store.DeleteSession(c.Request().Context(), cookie.Value)
		return User{}, time.Time{}, store.ErrNotFound
	}
	// Sliding expiry: only rewrite the cookie when the row actually moved, so a request-heavy
	// page does not send a Set-Cookie header on every response.
	if sess.Extended {
		a.cookie.set(c, cookie.Value, a.cfg.SessionTTL)
	}
	u := sess.User
	return User{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}, sess.Expiry, nil
}

// SignIn starts the OAuth dance. It delegates to gothic, which generates and signs the state.
func (a *Auth) SignIn(c echo.Context) error {
	provider := c.Param("provider")
	if !a.providers[provider] {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("unknown sign-in provider %q: configured are %s",
			provider, strings.Join(a.Providers(), ", ")))
	}
	a.oauth.begin(c, provider)
	return nil
}

// Callback finishes the OAuth dance: verify state, resolve the identity, rotate the session.
func (a *Auth) Callback(c echo.Context) error {
	provider := c.Param("provider")
	if !a.providers[provider] {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("unknown sign-in provider %q", provider))
	}

	gothUser, err := a.oauth.complete(c, provider)
	if err != nil {
		// The state cookie is single-use; clear it whatever the outcome so a failed
		// callback cannot be replayed with a stale state.
		a.oauth.clear(c)
		a.log.WarnContext(c.Request().Context(), "oauth callback failed", "provider", provider, "err", err)
		return echo.NewHTTPError(http.StatusBadRequest,
			"sign-in did not complete: the browser must return to the same tab within ten minutes. Try again.").
			SetInternal(err)
	}

	user, err := a.store.UpsertIdentity(c.Request().Context(), store.Identity{
		Provider:       gothUser.Provider,
		ProviderUserID: gothUser.UserID,
		Email:          gothUser.Email,
		DisplayName:    displayName(gothUser),
	})
	if err != nil {
		a.log.ErrorContext(c.Request().Context(), "resolving identity", "provider", provider, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			"sign-in reached the provider but the account could not be saved; try again").SetInternal(err)
	}
	if user.Disabled {
		a.log.WarnContext(c.Request().Context(), "disabled user tried to sign in", "user", user.ID)
		return echo.NewHTTPError(http.StatusForbidden,
			"This account is disabled. Ask the operator to re-enable it.")
	}

	// Rotate: delete whatever session the browser arrived with, then issue a fresh one. That
	// is what stops a pre-login cookie from surviving into an authenticated session.
	if old, err := c.Cookie(a.cookie.Name); err == nil && old.Value != "" {
		if err := a.store.DeleteSession(c.Request().Context(), old.Value); err != nil {
			a.log.WarnContext(c.Request().Context(), "deleting pre-login session", "err", err)
		}
	}

	if _, err := a.issueSession(c, user.ID); err != nil {
		a.log.ErrorContext(c.Request().Context(), "issuing session", "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			"signed in, but the session could not be created; try again").SetInternal(err)
	}

	a.log.InfoContext(c.Request().Context(), "signed in",
		"user", user.ID, "provider", gothUser.Provider, "new", user.CreatedAt.Equal(a.now()))

	// 303 so the browser follows with a GET; a 302 here would turn the redirect into an
	// awkward POST to the home page on some clients.
	return c.Redirect(http.StatusSeeOther, "/")
}

// issueSession creates a session row and sets the cookie.
func (a *Auth) issueSession(c echo.Context, userID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if _, err := a.store.CreateSession(c.Request().Context(), userID, token, a.cfg.SessionTTL); err != nil {
		return "", err
	}
	a.cookie.set(c, token, a.cfg.SessionTTL)
	return token, nil
}

// Logout revokes the session row and clears the cookie. It is idempotent: signing out twice,
// or with an already-expired cookie, still succeeds.
func (a *Auth) Logout(c echo.Context) error {
	if cookie, err := c.Cookie(a.cookie.Name); err == nil && cookie.Value != "" {
		if err := a.store.DeleteSession(c.Request().Context(), cookie.Value); err != nil {
			a.log.ErrorContext(c.Request().Context(), "revoking session", "err", err)
			return echo.NewHTTPError(http.StatusInternalServerError,
				"signing out failed: the session could not be revoked. Try again.").SetInternal(err)
		}
	}
	a.cookie.clear(c)
	a.oauth.clear(c)
	return c.NoContent(http.StatusNoContent)
}

func displayName(u goth.User) string {
	for _, candidate := range []string{u.Name, u.NickName, u.FirstName, u.Email} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return "Anonymous"
}

// newProvider maps a config entry onto a goth provider. Only the two providers this project
// actually uses are accepted, on purpose: an unknown name is a config typo, not a feature.
// Tests replace it through Config.MakeProvider.
func newProvider(p Provider, callback string) (goth.Provider, error) {
	switch p.Name {
	case "github":
		scopes := p.Scopes
		if len(scopes) == 0 {
			// `user:email` is what makes the anti-farming rule work: it unlocks the
			// /user/emails endpoint, and goth then returns the verified primary address.
			scopes = []string{"read:user", "user:email"}
		}
		return githubProvider(p.ClientID, p.ClientSecret, callback, scopes...), nil
	case "google":
		scopes := p.Scopes
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile"}
		}
		return googleProvider(p.ClientID, p.ClientSecret, callback, scopes...), nil
	default:
		return nil, fmt.Errorf("auth: unknown provider %q: this server knows github and google. Remove the credentials for anything else", p.Name)
	}
}
