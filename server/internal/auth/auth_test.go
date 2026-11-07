package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"golang.org/x/oauth2"

	"github.com/kaki-studios/newton/server/internal/store"
)

// The auth tests drive the real echo handlers, the real gothic handshake and goth's faux
// provider, against a real SQLite database. Only crypto/rand is absent from the path.

type harness struct {
	echo  *echo.Echo
	auth  *Auth
	store *store.Store
	clock *clock
}

type clock struct{ now time.Time }

func newHarness(t *testing.T, dev bool) *harness {
	t.Helper()
	clk := &clock{now: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "newton.db"),
		Now:  func() time.Time { return clk.now },
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	baseURL := "https://newton.example.com"
	if dev {
		baseURL = "http://localhost:8080"
	}
	a, err := New(Config{
		BaseURL:    baseURL,
		Dev:        dev,
		SessionTTL: 24 * time.Hour,
		CookieKey:  []byte("0123456789abcdef0123456789abcdef"),
		Providers: []Provider{{
			Name:         "faux",
			ClientID:     "id",
			ClientSecret: "secret",
		}},
		MakeProvider: func(p Provider, callbackURL string) (goth.Provider, error) {
			// The test provider answers BeginAuth/FetchUser locally, so the handshake is
			// real gothic but there is no network and no real credentials.
			return &testProvider{name: p.Name}, nil
		},
	}, st)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(a.Middleware())
	e.GET("/api/auth/:provider", a.SignIn)
	e.GET("/api/auth/:provider/callback", a.Callback)
	e.POST("/api/auth/logout", a.Logout)
	e.GET("/api/me", func(c echo.Context) error {
		u, ok := UserFrom(c)
		if !ok {
			return c.JSON(http.StatusUnauthorized, map[string]any{"error": "not signed in"})
		}
		return c.JSON(http.StatusOK, map[string]string{"id": u.ID, "name": u.DisplayName})
	})
	e.GET("/api/chat", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}, a.RequireUser())

	return &harness{echo: e, auth: a, store: st, clock: clk}
}

// do runs one request and returns the recorder, recording cookies into the jar-less
// harness by hand.
func (h *harness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.echo.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(t *testing.T, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return h.do(t, req)
}

// signIn runs the two-step OAuth dance and returns the session cookie.
func (h *harness) signIn(t *testing.T) *http.Cookie {
	t.Helper()

	// Step 1: begin. gothic writes its state cookie and issues the redirect.
	begin := h.get(t, "/api/auth/faux")
	if begin.Code != http.StatusTemporaryRedirect && begin.Code != http.StatusFound {
		t.Fatalf("begin auth = %d %s, want a redirect to the provider", begin.Code, begin.Body.String())
	}
	stateCookies := begin.Result().Cookies()
	if len(stateCookies) == 0 {
		t.Fatal("begin auth set no state cookie: the callback cannot validate the state")
	}

	// Step 2: the provider bounces back to the callback with a code and the state.
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing provider redirect: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("the provider redirect carries no state parameter")
	}

	callback := h.get(t, "/api/auth/faux/callback?code=fake-code&state="+url.QueryEscape(state), stateCookies...)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d %s, want 303", callback.Code, callback.Body.String())
	}

	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == h.auth.cookie.Name && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("the callback did not set a session cookie")
	return nil
}

func TestSignInCreatesASession(t *testing.T) {
	h := newHarness(t, false)
	cookie := h.signIn(t)

	if !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Errorf("cookie name = %q, want the __Host- prefix in production", cookie.Name)
	}
	if !cookie.Secure || !cookie.HttpOnly {
		t.Errorf("cookie Secure=%v HttpOnly=%v, want both true", cookie.Secure, cookie.HttpOnly)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want / (required by __Host-)", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Domain != "" {
		t.Errorf("cookie Domain = %q, want empty (forbidden by __Host-)", cookie.Domain)
	}

	me := h.get(t, "/api/me", cookie)
	if me.Code != http.StatusOK {
		t.Fatalf("/api/me = %d %s, want 200", me.Code, me.Body.String())
	}
	if !strings.Contains(me.Body.String(), "faux") && !strings.Contains(me.Body.String(), `"id"`) {
		t.Errorf("body = %s, want the user", me.Body.String())
	}
}

func TestAnonymousMeIsUnauthorized(t *testing.T) {
	h := newHarness(t, false)
	rec := h.get(t, "/api/me")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me = %d, want 401", rec.Code)
	}
}

func TestChatRequiresASession(t *testing.T) {
	h := newHarness(t, false)

	if rec := h.get(t, "/api/chat"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/chat = %d, want 401", rec.Code)
	}
	if rec := h.get(t, "/api/chat", h.signIn(t)); rec.Code != http.StatusOK {
		t.Fatalf("signed-in /api/chat = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	h := newHarness(t, false)
	cookie := h.signIn(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(cookie)
	rec := h.do(t, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", rec.Code)
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookie.Name && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}

	// The same cookie must now be refused: revocation is a DELETE, not a client-side choice.
	if rec := h.get(t, "/api/me", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me after logout = %d, want 401", rec.Code)
	}
	// And logging out again is not an error.
	if rec := h.do(t, reqWithCookie(http.MethodPost, "/api/auth/logout", cookie)); rec.Code != http.StatusNoContent {
		t.Fatalf("second logout = %d, want 204", rec.Code)
	}
}

func TestSessionCookieIsRotatedOnLogin(t *testing.T) {
	h := newHarness(t, false)
	first := h.signIn(t)

	// Sign in again while presenting the first cookie: the old row must be gone.
	second := h.signInWith(t, first)
	if second.Value == first.Value {
		t.Fatal("the second login reused the first token: the session was not rotated")
	}
	if rec := h.get(t, "/api/me", first); rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-login session survived the login: %d", rec.Code)
	}
	if rec := h.get(t, "/api/me", second); rec.Code != http.StatusOK {
		t.Fatalf("new session is not valid: %d", rec.Code)
	}
}

// signInWith runs the dance while presenting an existing session cookie, which is what a
// browser does when the user is already signed in.
func (h *harness) signInWith(t *testing.T, existing *http.Cookie) *http.Cookie {
	t.Helper()
	begin := h.get(t, "/api/auth/faux", existing)
	stateCookies := begin.Result().Cookies()
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")

	cookies := append([]*http.Cookie{existing}, stateCookies...)
	callback := h.get(t, "/api/auth/faux/callback?code=c&state="+url.QueryEscape(state), cookies...)
	if callback.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d %s", callback.Code, callback.Body.String())
	}
	for _, c := range callback.Result().Cookies() {
		if c.Name == h.auth.cookie.Name && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestStateMismatchIsRejected(t *testing.T) {
	h := newHarness(t, false)

	begin := h.get(t, "/api/auth/faux")
	stateCookies := begin.Result().Cookies()
	if len(stateCookies) == 0 {
		t.Fatal("no state cookie")
	}

	rec := h.get(t, "/api/auth/faux/callback?code=c&state=forged", stateCookies...)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "did not complete") {
		t.Errorf("body = %s, want an actionable message", rec.Body.String())
	}
	var sessions int
	if err := h.store.DB().QueryRow(`select count(*) from sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("%d sessions created despite a state mismatch", sessions)
	}
}

func TestCallbackWithoutStateCookieIsRejected(t *testing.T) {
	h := newHarness(t, false)
	rec := h.get(t, "/api/auth/faux/callback?code=c&state=anything")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("callback with no state cookie = %d, want 400", rec.Code)
	}
}

func TestUnknownProviderIs404(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{"/api/auth/myspace", "/api/auth/myspace/callback?code=c"} {
		if rec := h.get(t, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestDevCookieDropsHostPrefixAndSecure(t *testing.T) {
	h := newHarness(t, true)
	cookie := h.signIn(t)
	if strings.HasPrefix(cookie.Name, "__Host-") {
		t.Errorf("dev cookie name = %q, want no __Host- prefix (browsers reject it over http)", cookie.Name)
	}
	if cookie.Secure {
		t.Error("dev cookie is Secure; http://localhost would never send it back")
	}
}

func TestExpiredSessionIsRefused(t *testing.T) {
	h := newHarness(t, false)
	cookie := h.signIn(t)

	h.clock.now = h.clock.now.Add(25 * time.Hour) // past the 24h ttl

	if rec := h.get(t, "/api/me", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me with an expired session = %d, want 401", rec.Code)
	}
}

func TestDisabledUserLosesTheirSession(t *testing.T) {
	h := newHarness(t, false)
	cookie := h.signIn(t)

	var userID string
	if err := h.store.DB().QueryRow(`select id from users limit 1`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDisabled(context.Background(), userID, true); err != nil {
		t.Fatal(err)
	}

	if rec := h.get(t, "/api/me", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a banned user's session still works: %d", rec.Code)
	}
	// The row is gone too: un-banning should not silently resurrect the old session.
	var sessions int
	if err := h.store.DB().QueryRow(`select count(*) from sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived the ban", sessions)
	}
}

func TestDisabledUserCannotSignIn(t *testing.T) {
	h := newHarness(t, false)
	h.signIn(t)

	var userID string
	if err := h.store.DB().QueryRow(`select id from users limit 1`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDisabled(context.Background(), userID, true); err != nil {
		t.Fatal(err)
	}

	begin := h.get(t, "/api/auth/faux")
	stateCookies := begin.Result().Cookies()
	location, err := url.Parse(begin.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	rec := h.get(t, "/api/auth/faux/callback?code=c&state="+url.QueryEscape(location.Query().Get("state")), stateCookies...)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned user sign-in = %d %s, want 403", rec.Code, rec.Body.String())
	}
}

func TestConfigValidationNamesTheSetting(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no base url", Config{CookieKey: key32()}, "BASE_URL"},
		{"relative base url", Config{BaseURL: "/api", CookieKey: key32()}, "BASE_URL"},
		{"short key", Config{BaseURL: "https://x.example", CookieKey: []byte("short")}, "SESSION_KEY"},
		{
			"provider without secret",
			Config{BaseURL: "https://x.example", CookieKey: key32(), Providers: []Provider{{Name: "github", ClientID: "id"}}},
			"github",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, st)
			if err == nil {
				t.Fatal("New accepted an unusable configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestNoProvidersStillServesButReportsDisabled(t *testing.T) {
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "x.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a, err := New(Config{BaseURL: "https://x.example", CookieKey: key32()}, st)
	if err != nil {
		t.Fatalf("New with no providers: %v", err)
	}
	if a.Enabled() {
		t.Error("Enabled = true with no providers")
	}
	if got := a.Providers(); len(got) != 0 {
		t.Errorf("Providers() = %v, want empty", got)
	}
}

func TestSessionCookieNameDerivation(t *testing.T) {
	if got := SessionCookieName(false); got != "__Host-newton" {
		t.Errorf("production cookie = %q", got)
	}
	if got := SessionCookieName(true); got == "__Host-newton" {
		t.Error("dev cookie still carries the __Host- prefix")
	}
}

func TestMustUserPanicsOutsideRequireUser(t *testing.T) {
	e := echo.New()
	e.GET("/", func(c echo.Context) error {
		defer func() {
			if recover() == nil {
				t.Error("MustUser did not panic without a user")
			}
		}()
		MustUser(c)
		return nil
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
}

func reqWithCookie(method, path string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func key32() []byte { return []byte("0123456789abcdef0123456789abcdef") }

// testProvider is a minimal goth.Provider: the same session contract the GitHub provider
// implements, with a fixed user and no HTTP. Everything else in these tests (gothic's state
// validation, the session table, the cookies) is the production path.
type testProvider struct {
	name string
}

func (p *testProvider) Name() string                { return p.name }
func (p *testProvider) SetName(name string)         { p.name = name }
func (p *testProvider) Debug(bool)                  {}
func (p *testProvider) RefreshTokenAvailable() bool { return false }
func (p *testProvider) Client() *http.Client        { return http.DefaultClient }
func (p *testProvider) RefreshToken(string) (*oauth2.Token, error) {
	return nil, errors.New("testProvider: refresh is not implemented")
}

func (p *testProvider) BeginAuth(state string) (goth.Session, error) {
	return &testSession{AuthURL: "https://provider.example/authorize?state=" + url.QueryEscape(state)}, nil
}

func (p *testProvider) UnmarshalSession(data string) (goth.Session, error) {
	s := &testSession{}
	if err := json.Unmarshal([]byte(data), s); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *testProvider) FetchUser(sess goth.Session) (goth.User, error) {
	s := sess.(*testSession)
	if s.AccessToken == "" {
		return goth.User{}, errors.New("testProvider: no access token yet")
	}
	return goth.User{
		Provider:    p.name,
		UserID:      "test-user-1",
		Email:       "ada@example.com",
		Name:        "Ada Lovelace",
		AccessToken: s.AccessToken,
	}, nil
}

type testSession struct {
	AuthURL     string `json:"auth_url"`
	AccessToken string `json:"access_token"`
}

func (s *testSession) GetAuthURL() (string, error) {
	if s.AuthURL == "" {
		return "", errors.New(goth.NoAuthUrlErrorMessage)
	}
	return s.AuthURL, nil
}

func (s *testSession) Authorize(goth.Provider, goth.Params) (string, error) {
	s.AccessToken = "test-access-token"
	return s.AccessToken, nil
}

func (s *testSession) Marshal() string {
	b, _ := json.Marshal(s)
	return string(b)
}
