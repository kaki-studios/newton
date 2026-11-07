// Package app assembles the HTTP server from its parts. `cmd/api` owns flags, signals and the
// process; this package owns the routes, so an end-to-end test can build the exact same server
// without starting a listener.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/kaki-studios/newton/server/internal/auth"
	"github.com/kaki-studios/newton/server/internal/chat"
	"github.com/kaki-studios/newton/server/internal/config"
	"github.com/kaki-studios/newton/server/internal/llm"
	"github.com/kaki-studios/newton/server/internal/quota"
	"github.com/kaki-studios/newton/server/internal/store"
	"github.com/kaki-studios/newton/server/internal/web"
)

// App is a fully wired server, ready for httptest or a real listener.
type App struct {
	Echo  *echo.Echo
	Auth  *auth.Auth
	Chat  *chat.Handler
	Quota *quota.Manager
	Store *store.Store
	// Provider is exposed so /api/me can say which one is live, and so tests can assert on it.
	Provider llm.Provider
	cfg      config.Config
	log      *slog.Logger
}

// Options are the parts the caller has already built. Anything nil is constructed from cfg, so
// production code passes nothing and tests pass what they need to observe.
type Options struct {
	Config config.Config
	Logger *slog.Logger
	Store  *store.Store
	// Provider overrides the configured one (tests use the fake).
	Provider llm.Provider
	// Auth, if set, replaces the one built from cfg. Tests inject it to skip OAuth.
	Auth *auth.Auth
	// NoAuth builds a server with no sign-in at all, whatever the config says: the chat route is
	// not registered, and every auth route answers 503. It exists so tests can prove that
	// "anonymous paid generations" is impossible, and so an API-only deployment is expressible.
	NoAuth bool
}

// New builds the app. It fails on an unusable configuration rather than serving a half-working
// endpoint: the errors name the setting and the fix.
func New(ctx context.Context, opts Options) (*App, error) {
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	st := opts.Store
	if st == nil {
		var err error
		st, err = store.Open(ctx, store.Options{Path: cfg.DB})
		if err != nil {
			return nil, err
		}
	}

	provider := opts.Provider
	if provider == nil {
		var err error
		provider, err = llm.New(cfg.LLM())
		if err != nil && !errors.Is(err, llm.ErrUnavailable) {
			return nil, err
		}
		if errors.Is(err, llm.ErrUnavailable) {
			// The server keeps serving the simulator. Chat will answer 503 with the reason,
			// which is better than refusing to boot over a secret a deploy can fix.
			log.Warn("the model provider is unavailable; chat will answer 503", "err", err)
			provider = &unavailable{err: err}
		}
	}

	authCfg := auth.Config{
		BaseURL:    cfg.BaseURL,
		Dev:        cfg.Dev,
		SessionTTL: cfg.SessionTTL,
		CookieKey:  cfg.SessionKey,
		Logger:     log,
		Providers:  providers(cfg),
	}
	a := opts.Auth
	if opts.NoAuth {
		a = nil
	}
	if a == nil && !opts.NoAuth {
		if cfg.BaseURL == "" {
			// No origin means no OAuth callbacks. An explicit, honest 503 beats a router that
			// panics on a missing config value.
			log.Warn("BASE_URL is unset: sign-in is disabled, so chat is unreachable",
				"fix", "set BASE_URL to the public origin, e.g. https://newton.example.com")
		} else {
			var err error
			a, err = auth.New(authCfg, st)
			if err != nil {
				return nil, err
			}
		}
	}

	qm := &quota.Manager{
		Store: st,
		Limits: quota.Limits{
			DailyGenerations: cfg.DailyGenerations,
			MaxTurns:         cfg.MaxConversationTurns,
			DailyCostMicros:  cfg.DailyCostMicros,
			StaleAfter:       quota.DefaultStaleAfter,
		},
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = web.ErrorHandler(log)

	// Middleware order matters. Recover first so a panic is a 500 rather than a dead connection;
	// RequestID before the body limit so a rejected request still has an id in the log.
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(requestLogger(log))
	e.Use(web.SecurityHeaders())
	e.Use(middleware.BodyLimit(fmt.Sprintf("%dB", chat.BodyLimit())))
	// Rate limiting is per IP and cheap; the real spend control is the per-user quota below, but
	// this blunts a script that opens thousands of connections.
	e.Use(middleware.RateLimiter(middleware.NewRateLimiterMemoryStoreWithConfig(
		middleware.RateLimiterMemoryStoreConfig{
			// Measured, not guessed: one cold page load is ~40 requests (index.html, dist/**, the
			// zod tree, planck.mjs). A burst of 20 at 1/s answered a real visitor's page load with
			// 429s, which is a broken app; a browser test caught it. 300/20/s absorbs the largest
			// tree while still making a scripted flood impractical.
			Rate:      20,              // sustained requests per second per IP
			Burst:     300,             // one cold page load plus slack
			ExpiresIn: 3 * time.Minute, // an idle visitor forgets their history
		})))
	e.Use(web.CSRF())
	if a != nil {
		// Resolves the session cookie on every request and stores the user in the context. It
		// never refuses, because the simulator must work anonymously; RequireUser is the gate,
		// applied only to the routes that cost money.
		e.Use(a.Middleware())
	}

	// Behind Traefik the real client IP is in X-Forwarded-For. Trusting it blindly would let a
	// client forge its own IP for the rate limiter, so this is only enabled when the app is
	// actually behind a proxy (which, in this deployment, it always is: the container is not
	// published).
	e.IPExtractor = echo.ExtractIPFromXFFHeader()

	app := &App{Echo: e, Auth: a, Quota: qm, Store: st, Provider: provider, cfg: cfg, log: log}

	// Registration order is the routing policy, so it is explicit:
	//
	//   1. concrete /api routes (health, config, me, auth, chat)
	//   2. /api/*        → JSON 404, so an unknown API path never leaks a static file
	//   3. /*            → static files, with a real 404 for a miss
	//
	// Echo matches in registration order, so the catch-alls must come last. Registering the chat
	// route after the /api/* fallback would shadow it behind a 404, which is exactly the bug this
	// ordering exists to prevent.
	app.registerAPI()
	if a != nil {
		// The chat route exists only when there is a way to authenticate. A route that answers 401
		// for everybody would be honest but useless; a 404 for a nonexistent endpoint is clearer,
		// and it makes "anonymous, paid generations" structurally impossible rather than a missing
		// check.
		chatHandler, err := app.buildChat(provider)
		if err != nil {
			return nil, err
		}
		app.Chat = chatHandler
		group := e.Group("/api")
		group.Use(a.RequireUser())
		app.Chat.Register(group)
	}

	e.Any("/api/*", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "no such API endpoint")
	})

	static := web.Static{Root: cfg.Static}.Handler()
	e.GET("/*", static)
	// HEAD explicitly: echo does not derive it from GET, and a HEAD on a static file is what a
	// proxy or `curl -I` uses to check a Content-Type.
	e.HEAD("/*", static)

	return app, nil
}

// buildChat wires the chat handler. A nil user resolver would mean anonymous, paid generations, so
// the handler refuses to be built without one; the caller decides that only when there is no auth
// at all, in which case the route is not registered either.
func (a *App) buildChat(provider llm.Provider) (*chat.Handler, error) {
	// The only caller guarantees auth, so this resolver can be unconditional. Wiring in one place
	// means a second call site cannot accidentally pass an anonymous resolver.
	user := func(c echo.Context) (string, bool) {
		u, ok := auth.UserFrom(c)
		return u.ID, ok
	}

	h, err := chat.New(chat.Config{
		Enabled:  a.cfg.ChatEnabled,
		Provider: provider,
		Quota:    a.Quota,
		User:     user,
		Logger:   a.log,
		Model:    a.cfg.Model,
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// registerAPI mounts the routes that do not depend on a signed-in user.
func (a *App) registerAPI() {
	api := a.Echo.Group("/api")

	api.GET("/health", a.health)
	api.GET("/config", a.publicConfig)

	if a.Auth != nil {
		api.GET("/auth/:provider", a.Auth.SignIn)
		api.GET("/auth/:provider/callback", a.Auth.Callback)
		api.POST("/auth/logout", a.Auth.Logout, a.Auth.RequireUser())
		api.GET("/me", a.me)
	} else {
		// One honest answer for every auth route, so the client's degradation matrix has a
		// single "not configured" case instead of a 404 it has to interpret.
		disabled := func(c echo.Context) error {
			return echo.NewHTTPError(http.StatusServiceUnavailable,
				"sign-in is not configured on this server: set BASE_URL and an OAuth provider").SetInternal(errors.New("app: no auth"))
		}
		api.Any("/me", disabled)
		api.Any("/auth/*", disabled)
	}
}

// health reports liveness plus what the process actually has. It runs a real query: a healthcheck
// that only proves the process is up is the reason Swarm rolls back on a broken database.
func (a *App) health(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()

	migrated := true
	version, err := a.Store.Version(ctx)
	if err != nil {
		migrated = false
	}
	var one int
	if err := a.Store.DB().QueryRowContext(ctx, "select 1").Scan(&one); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"ok": false, "version": a.cfg.Version, "migrated": migrated, "error": "the database is unreachable",
		})
	}

	status := http.StatusOK
	if !migrated {
		status = http.StatusServiceUnavailable
	}
	return c.JSON(status, map[string]any{
		"ok":       status == http.StatusOK,
		"version":  a.cfg.Version,
		"migrated": migrated,
		"schema":   version,
		"chat":     a.cfg.ChatEnabled,
	})
}

// publicConfig tells an anonymous browser what the server can do, so the UI does not have to
// discover it by triggering errors. It must never include a secret or a limit an attacker could
// use to time an attack; the numbers here are already public in the plan.
func (a *App) publicConfig(c echo.Context) error {
	providers := []string{}
	if a.Auth != nil {
		providers = a.Auth.Providers()
	}
	return c.JSON(http.StatusOK, map[string]any{
		"chatEnabled": a.cfg.ChatEnabled,
		"providers":   providers,
		"dev":         a.cfg.Dev,
		"version":     a.cfg.Version,
	})
}

// me is the authenticated user's view of itself, including the quota counters the composer shows.
func (a *App) me(c echo.Context) error {
	user, ok := auth.UserFrom(c)
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, "sign in to use the AI")
	}

	usage, err := a.Quota.Today(c.Request().Context(), user.ID)
	if err != nil {
		a.log.ErrorContext(c.Request().Context(), "reading quota", "user", user.ID, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			"could not read your usage; try again").SetInternal(err)
	}
	pending, err := a.Store.PendingCount(c.Request().Context(), user.ID)
	if err != nil {
		// The pending count is a nicety; its failure must not break /api/me.
		pending = 0
	}

	return c.JSON(http.StatusOK, map[string]any{
		"id":          user.ID,
		"displayName": user.DisplayName,
		"quota": map[string]any{
			"used":     usage.Used,
			"limit":    usage.Limit,
			"resetsAt": usage.ResetsAt.Format(time.RFC3339),
		},
		"inFlight":    pending > 0,
		"chatEnabled": a.cfg.ChatEnabled,
	})
}

// providers builds the configured sign-in providers from the config.
func providers(cfg config.Config) []auth.Provider {
	var out []auth.Provider
	if cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "" {
		out = append(out, auth.Provider{
			Name:         "github",
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
		})
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		out = append(out, auth.Provider{
			Name:         "google",
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
		})
	}
	return out
}

// requestLogger logs one line per request at a level that matches the outcome. It deliberately does
// not log a body, a prompt, or a completion.
func requestLogger(log *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			if errors.Is(err, echo.ErrNotFound) {
				// A 404 from a static miss is normal noise; the error handler already
				// records it at debug level.
				return err
			}
			status := c.Response().Status
			if err != nil {
				if httpErr, ok := err.(*echo.HTTPError); ok {
					status = httpErr.Code
				}
			}
			log.LogAttrs(c.Request().Context(), levelFor(status), "request",
				slog.String("method", c.Request().Method),
				slog.String("path", c.Request().URL.Path),
				slog.Int("status", status),
				slog.Duration("took", time.Since(start)),
				slog.String("request_id", c.Response().Header().Get(echo.HeaderXRequestID)),
			)
			return err
		}
	}
}

func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// Start runs the server until ctx is canceled, then shuts it down gracefully.
//
// Graceful matters for SSE: Shutdown waits for active requests, and a client mid-generation gets
// its terminal frame instead of a reset connection. The timeout is the ceiling on that wait.
func (a *App) Start(ctx context.Context, log *slog.Logger) error {
	server := &http.Server{
		Addr:              a.cfg.Addr,
		Handler:           a.Echo,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		IdleTimeout:       config.IdleTimeout,
		// No WriteTimeout: SSE streams are long-lived and a write deadline would cut every
		// generation off mid-answer. Per-call bounds come from the provider's own timeout.
		ReadTimeout: 0,
	}

	done := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", a.cfg.Addr, "static", a.cfg.Static)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			done <- err
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	// Settle token counts before exiting: a generation whose client is still connected gets its
	// own shutdown window.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	log.Info("shutting down")
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("app: graceful shutdown failed: %w", err)
	}
	return nil
}

// unavailable is the provider that answers everything with 503's cause. It exists so a missing API
// key degrades one endpoint instead of the whole process.
type unavailable struct{ err error }

func (u *unavailable) Chat(context.Context, llm.Request, func(string) error) (llm.Usage, error) {
	return llm.Usage{}, u.err
}

func (u *unavailable) String() string { return "unavailable" }
