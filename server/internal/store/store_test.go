package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock is the test clock. Every quota test uses it, so no test sleeps.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testDay is a Tuesday noon UTC: far from a midnight boundary unless a test moves it.
var testDay = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// openTest returns a store on a fresh temporary database, closed when the test ends.
func openTest(t *testing.T) (*Store, *fixedClock) {
	t.Helper()
	clock := newClock(testDay)
	s, err := Open(context.Background(), Options{
		Path: filepath.Join(t.TempDir(), "newton.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clock
}

func TestOpenAppliesSchema(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	version, err := s.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1 (the init migration)", version)
	}

	// Every table the rest of the server relies on has to exist after Open.
	for _, table := range []string{"users", "identities", "sessions", "generations", "goose_db_version"} {
		var name string
		err := s.DB().QueryRowContext(ctx,
			`select name from sqlite_master where type = 'table' and name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %q is missing after migrations: %v", table, err)
		}
	}
}

// The DSN pragmas are load-bearing: WAL is what Litestream replicates, foreign keys are how
// the cascades work, and _txlock=immediate is why no query in this package can fail on a lock
// upgrade. A Go or driver upgrade that silently stopped applying them would be invisible until
// production, so the values are asserted rather than assumed.
func TestDSNPragmasAreApplied(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"synchronous", "1"}, // NORMAL
	} {
		var got string
		if err := s.DB().QueryRowContext(ctx, "pragma "+tc.pragma).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("pragma %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}

	var busy int
	if err := s.DB().QueryRowContext(ctx, "pragma busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("pragma busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestDSNEscapesPaths(t *testing.T) {
	dsn := DSN("/tmp/a dir/newton.db")
	if !strings.HasPrefix(dsn, "file:/tmp/a%20dir/newton.db?") {
		t.Fatalf("DSN = %q, want the path escaped and prefixed with file:", dsn)
	}
}

func TestOnePendingIndexExists(t *testing.T) {
	s, _ := openTest(t)
	var ddl string
	err := s.DB().QueryRowContext(context.Background(),
		`select sql from sqlite_master where type = 'index' and name = 'one_pending'`).Scan(&ddl)
	if err != nil {
		t.Fatalf("one_pending index is missing: %v", err)
	}
	if !strings.Contains(ddl, "where status = 'pending'") {
		t.Errorf("one_pending is not partial: %s", ddl)
	}
}

func newUser(t *testing.T, s *Store, email string) User {
	t.Helper()
	u, err := s.UpsertIdentity(context.Background(), Identity{
		Provider:       "github",
		ProviderUserID: "gh-" + email,
		Email:          email,
		DisplayName:    "Test " + email,
	})
	if err != nil {
		t.Fatalf("UpsertIdentity(%s): %v", email, err)
	}
	return u
}

func TestUpsertIdentityIsStable(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	first, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "42", Email: "a@example.com", DisplayName: "Ada"})
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "42", Email: "a@example.com", DisplayName: "Ada Lovelace"})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("same provider id produced two users: %s then %s", first.ID, second.ID)
	}
	if second.DisplayName != "Ada Lovelace" {
		t.Errorf("display name = %q, want the provider's current value", second.DisplayName)
	}
	if !second.CreatedAt.Equal(testDay) {
		t.Errorf("created_at = %s, want the injected clock (%s)", second.CreatedAt, testDay)
	}
}

// A returning user keeps their account even after changing their address at the provider.
func TestUpsertIdentityPrefersProviderIDOverEmail(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	first, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "7", Email: "old@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Someone else now owns old@example.com at a *different* provider.
	other, err := s.UpsertIdentity(ctx, Identity{Provider: "google", ProviderUserID: "g-1", Email: "old@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID != first.ID {
		t.Fatalf("same verified email made a second account: %s vs %s", first.ID, other.ID)
	}

	// The GitHub user changes their email; the account must not move.
	moved, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "7", Email: "new@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != first.ID {
		t.Fatalf("changing the provider email created a new account: %s then %s", first.ID, moved.ID)
	}
}

func TestUpsertIdentityLinksByVerifiedEmail(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	github, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "1", Email: "Same@Example.com"})
	if err != nil {
		t.Fatal(err)
	}
	google, err := s.UpsertIdentity(ctx, Identity{Provider: "google", ProviderUserID: "2", Email: " same@example.com "})
	if err != nil {
		t.Fatal(err)
	}
	if github.ID != google.ID {
		t.Fatalf("verified email did not link identities: %s vs %s", github.ID, google.ID)
	}
}

// No email means no linking: an identity without a verified address gets its own account.
func TestUpsertIdentityWithoutEmailDoesNotLink(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	a, err := s.UpsertIdentity(ctx, Identity{Provider: "github", ProviderUserID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.UpsertIdentity(ctx, Identity{Provider: "google", ProviderUserID: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("identities with no email were merged into one account")
	}
	if a.Email != "" {
		t.Errorf("email = %q, want empty", a.Email)
	}
}

func TestUpsertIdentityRejectsIncompleteIdentity(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.UpsertIdentity(context.Background(), Identity{Provider: "github"})
	if err == nil {
		t.Fatal("an identity without a provider user id was accepted")
	}
	if !strings.Contains(err.Error(), "provider user id") {
		t.Errorf("error = %q, want it to name the missing field", err)
	}
}

func TestDisabledUserIsReported(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "ban@example.com")

	// A live session must be revoked by the ban, not merely refused on its next use.
	if _, err := s.CreateSession(ctx, u.ID, "ban-me", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	got, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Fatal("user is not marked disabled")
	}
	var sessions int
	if err := s.DB().QueryRowContext(ctx, `select count(*) from sessions where user_id = ?`, u.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("%d sessions survived the ban: un-banning would resurrect them", sessions)
	}

	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatalf("un-disabling: %v", err)
	}
	got, err = s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disabled {
		t.Fatal("user is still disabled after clearing")
	}

	if err := s.SetDisabled(ctx, "nobody", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabling an unknown user = %v, want ErrNotFound", err)
	}
}
