package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envMap is a getenv for tests: no process environment, no ordering surprises.
func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// noFiles makes any secret-file read fail loudly: a test that expects a file should provide one.
func noFiles(t *testing.T) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		t.Helper()
		t.Fatalf("unexpected read of secret file %q", path)
		return nil, os.ErrNotExist
	}
}

func readableFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if content, ok := files[path]; ok {
			return []byte(content), nil
		}
		return nil, os.ErrNotExist
	}
}

// validForProduction is the smallest configuration that passes Validate.
func validForProduction(t *testing.T) map[string]string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"BASE_URL":                  "https://newton.example.com",
		"STATIC":                    dir,
		"GITHUB_CLIENT_ID_FILE":     "/run/secrets/github_client_id",
		"GITHUB_CLIENT_SECRET_FILE": "/run/secrets/github_client_secret",
		"SESSION_KEY_FILE":          "/run/secrets/session_key",
	}
}

func TestDefaultsServeTheSimulatorAndSpendNothing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load([]string{"-static", dir, "-db", filepath.Join(dir, "n.db")}, envMap(nil), noFiles(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChatEnabled {
		t.Error("chat is enabled by default: the kill switch must default to off")
	}
	if cfg.ProviderKind != "fake" {
		t.Errorf("provider = %q, want fake by default", cfg.ProviderKind)
	}
	if cfg.DailyGenerations != 10 {
		t.Errorf("DAILY_GENERATIONS = %d, want 10", cfg.DailyGenerations)
	}
	if cfg.MaxConversationTurns != 6 {
		t.Errorf("MAX_CONVERSATION_TURNS = %d, want 6", cfg.MaxConversationTurns)
	}
	if cfg.SessionTTL != 30*24*time.Hour {
		t.Errorf("SessionTTL = %s, want 30 days", cfg.SessionTTL)
	}
}

func TestSecretsAreReadFromFiles(t *testing.T) {
	env := validForProduction(t)
	cfg, err := Load(nil, envMap(env), readableFiles(map[string]string{
		"/run/secrets/github_client_id":     "Iv1.abc\n",
		"/run/secrets/github_client_secret": " hunter2 \n",
		"/run/secrets/session_key":          "0123456789abcdef0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Trailing newlines are an artifact of every secret-creation workflow; they are trimmed so a
	// cookie signature does not silently include one.
	if cfg.GitHubClientID != "Iv1.abc" {
		t.Errorf("client id = %q, want it trimmed", cfg.GitHubClientID)
	}
	if cfg.GitHubClientSecret != "hunter2" {
		t.Errorf("client secret = %q, want it trimmed", cfg.GitHubClientSecret)
	}
	if len(cfg.SessionKey) != 32 {
		t.Errorf("session key = %d bytes, want 32", len(cfg.SessionKey))
	}
	// Redacted must not contain any of them.
	redacted := cfg.Redacted()
	for key, value := range redacted {
		if s, ok := value.(string); ok {
			if strings.Contains(s, "hunter2") || strings.Contains(s, "Iv1.abc") {
				t.Fatalf("Redacted() leaked a secret under %q", key)
			}
		}
	}
}

func TestMissingSecretFileNamesTheFix(t *testing.T) {
	env := validForProduction(t)
	missing := func(path string) ([]byte, error) { return nil, os.ErrNotExist }
	_, err := Load(nil, envMap(env), missing)
	if err == nil {
		t.Fatal("a missing secret file was accepted")
	}
	if !strings.Contains(err.Error(), "GITHUB_CLIENT_ID_FILE") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
	if !strings.Contains(err.Error(), "mount the secret") {
		t.Errorf("error = %q, want it to name the fix", err)
	}
}

func TestFlagsBeatEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(
		[]string{"-chat=false", "-provider", "fake", "-static", dir},
		envMap(map[string]string{"CHAT_ENABLED": "true", "PROVIDER": "openai"}),
		noFiles(t),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChatEnabled {
		t.Error("CHAT_ENABLED=true beat -chat=false")
	}
	if cfg.ProviderKind != "fake" {
		t.Errorf("provider = %q, want the flag to win", cfg.ProviderKind)
	}
}

func TestValidateNamesEveryProblemAtOnce(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr:                 "",
		Static:               filepath.Join(dir, "nope"),
		DB:                   "",
		BaseURL:              "newton.example.com",
		ChatEnabled:          true,
		ProviderKind:         "gpt",
		DailyGenerations:     -1,
		MaxConversationTurns: 6,
		SessionTTL:           0,
		LogLevel:             "loud",
		GitHubClientID:       "only-the-id",
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a broken configuration")
	}
	message := err.Error()
	for _, want := range []string{
		"listen address",
		"STATIC",
		"database path",
		"BASE_URL",
		"fake and openai",
		"half-configured",
		"SESSION_TTL",
		"DAILY_GENERATIONS",
		"LOG_LEVEL",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not mention %q:\n%s", want, message)
		}
	}
}

func TestChatEnabledNeedsAProviderAndSignIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Chat on, no OAuth: nobody could reach it.
	_, err := Load(
		[]string{"-chat", "-static", dir},
		envMap(map[string]string{"BASE_URL": "https://newton.example.com"}),
		noFiles(t),
	)
	if err == nil || !strings.Contains(err.Error(), "no OAuth provider") {
		t.Fatalf("err = %v, want it to say sign-in is missing", err)
	}

	// Chat on with a real provider and no key.
	_, err = Load(
		[]string{"-chat", "-provider", "openai", "-static", dir},
		envMap(map[string]string{
			"BASE_URL":                  "https://newton.example.com",
			"MODEL":                     "some-model",
			"GITHUB_CLIENT_ID_FILE":     "/run/secrets/id",
			"GITHUB_CLIENT_SECRET_FILE": "/run/secrets/secret",
			"SESSION_KEY_FILE":          "/run/secrets/key",
		}),
		readableFiles(map[string]string{
			"/run/secrets/id":     "id",
			"/run/secrets/secret": "secret",
			"/run/secrets/key":    strings.Repeat("k", 32),
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "PROVIDER_API_KEY") {
		t.Fatalf("err = %v, want it to name the missing API key", err)
	}
}

func TestHTTPSIsRequiredOutsideDev(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"BASE_URL": "http://newton.example.com", "STATIC": dir}
	_, err := Load(nil, envMap(env), noFiles(t))
	if err == nil || !strings.Contains(err.Error(), "__Host-") {
		t.Fatalf("err = %v, want it to explain the cookie requirement", err)
	}

	// In dev, http://localhost is the only sensible origin.
	if _, err := Load([]string{"-dev", "-static", dir}, envMap(env), noFiles(t)); err == nil {
		t.Fatal("dev mode accepted a non-localhost origin")
	}
	if _, err := Load([]string{"-dev", "-static", dir}, envMap(map[string]string{
		"BASE_URL": "http://localhost:8080", "STATIC": dir,
	}), noFiles(t)); err != nil {
		t.Fatalf("dev mode rejected localhost: %v", err)
	}
}

func TestShortSessionKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]string{"-static", dir}, envMap(map[string]string{
		"SESSION_KEY": strings.Repeat("k", 8),
	}), noFiles(t))
	if err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("err = %v, want it to require 32 bytes", err)
	}
	if !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("err = %q, want a way to generate one", err)
	}
}

func TestHelpRequestIsDistinct(t *testing.T) {
	_, err := Load([]string{"-h"}, envMap(nil), noFiles(t))
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("err = %v, want ErrHelp so main can exit 0", err)
	}
	if !strings.Contains(Help(), "SESSION_KEY_FILE") {
		t.Error("Help does not mention the secret file variables")
	}
	if strings.Contains(Help(), "hunter2") {
		t.Error("Help contains a secret")
	}
}

func TestInvalidEnvironmentValueFallsBackToTheDefault(t *testing.T) {
	// A typo in a boolean must not silently become false and disable the kill switch the wrong
	// way round: the default stands, and the startup log shows the real value.
	dir := t.TempDir()
	cfg, err := Load([]string{"-static", dir}, envMap(map[string]string{"CHAT_ENABLED": "yes please"}), noFiles(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChatEnabled {
		t.Error("an unparseable boolean enabled chat")
	}
}
