package quota

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kaki-studios/newton/server/internal/store"
)

// testClock is the injected clock. No test in this package sleeps: a day boundary is 24 lines
// of arithmetic away.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// noon is the fixture instant: comfortably inside a UTC day in both directions.
var noon = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

type fixture struct {
	store *store.Store
	clock *testClock
	quota *Manager
	user  string
}

func newFixture(t *testing.T, limits Limits) *fixture {
	t.Helper()
	clock := &testClock{now: noon}
	s, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "newton.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u, err := s.UpsertIdentity(context.Background(), store.Identity{
		Provider: "github", ProviderUserID: "u1", Email: "q@example.com",
	})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}

	if limits.StaleAfter == 0 {
		limits.StaleAfter = DefaultStaleAfter
	}
	return &fixture{
		store: s,
		clock: clock,
		quota: &Manager{Store: s, Limits: limits},
		user:  u.ID,
	}
}

func (f *fixture) reserve(t *testing.T, conversation string) (Reservation, error) {
	t.Helper()
	return f.quota.Reserve(context.Background(), Request{
		UserID: f.user, ConversationID: conversation,
		Model: "fake", PromptVersion: "test",
	})
}

func (f *fixture) mustReserve(t *testing.T, conversation string) Reservation {
	t.Helper()
	res, err := f.reserve(t, conversation)
	if err != nil {
		t.Fatalf("Reserve(%s): %v", conversation, err)
	}
	return res
}

func (f *fixture) mustSettle(t *testing.T, res Reservation, out Outcome) {
	t.Helper()
	if err := res.Settle(context.Background(), out); err != nil {
		t.Fatalf("Settle: %v", err)
	}
}

// refusal extracts the Refusal from an ErrExceeded, failing the test if it is not one.
func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	if err == nil {
		t.Fatal("Reserve succeeded, want a refusal")
	}
	if !errors.Is(err, ErrExceeded) {
		t.Fatalf("Reserve = %v, want ErrExceeded", err)
	}
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("Reserve = %v, want a *Refusal in the chain", err)
	}
	return r
}

func TestReserveAndSettle(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 10, MaxTurns: 6})

	res := f.mustReserve(t, "c1")
	if res.ID == "" {
		t.Fatal("reservation has no id")
	}
	if !res.ReservedAt.Equal(noon) {
		t.Errorf("ReservedAt = %s, want the injected clock", res.ReservedAt)
	}

	// While pending, the row is visible and the slot is taken.
	if _, err := f.reserve(t, "c1"); refusal(t, err).Reason != ReasonInFlight {
		t.Fatalf("second concurrent reserve was not refused as in-flight: %v", err)
	}

	f.mustSettle(t, res, Outcome{Status: store.StatusOK, InputTokens: 120, OutputTokens: 80, CostMicros: 9})

	saved, err := f.store.GenerationByID(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != store.StatusOK || saved.InputTokens != 120 || saved.OutputTokens != 80 || saved.CostMicros != 9 {
		t.Fatalf("settled = %+v, want ok 120/80/9", saved)
	}

	// The slot is free again.
	f.mustReserve(t, "c1")
}

func TestDailyLimit(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 2, MaxTurns: 100})

	for i := range 2 {
		res := f.mustReserve(t, "c1")
		f.mustSettle(t, res, Outcome{Status: store.StatusOK})
		_ = i
	}

	r := refusal(t, func() error { _, err := f.reserve(t, "c1"); return err }())
	if r.Reason != ReasonDaily {
		t.Fatalf("reason = %q, want %q", r.Reason, ReasonDaily)
	}
	if r.Limit != 2 || r.Used != 2 {
		t.Errorf("refusal = %d/%d, want 2/2", r.Used, r.Limit)
	}
	if !r.ResetsAt.Equal(NextReset(noon)) {
		t.Errorf("resets at %s, want %s", r.ResetsAt, NextReset(noon))
	}
	if r.Message == "" {
		t.Error("the refusal has no message for the user")
	}
}

// The day boundary is 00:00 UTC. Advancing the injected clock past it must reopen the budget
// without any row being deleted: the window is a query, not a counter to reset.
func TestDailyLimitResetsAtUTCMidnight(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 1, MaxTurns: 100})

	f.mustSettle(t, f.mustReserve(t, "c1"), Outcome{Status: store.StatusOK})
	if _, err := f.reserve(t, "c1"); err == nil {
		t.Fatal("second generation in the same day was allowed")
	}

	// 23:59:59 on the same day: still blocked.
	f.clock.now = time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC)
	if _, err := f.reserve(t, "c1"); err == nil {
		t.Fatal("generation was allowed one second before the reset")
	}

	// One second later, the day boundary has moved: a new allowance.
	f.clock.now = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	f.mustReserve(t, "c1")
}

func TestPendingCountsAgainstTheDailyLimit(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 1, MaxTurns: 100})

	f.mustReserve(t, "c1") // never settled, as if the process is about to die

	// Not reaped yet (fresh), so the in-flight check fires first...
	r := refusal(t, func() error { _, err := f.reserve(t, "c2"); return err }())
	if r.Reason != ReasonDaily && r.Reason != ReasonInFlight {
		t.Fatalf("reason = %q, want the pending row to count", r.Reason)
	}

	// ...and once the reap makes it a failure, the daily count still includes it: a request
	// that burned tokens is spent budget, not a refund.
	f.clock.Advance(10 * time.Minute)
	r = refusal(t, func() error { _, err := f.reserve(t, "c2"); return err }())
	if r.Reason != ReasonDaily {
		t.Fatalf("reason = %q after the reap, want %q", r.Reason, ReasonDaily)
	}
}

func TestConversationTurnLimit(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 100, MaxTurns: 2})

	for range 2 {
		f.mustSettle(t, f.mustReserve(t, "c1"), Outcome{Status: store.StatusOK})
	}

	r := refusal(t, func() error { _, err := f.reserve(t, "c1"); return err }())
	if r.Reason != ReasonTurns {
		t.Fatalf("reason = %q, want %q", r.Reason, ReasonTurns)
	}
	if r.Limit != 2 {
		t.Errorf("limit = %d, want 2", r.Limit)
	}

	// A different conversation is a fresh budget. That is the escape hatch the message
	// points at, so it has to actually work.
	f.mustReserve(t, "c2")
}

func TestGlobalCostCeiling(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 100, MaxTurns: 100, DailyCostMicros: 1000})

	other, err := f.store.UpsertIdentity(context.Background(), store.Identity{Provider: "github", ProviderUserID: "u2", Email: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	// Spend the budget as a *different* user: the ceiling is install-wide, not per-user.
	res, err := f.quota.Reserve(context.Background(), Request{UserID: other.ID, ConversationID: "c", Model: "fake", PromptVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f.mustSettle(t, res, Outcome{Status: store.StatusOK, CostMicros: 1000})

	r := refusal(t, func() error { _, err := f.reserve(t, "c"); return err }())
	if r.Reason != ReasonGlobal {
		t.Fatalf("reason = %q, want %q", r.Reason, ReasonGlobal)
	}
	if r.Message == "" {
		t.Error("the global refusal has no message")
	}
}

func TestStalePendingIsReapedByTheNextReserve(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 100, MaxTurns: 100, StaleAfter: time.Minute})

	abandoned := f.mustReserve(t, "c1")
	_ = abandoned // the request died: no Settle is ever called

	// Still fresh: refused, and not reaped.
	if r := refusal(t, func() error { _, err := f.reserve(t, "c1"); return err }()); r.Reason != ReasonInFlight {
		t.Fatalf("fresh pending = %q, want in-flight", r.Reason)
	}

	f.clock.Advance(2 * time.Minute)
	res := f.mustReserve(t, "c1") // the reaper freed the slot first

	// The abandoned row is recorded as failed, not deleted: the ledger stays complete.
	stale, err := f.store.GenerationByID(context.Background(), abandoned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != store.StatusFailed {
		t.Fatalf("abandoned row status = %s, want failed", stale.Status)
	}
	f.mustSettle(t, res, Outcome{Status: store.StatusOK})
}

// A cancelled generation still consumes quota. Otherwise closing the tab and retrying would be
// an unlimited plan, which is the one thing the quota exists to prevent.
func TestCanceledGenerationsCountAgainstQuota(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 1, MaxTurns: 100})

	res := f.mustReserve(t, "c1")
	f.mustSettle(t, res, Outcome{Status: store.StatusCanceled, InputTokens: 10, OutputTokens: 5})

	r := refusal(t, func() error { _, err := f.reserve(t, "c2"); return err }())
	if r.Reason != ReasonDaily {
		t.Fatalf("reason = %q, want the canceled generation to count", r.Reason)
	}

	usage, err := f.quota.Today(context.Background(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Used != 1 || usage.Limit != 1 {
		t.Fatalf("usage = %+v, want 1/1", usage)
	}
}

// Concurrent reservations must produce exactly one winner, and the losers must be told why.
func TestConcurrentReserveExactlyOneWinner(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 10, MaxTurns: 10})

	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		refused int
		other   []error
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := f.reserve(t, "c")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrExceeded):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if wins != 1 {
		t.Fatalf("%d reservations succeeded, want exactly 1", wins)
	}
	if refused != writers-1 {
		t.Fatalf("%d refusals, want %d", refused, writers-1)
	}
}

func TestSettleRejectsAnInvalidStatus(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 1})
	res := f.mustReserve(t, "c")
	err := res.Settle(context.Background(), Outcome{Status: store.StatusPending})
	if err == nil {
		t.Fatal("settling as pending was accepted")
	}
	if want := "want ok, failed or canceled"; !contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q", err, want)
	}
}

// A settle must still be written when the request context has been canceled: that is exactly
// the client-disconnect path, and it is the one settle that matters most.
func TestSettleSurvivesACanceledRequestContext(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 10})

	res := f.mustReserve(t, "c")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := res.Settle(ctx, Outcome{Status: store.StatusCanceled, OutputTokens: 3}); err != nil {
		t.Fatalf("Settle with a canceled context: %v", err)
	}
	saved, err := f.store.GenerationByID(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != store.StatusCanceled {
		t.Fatalf("status = %s, want canceled", saved.Status)
	}
}

func TestReserveRequiresIdentity(t *testing.T) {
	f := newFixture(t, Limits{DailyGenerations: 10})
	if _, err := f.quota.Reserve(context.Background(), Request{ConversationID: "c"}); err == nil {
		t.Fatal("a reservation with no user was accepted")
	}
	if _, err := f.quota.Reserve(context.Background(), Request{UserID: f.user}); err == nil {
		t.Fatal("a reservation with no conversation was accepted")
	}
}

func TestDayStartAndNextReset(t *testing.T) {
	for _, tc := range []struct {
		in    time.Time
		day   string
		reset string
	}{
		{noon, "2026-03-10T00:00:00Z", "2026-03-11T00:00:00Z"},
		{time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), "2026-03-10T00:00:00Z", "2026-03-11T00:00:00Z"},
		{time.Date(2026, 3, 9, 23, 59, 59, 999000000, time.UTC), "2026-03-09T00:00:00Z", "2026-03-10T00:00:00Z"},
		// A non-UTC input is still resolved to the UTC day: the server's timezone must not
		// move anybody's reset.
		{time.Date(2026, 3, 10, 23, 30, 0, 0, time.FixedZone("CET", 3600)), "2026-03-10T00:00:00Z", "2026-03-11T00:00:00Z"},
	} {
		if got := DayStart(tc.in).Format(time.RFC3339); got != tc.day {
			t.Errorf("DayStart(%s) = %s, want %s", tc.in, got, tc.day)
		}
		if got := NextReset(tc.in).Format(time.RFC3339); got != tc.reset {
			t.Errorf("NextReset(%s) = %s, want %s", tc.in, got, tc.reset)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
