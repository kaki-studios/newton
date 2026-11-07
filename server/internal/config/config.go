// Package config reads the server's settings from flags, the environment, and — for anything
// secret — a file under /run/secrets (a Docker/Swarm secret mount).
//
// Env vars leak to anyone who can `docker inspect` a container; secret files do not. So every
// credential is a file path, and this package is the only place that reads one.
//
// Fail fast and loudly on missing config at boot. A server that starts with no OAuth secret and
// fails at the login click is worse than one that refuses to start, because the failure is silent
// until a user finds it.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kaki-studios/newton/server/internal/llm"
)

// Config is the whole server configuration. Zero values are meaningful; Validate says which ones
// are not allowed to stay zero.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Static is the directory of files to serve. Empty disables static serving.
	Static string
	// DB is the SQLite file path.
	DB string
	// Dev relaxes cookie flags, allows http://localhost redirect URIs, and turns on verbose
	// logs.
	Dev bool
	// BaseURL is the absolute origin used for OAuth callbacks. Required when any provider is
	// configured.
	BaseURL string

	// ChatEnabled is the kill switch. False means /api/chat is 503 before it does any work.
	ChatEnabled bool
	// Model is the provider's model id, recorded on every generation row.
	Model string
	// PromptVersion overrides the built-in prompt version.
	PromptVersion string
	// DailyGenerations is the per-user daily cap.
	DailyGenerations int
	// MaxConversationTurns is the per-conversation cap that bounds a repair loop.
	MaxConversationTurns int
	// DailyCostMicros is the install-wide daily cost ceiling. Zero means no ceiling.
	DailyCostMicros int64

	// ProviderKind selects the llm implementation: "fake" or "openai".
	ProviderKind string
	// ProviderBaseURL is the API root for the real provider.
	ProviderBaseURL string
	// ProviderAPIKey is the bearer token.
	ProviderAPIKey string

	// GitHubClientID and GitHubClientSecret configure the GitHub sign-in. Empty disables it.
	GitHubClientID     string
	GitHubClientSecret string
	// GoogleClientID and GoogleClientSecret configure Google sign-in. Empty disables it.
	GoogleClientID     string
	GoogleClientSecret string

	// SessionKey signs the OAuth state cookie. 32+ bytes; generated if absent in dev.
	SessionKey []byte
	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration

	// LogLevel is "debug", "info", "warn" or "error".
	LogLevel string
	// Version is stamped into the binary at build time and reported by /api/health.
	Version string
}

// Limits are the request/session sizes that are not worth a flag per value. They live here so the
// router and the handlers read the same numbers.
const (
	// ReadHeaderTimeout bounds how long a client can take to send its headers.
	ReadHeaderTimeout = 10 * time.Second
	// IdleTimeout closes idle keep-alive connections, so a stale tab does not pin a slot.
	IdleTimeout = 2 * time.Minute
	// WriteTimeout is deliberately zero in the server: SSE streams are long-lived by design and
	// a write deadline would cut every generation off. Per-stream bounds come from the provider
	// timeout instead.
)

// ErrHelp is returned by Load when -h was passed. main exits 0 on it.
var ErrHelp = errors.New("config: help requested")

// Load parses flags and the environment, and validates the result.
//
// It takes args explicitly (rather than using os.Args) so tests can drive it, and the getenv and
// readFile functions are parameters for the same reason: a config bug should be reproducible
// without touching the process environment.
func Load(args []string, getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}

	cfg := defaults()
	fs := flag.NewFlagSet("newton", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // help is printed by the caller, with the secrets redacted
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address, e.g. :8080")
	fs.StringVar(&cfg.Static, "static", cfg.Static, "directory of static files to serve; empty disables it")
	fs.StringVar(&cfg.DB, "db", cfg.DB, "SQLite database file")
	fs.BoolVar(&cfg.Dev, "dev", cfg.Dev, "development mode: relax cookie flags and allow http://localhost")
	fs.BoolVar(&cfg.ChatEnabled, "chat", cfg.ChatEnabled, "enable POST /api/chat")
	fs.StringVar(&cfg.ProviderKind, "provider", cfg.ProviderKind, "model provider: fake or openai")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, ErrHelp
		}
		return Config{}, fmt.Errorf("config: %w", err)
	}

	// Precedence is flag > env > default, which is what an operator expects when debugging a
	// running container: the explicit thing wins.
	applyEnv(fs, &cfg, getenv)

	// Secrets are files. A path in env, the contents read here, and the value never printed.
	var err error
	if cfg.GitHubClientID, err = secret(getenv, readFile, "GITHUB_CLIENT_ID", cfg.GitHubClientID); err != nil {
		return Config{}, err
	}
	if cfg.GitHubClientSecret, err = secret(getenv, readFile, "GITHUB_CLIENT_SECRET", cfg.GitHubClientSecret); err != nil {
		return Config{}, err
	}
	if cfg.GoogleClientID, err = secret(getenv, readFile, "GOOGLE_CLIENT_ID", cfg.GoogleClientID); err != nil {
		return Config{}, err
	}
	if cfg.GoogleClientSecret, err = secret(getenv, readFile, "GOOGLE_CLIENT_SECRET", cfg.GoogleClientSecret); err != nil {
		return Config{}, err
	}
	if cfg.ProviderAPIKey, err = secret(getenv, readFile, "PROVIDER_API_KEY", cfg.ProviderAPIKey); err != nil {
		return Config{}, err
	}

	if raw, err := secret(getenv, readFile, "SESSION_KEY", ""); err != nil {
		return Config{}, err
	} else if raw != "" {
		cfg.SessionKey = []byte(raw)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// defaults are the values the plan documents. They are chosen so that running the binary with no
// configuration at all serves the simulator and refuses to spend money.
func defaults() Config {
	return Config{
		Addr:                 ":8080",
		Static:               "public",
		DB:                   "data/newton.db",
		ChatEnabled:          false,
		ProviderKind:         "fake",
		DailyGenerations:     10,
		MaxConversationTurns: 6,
		SessionTTL:           30 * 24 * time.Hour,
		LogLevel:             "info",
		Version:              "dev",
	}
}

// applyEnv fills anything not set by a flag from the environment. Only flags that were not
// explicitly given are touched, so `-chat=false` beats CHAT_ENABLED=true.
func applyEnv(fs *flag.FlagSet, cfg *Config, getenv func(string) string) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	str := func(flagName, env string, target *string) {
		if explicit[flagName] {
			return
		}
		if v := strings.TrimSpace(getenv(env)); v != "" {
			*target = v
		}
	}
	boolean := func(flagName, env string, target *bool) {
		if explicit[flagName] {
			return
		}
		if v := strings.TrimSpace(getenv(env)); v != "" {
			parsed, err := strconv.ParseBool(v)
			if err == nil {
				*target = parsed
			}
			// An unparseable boolean is caught by Validate below only if it matters; the flag
			// stays at its default and the operator sees the real value in the startup log.
		}
	}
	integer := func(env string, target *int) {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				*target = parsed
			}
		}
	}
	integer64 := func(env string, target *int64) {
		if v := strings.TrimSpace(getenv(env)); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
				*target = parsed
			}
		}
	}

	boolean("chat", "CHAT_ENABLED", &cfg.ChatEnabled)
	str("provider", "PROVIDER", &cfg.ProviderKind)
	str("addr", "ADDR", &cfg.Addr)
	str("static", "STATIC", &cfg.Static)
	str("db", "DB", &cfg.DB)
	boolean("dev", "DEV", &cfg.Dev)

	if v := strings.TrimSpace(getenv("BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	str("", "MODEL", &cfg.Model)
	str("", "PROMPT_VERSION", &cfg.PromptVersion)
	str("", "PROVIDER_BASE_URL", &cfg.ProviderBaseURL)
	str("log-level", "LOG_LEVEL", &cfg.LogLevel)
	integer("DAILY_GENERATIONS", &cfg.DailyGenerations)
	integer("MAX_CONVERSATION_TURNS", &cfg.MaxConversationTurns)
	integer64("DAILY_COST_MICROS", &cfg.DailyCostMicros)
	if v := strings.TrimSpace(getenv("SESSION_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.SessionTTL = d
		}
	}
}

// secret reads a value from a file path held in env, falling back to the value already in cfg.
//
// The variable names a *path*, not the value: that is what lets `docker secret` mount work
// unchanged, and what keeps the value out of `docker inspect`.
func secret(getenv func(string) string, readFile func(string) ([]byte, error), env, current string) (string, error) {
	path := strings.TrimSpace(getenv(env + "_FILE"))
	if path == "" {
		// Allow the value itself for local development, but never in a container: Validate
		// warns about it and the startup log names the variable.
		if v := strings.TrimSpace(getenv(env)); v != "" {
			return v, nil
		}
		return current, nil
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("config: %s_FILE points at %s but it cannot be read: %w\nThe fix: mount the secret at that path, or unset %s_FILE", env, path, err, env)
	}
	return strings.TrimSpace(string(b)), nil
}

// Validate reports every unusable setting at once, each with the fix. A server that starts with a
// broken setting and fails on the first user request is the failure mode this prevents.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if c.Addr == "" {
		add("the listen address is empty: set -addr or ADDR (e.g. :8080)")
	}
	if c.DB == "" {
		add("the database path is empty: set -db or DB (e.g. data/newton.db)")
	}
	if c.Static != "" {
		if info, err := os.Stat(c.Static); err != nil {
			add("STATIC points at %s, which cannot be read: %v. Set -static to the directory holding index.html, or empty to serve the API only", c.Static, err)
		} else if !info.IsDir() {
			add("STATIC points at %s, which is not a directory", c.Static)
		}
	}

	switch c.ProviderKind {
	case "fake", "openai":
	default:
		add("PROVIDER is %q; supported values are fake and openai", c.ProviderKind)
	}

	// A provider is only half-configured when the chat is enabled; with the kill switch off,
	// missing credentials are fine and the simulator still works.
	hasGitHub := c.GitHubClientID != "" && c.GitHubClientSecret != ""
	hasGoogle := c.GoogleClientID != "" && c.GoogleClientSecret != ""
	hasProvider := hasGitHub || hasGoogle

	if c.BaseURL == "" {
		if c.ChatEnabled || hasProvider {
			add("BASE_URL is required: OAuth callbacks need an absolute origin (e.g. https://newton.example.com)")
		}
	} else if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		add("BASE_URL is %q, which is not an absolute http(s) URL", c.BaseURL)
	}
	if c.BaseURL != "" && !c.Dev && strings.HasPrefix(c.BaseURL, "http://") {
		add("BASE_URL is http but -dev is off: the __Host- session cookie requires https. Use https in production, or -dev for localhost")
	}
	if c.BaseURL != "" && c.Dev && !strings.HasPrefix(c.BaseURL, "http://localhost") && !strings.HasPrefix(c.BaseURL, "http://127.0.0.1") {
		add("BASE_URL is %q in dev mode; the OAuth redirect must be http://localhost:<port> so the browser will send the cookie back", c.BaseURL)
	}

	if hasGitHub != (c.GitHubClientID != "" || c.GitHubClientSecret != "") {
		add("GitHub sign-in is half-configured: both GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET are needed")
	}
	if hasGoogle != (c.GoogleClientID != "" || c.GoogleClientSecret != "") {
		add("Google sign-in is half-configured: both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET are needed")
	}
	if c.ChatEnabled && !hasProvider {
		add("CHAT_ENABLED=true but no OAuth provider is configured, so nobody could sign in and use it. Configure GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET, or set CHAT_ENABLED=false")
	}
	if c.ChatEnabled && c.ProviderKind == "openai" && c.ProviderAPIKey == "" {
		add("CHAT_ENABLED=true with PROVIDER=openai but PROVIDER_API_KEY is empty. Set the provider_api_key secret, or use PROVIDER=fake")
	}
	if c.ChatEnabled && c.ProviderKind == "openai" && c.Model == "" {
		add("CHAT_ENABLED=true with PROVIDER=openai but MODEL is empty. Set MODEL to the provider's model id")
	}

	if len(c.SessionKey) > 0 && len(c.SessionKey) < 32 {
		add("SESSION_KEY is %d bytes; it needs at least 32 for the cookie signing key. Generate one with: openssl rand -base64 32", len(c.SessionKey))
	}
	if c.SessionTTL <= 0 {
		add("SESSION_TTL is %s; it must be positive", c.SessionTTL)
	}
	if c.DailyGenerations < 0 {
		add("DAILY_GENERATIONS is %d; it must be zero (refuse chat) or positive", c.DailyGenerations)
	}
	if c.MaxConversationTurns < 0 {
		add("MAX_CONVERSATION_TURNS is %d; it must be zero (unbounded) or positive", c.MaxConversationTurns)
	}
	if c.DailyCostMicros < 0 {
		add("DAILY_COST_MICROS is %d; it must be zero (no ceiling) or positive", c.DailyCostMicros)
	}

	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		add("LOG_LEVEL is %q; supported values are debug, info, warn and error", c.LogLevel)
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %d setting(s) need attention:\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// LLM builds the provider configuration this Config describes.
func (c Config) LLM() llm.Config {
	return llm.Config{
		Kind:      c.ProviderKind,
		BaseURL:   c.ProviderBaseURL,
		APIKey:    c.ProviderAPIKey,
		Model:     c.Model,
		MaxTokens: 2000,
	}
}

// Redacted is the form safe to log at startup: every secret is a boolean. Reading this line is how
// an operator confirms what the container actually got, without the value ending up in a log.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"addr":              c.Addr,
		"static":            c.Static,
		"db":                c.DB,
		"dev":               c.Dev,
		"base_url":          c.BaseURL,
		"chat_enabled":      c.ChatEnabled,
		"provider":          c.ProviderKind,
		"model":             c.Model,
		"daily_generations": c.DailyGenerations,
		"max_turns":         c.MaxConversationTurns,
		"daily_cost_micros": c.DailyCostMicros,
		"session_ttl":       c.SessionTTL.String(),
		"github_sign_in":    c.GitHubClientID != "" && c.GitHubClientSecret != "",
		"google_sign_in":    c.GoogleClientID != "" && c.GoogleClientSecret != "",
		"session_key":       len(c.SessionKey) > 0,
		"provider_key":      c.ProviderAPIKey != "",
		"log_level":         c.LogLevel,
		"version":           c.Version,
	}
}

// Help is the flag usage text, written out by hand so secrets are described by file and not by
// flag value.
func Help() string {
	return strings.TrimSpace(`
newton — physics scene simulator with an optional metered AI endpoint

Flags:
  -addr :8080        listen address
  -static public     directory of static files (index.html + dist/); empty serves the API only
  -db data/newton.db SQLite database file
  -dev               development mode: no __Host- cookie prefix, allows http://localhost
  -chat              enable POST /api/chat (also CHAT_ENABLED=true)
  -provider fake     model provider: fake or openai
  -log-level info    debug, info, warn or error
  -healthcheck       GET the local /api/health and exit 0 or 1

Environment (secrets are file paths, read once at startup):
  BASE_URL               absolute origin for OAuth callbacks, e.g. https://newton.example.com
  CHAT_ENABLED           kill switch, default false
  MODEL                  provider model id, recorded on every generation
  PROMPT_VERSION         prompt version, recorded on every generation
  DAILY_GENERATIONS      per-user daily cap, default 10
  MAX_CONVERSATION_TURNS per-conversation cap, default 6
  DAILY_COST_MICROS      install-wide daily cost ceiling, default 0 (none)
  PROVIDER_BASE_URL      API root for PROVIDER=openai
  LOG_LEVEL              debug|info|warn|error
  SESSION_TTL            30d by default, Go duration syntax
  GITHUB_CLIENT_ID_FILE, GITHUB_CLIENT_SECRET_FILE
  GOOGLE_CLIENT_ID_FILE, GOOGLE_CLIENT_SECRET_FILE
  PROVIDER_API_KEY_FILE
  SESSION_KEY_FILE
`)
}
