// Command api is the Newton server: static files plus an authenticated, quota-metered relay to a
// language model.
//
// It has two modes. `newton -healthcheck` does one HTTP GET against the local /api/health and exits
// 0 or 1, because the final image is distroless and has no shell for a curl-based healthcheck.
// Everything else starts the server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kaki-studios/newton/server/internal/app"
	"github.com/kaki-studios/newton/server/internal/config"
)

// version is stamped at build time: -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, config.ErrHelp) {
			fmt.Println(config.Help())
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// -healthcheck and -admin are handled before any config validation: they run inside the same
	// container as a server that already validated its own config, so a check or an operator
	// command that failed because of an unrelated setting would be a false negative.
	if healthcheckRequested(args) {
		return healthcheck()
	}
	if _, _, ok := adminRequested(args); ok {
		return runAdmin(args, os.Stdout, os.Stderr)
	}

	cfg, err := config.Load(args, os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	cfg.Version = version

	log := newLogger(cfg)
	log.Info("starting", "settings", cfg.Redacted())

	// A subtle trap: with no OAuth provider the auth subsystem cannot be built, and the chat route
	// is not registered at all. That is a 404 rather than a 503, which would confuse the client's
	// degradation matrix, so it is stated here instead of discovered in the browser.
	if cfg.ChatEnabled && cfg.BaseURL == "" {
		log.Warn("CHAT_ENABLED is true but BASE_URL is unset: nobody can sign in, so chat is unreachable",
			"fix", "set BASE_URL to the public origin")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, app.Options{Config: cfg, Logger: log})
	if err != nil {
		return err
	}

	// Sweep expired sessions in the background. The read path already refuses an expired session,
	// so this is housekeeping rather than correctness: without it the table grows forever.
	go sweepSessions(ctx, application, log)

	return application.Start(ctx, log)
}

// healthcheckRequested looks for -healthcheck without parsing the rest: config.Load would fail on a
// missing directory that the healthcheck never touches.
func healthcheckRequested(args []string) bool {
	for _, arg := range args {
		if arg == "-healthcheck" || arg == "--healthcheck" {
			return true
		}
	}
	return false
}

// healthcheck does one GET against the local server. It is what `HEALTHCHECK CMD ["/app/newton",
// "-healthcheck"]` runs: distroless has no shell, so the binary has to be able to check itself.
func healthcheck() error {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// The listen address is usually ":8080" (all interfaces); the client needs a host.
	if host, port, err := net.SplitHostPort(addr); err == nil && host == "" {
		addr = net.JoinHostPort("127.0.0.1", port)
	} else if err != nil {
		return fmt.Errorf("healthcheck: cannot parse ADDR %q as host:port: %w", addr, err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/health")
	if err != nil {
		return fmt.Errorf("healthcheck: GET http://%s/api/health failed: %w\nThe server is not accepting connections", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: /api/health returned %d; the database or the migrations are unhealthy", resp.StatusCode)
	}
	return nil
}

// sweepSessions deletes expired session rows hourly.
func sweepSessions(ctx context.Context, a *app.App, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A short deadline: a hung database must not pile up sweepers.
			sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			n, err := a.Store.DeleteExpiredSessions(sweepCtx)
			cancel()
			if err != nil {
				log.Error("sweeping expired sessions", "err", err)
				continue
			}
			if n > 0 {
				log.Info("swept expired sessions", "count", n)
			}
		}
	}
}

// newLogger builds the process logger. JSON in production so a log collector can parse it, text in
// dev so a human can read it.
func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.Dev {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler).With("service", "newton", "version", version)
}
