package store

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "session@example.com")

	const token = "raw-cookie-value"
	expiry, err := s.CreateSession(ctx, u.ID, token, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !expiry.Equal(testDay.Add(30 * 24 * time.Hour)) {
		t.Errorf("expiry = %s, want the injected clock plus the ttl", expiry)
	}

	sess, err := s.SessionUser(ctx, token, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("SessionUser: %v", err)
	}
	if sess.User.ID != u.ID {
		t.Errorf("session resolved to %s, want %s", sess.User.ID, u.ID)
	}
	if !sess.Expiry.Equal(expiry) {
		t.Errorf("expiry on read = %s, want %s", sess.Expiry, expiry)
	}
	if sess.Extended {
		t.Error("a session used immediately after login was extended")
	}
}

// The database must not contain the cookie value: a dump or a replicated copy is then useless.
func TestSessionStoresOnlyTheHash(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "hash@example.com")

	const token = "super-secret-cookie"
	if _, err := s.CreateSession(ctx, u.ID, token, time.Hour); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := s.DB().QueryRowContext(ctx, `select token_hash from sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(token)) {
		t.Fatal("the raw session token is stored in the database")
	}
	if !bytes.Equal(stored, HashToken(token)) {
		t.Fatalf("stored hash = %x, want sha256 of the token", stored)
	}
}

func TestUnknownSessionIsAMiss(t *testing.T) {
	s, _ := openTest(t)
	_, err := s.SessionUser(context.Background(), "never issued", time.Hour)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionUser = %v, want ErrNotFound", err)
	}
}

func TestExpiredSessionIsDeletedOnRead(t *testing.T) {
	s, clock := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "expired@example.com")

	if _, err := s.CreateSession(ctx, u.ID, "t", time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)

	if _, err := s.SessionUser(ctx, "t", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session = %v, want ErrNotFound", err)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx, `select count(*) from sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d expired session rows remain; the read path should have deleted the row", n)
	}
}

func TestSessionSlidesOnlyAfterHalfway(t *testing.T) {
	s, clock := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "slide@example.com")
	ttl := 10 * time.Hour

	created, err := s.CreateSession(ctx, u.ID, "t", ttl)
	if err != nil {
		t.Fatal(err)
	}

	// Early use: expiry unchanged, or a session used once a minute would never expire.
	clock.Advance(time.Hour)
	if sess, err := s.SessionUser(ctx, "t", ttl); err != nil {
		t.Fatal(err)
	} else if !sess.Expiry.Equal(created) {
		t.Fatalf("expiry moved early: %s, want %s", sess.Expiry, created)
	} else if sess.Extended {
		t.Fatal("session was marked extended before the halfway point")
	}

	// Past halfway: expiry extends from now.
	clock.Advance(6 * time.Hour)
	sess, err := s.SessionUser(ctx, "t", ttl)
	if err != nil {
		t.Fatal(err)
	}
	want := clock.Now().Add(ttl)
	if !sess.Expiry.Equal(want) {
		t.Fatalf("expiry = %s, want %s (sliding)", sess.Expiry, want)
	}
	if !sess.Extended {
		t.Error("the session moved but was not reported as extended")
	}
}

func TestLogoutDeletesTheSession(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "logout@example.com")
	if _, err := s.CreateSession(ctx, u.ID, "t", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(ctx, "t"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, "t", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived logout: %v", err)
	}
	// Logging out twice is not an error: the cookie may outlive the row.
	if err := s.DeleteSession(ctx, "t"); err != nil {
		t.Fatalf("second DeleteSession: %v", err)
	}
}

func TestDeletingAUserCascadesToSessionsAndIdentities(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "cascade@example.com")
	if _, err := s.CreateSession(ctx, u.ID, "t", time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().ExecContext(ctx, `delete from users where id = ?`, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	for _, table := range []string{"sessions", "identities"} {
		var n int
		if err := s.DB().QueryRowContext(ctx, `select count(*) from `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left in %s after deleting the user: foreign keys are off", n, table)
		}
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s, clock := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "sweep@example.com")

	if _, err := s.CreateSession(ctx, u.ID, "short", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, u.ID, "long", 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)

	n, err := s.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d sessions, want 1", n)
	}
	if _, err := s.SessionUser(ctx, "long", 100*time.Hour); err != nil {
		t.Fatalf("the live session was swept: %v", err)
	}
}

// The partial unique index is the concurrency control for the whole chat endpoint: with 8
// writers racing, exactly one must win and the others must see ErrInFlight. Testing with
// goroutines against one *sql.DB, with SetMaxOpenConns(1), is exactly the production shape.
func TestConcurrentReservesOneWinner(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "race@example.com")

	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		inFligt int
		other   []error
	)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together to maximize overlap
			err := s.InsertGeneration(ctx, Generation{
				ID:             fmtID(i),
				UserID:         u.ID,
				ConversationID: "c1",
				Status:         StatusPending,
				Model:          "fake",
				PromptVersion:  "test",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrInFlight):
				inFligt++
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
		t.Fatalf("%d writers won, want exactly 1", wins)
	}
	if inFligt != writers-1 {
		t.Fatalf("%d writers saw ErrInFlight, want %d", inFligt, writers-1)
	}
}

func TestSettlingFreesThePendingSlot(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "settle@example.com")

	g := Generation{ID: "g1", UserID: u.ID, ConversationID: "c", Status: StatusPending, Model: "fake", PromptVersion: "v"}
	if err := s.InsertGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertGeneration(ctx, Generation{ID: "g2", UserID: u.ID, ConversationID: "c", Status: StatusPending, Model: "fake", PromptVersion: "v"}); !errors.Is(err, ErrInFlight) {
		t.Fatalf("second pending = %v, want ErrInFlight", err)
	}

	g.Status = StatusOK
	g.InputTokens, g.OutputTokens, g.CostMicros = 100, 50, 7
	if err := s.Settle(ctx, g); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := s.InsertGeneration(ctx, Generation{ID: "g3", UserID: u.ID, ConversationID: "c", Status: StatusPending, Model: "fake", PromptVersion: "v"}); err != nil {
		t.Fatalf("inserting after settle: %v", err)
	}

	stored, err := s.GenerationByID(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusOK || stored.InputTokens != 100 || stored.OutputTokens != 50 || stored.CostMicros != 7 {
		t.Fatalf("settled row = %+v, want ok with 100/50/7", stored)
	}
}

func TestReapStalePendingFreesAnAbandonedRequest(t *testing.T) {
	s, clock := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "stale@example.com")

	g := Generation{ID: "stale", UserID: u.ID, ConversationID: "c", Status: StatusPending, Model: "fake", PromptVersion: "v"}
	if err := s.InsertGeneration(ctx, g); err != nil {
		t.Fatal(err)
	}

	// Not yet old enough: the reaper must not touch a live request.
	if n, err := s.ReapStalePending(ctx, u.ID, clock.Now().Add(-3*time.Minute)); err != nil || n != 0 {
		t.Fatalf("early reap = %d, %v; want 0, nil", n, err)
	}

	clock.Advance(4 * time.Minute)
	n, err := s.ReapStalePending(ctx, u.ID, clock.Now().Add(-3*time.Minute))
	if err != nil {
		t.Fatalf("ReapStalePending: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d rows, want 1", n)
	}

	if err := s.InsertGeneration(ctx, Generation{ID: "fresh", UserID: u.ID, ConversationID: "c", Status: StatusPending, Model: "fake", PromptVersion: "v"}); err != nil {
		t.Fatalf("the slot is still blocked after reaping: %v", err)
	}

	reaped, err := s.GenerationByID(ctx, "stale")
	if err != nil {
		t.Fatal(err)
	}
	if reaped.Status != StatusFailed {
		t.Fatalf("reaped row status = %s, want failed", reaped.Status)
	}
}

func TestUsageAggregates(t *testing.T) {
	s, clock := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "usage@example.com")
	yesterday := clock.Now().Add(-24 * time.Hour)

	// One settled row today, one yesterday: only today's counts.
	if err := s.InsertGeneration(ctx, Generation{ID: "old", UserID: u.ID, ConversationID: "c", Status: StatusOK, Model: "fake", PromptVersion: "v", CreatedAt: yesterday}); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Generation{ID: "old", Status: StatusOK, CostMicros: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertGeneration(ctx, Generation{ID: "new", UserID: u.ID, ConversationID: "c", Status: StatusOK, Model: "fake", PromptVersion: "v", CreatedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, Generation{ID: "new", Status: StatusOK, CostMicros: 3}); err != nil {
		t.Fatal(err)
	}

	startOfDay := time.Date(testDay.Year(), testDay.Month(), testDay.Day(), 0, 0, 0, 0, time.UTC)
	usage, err := s.UserUsageSince(ctx, u.ID, startOfDay)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Count != 1 || usage.CostMicros != 3 {
		t.Fatalf("usage = %+v, want {1, 3}", usage)
	}

	global, err := s.GlobalUsageSince(ctx, startOfDay)
	if err != nil {
		t.Fatal(err)
	}
	if global.Count != 1 || global.CostMicros != 3 {
		t.Fatalf("global usage = %+v, want {1, 3}", global)
	}
}

func TestConversationTurnsCountsPerConversation(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	u := newUser(t, s, "turns@example.com")

	for i := range 3 {
		if err := s.InsertGeneration(ctx, Generation{
			ID: fmtID(i), UserID: u.ID, ConversationID: "chat-a", Status: StatusOK, Model: "fake", PromptVersion: "v",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Another user's rows must not count toward this conversation.
	other := newUser(t, s, "other@example.com")
	if err := s.InsertGeneration(ctx, Generation{
		ID: "other-1", UserID: other.ID, ConversationID: "chat-a", Status: StatusOK, Model: "fake", PromptVersion: "v",
	}); err != nil {
		t.Fatal(err)
	}

	turns, err := s.ConversationTurns(ctx, u.ID, "chat-a")
	if err != nil {
		t.Fatal(err)
	}
	if turns != 3 {
		t.Fatalf("turns = %d, want 3", turns)
	}
	if turns, err := s.ConversationTurns(ctx, u.ID, "chat-b"); err != nil || turns != 0 {
		t.Fatalf("unknown conversation = %d, %v; want 0, nil", turns, err)
	}
}

func TestSettlingUnknownGenerationIsReported(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Settle(context.Background(), Generation{ID: "missing", Status: StatusOK}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Settle = %v, want ErrNotFound", err)
	}
}

func fmtID(i int) string {
	return "g-" + string(rune('a'+i))
}
