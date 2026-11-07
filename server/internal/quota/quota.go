// Package quota decides whether a user may start one more generation, and records what it
// cost. Every control in here is a spend control: the threat model is the owner's provider
// bill, not a distributed-systems exercise.
//
// The shape is reserve → call → settle. The reserved row is written *before* the upstream
// request, inside one BEGIN IMMEDIATE transaction that also reads the counters. That is what
// makes two concurrent requests unable to jointly overshoot the global ceiling: the second
// one either sees the first one's pending row in `sum(cost_micros)` terms or blocks on the
// write lock, never slips between the check and the insert.
package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kaki-studios/newton/server/internal/id"
	"github.com/kaki-studios/newton/server/internal/store"
)

// ErrInFlight means the user already has a request running. The database's partial unique
// index is the authority, so this holds across processes and across restarts.
var ErrInFlight = store.ErrInFlight

// Reason is why a reservation was refused. Handlers map it to a status code and a message;
// the message has to tell the user when they can try again, because "limit reached" alone is
// an unactionable dead end.
type Reason string

const (
	// ReasonDaily means the per-user daily generation cap is spent.
	ReasonDaily Reason = "daily_generations"
	// ReasonTurns means this conversation has used its repair budget.
	ReasonTurns Reason = "conversation_turns"
	// ReasonGlobal means the install-wide daily cost ceiling is spent.
	ReasonGlobal Reason = "daily_cost"
	// ReasonInFlight means one request is already running for this user.
	ReasonInFlight Reason = "in_flight"
)

// Refusal is returned (wrapped in ErrExceeded) when a reservation is denied. Handlers turn it
// into a 429 with a code and a message the user can act on.
type Refusal struct {
	Reason   Reason
	Limit    int64
	Used     int64
	ResetsAt time.Time
	// RetryAfter is how long the caller should wait, measured with the store's clock. It is
	// carried rather than recomputed by the handler because recomputing it against the wall
	// clock is exactly the kind of mixed-clock bug that produces a negative header, or a
	// header that says three months after a clock change.
	RetryAfter time.Duration
	Message    string
}

func (r *Refusal) Error() string {
	if r.Message != "" {
		return r.Message
	}
	return fmt.Sprintf("quota: %s reached (%d/%d), resets %s",
		r.Reason, r.Used, r.Limit, FormatReset(r.ResetsAt))
}

// FormatReset renders a reset time the way the UI shows it: "11 Mar 2026, 00:00 UTC". Explicit
// rather than localized, because the plan promises "resets 00:00 UTC" and a half-localized
// timestamp is worse than none.
func FormatReset(t time.Time) string {
	return t.UTC().Format("2 Jan 2006, 15:04") + " UTC"
}

// ErrExceeded is the sentinel handlers match on. errors.As gets the Refusal for detail.
var ErrExceeded = errors.New("quota exceeded")

// Limits is configuration, all of it optional in the sense that zero disables the check.
// Zero is a deliberate value: an operator who sets DAILY_GENERATIONS=0 means "no chat", not
// "unlimited", so handlers treat a zero limit as a hard denial and the kill switch is a
// separate, clearer flag.
type Limits struct {
	// DailyGenerations caps generations per user per UTC day.
	DailyGenerations int
	// MaxTurns caps generations per conversation, which is what actually bounds a repair
	// loop: a model that keeps failing can burn six turns in two minutes.
	MaxTurns int
	// DailyCostMicros caps the sum of cost_micros across all users per UTC day. Zero means
	// no global ceiling (the per-user cap still applies).
	DailyCostMicros int64
	// StaleAfter is how old a pending row must be before Reserve treats it as dead. It must
	// be comfortably longer than the slowest real generation, or a slow request gets reaped
	// while it is still streaming and the user can start a second one.
	StaleAfter time.Duration
}

// DefaultStaleAfter is the reaper cutoff. Three minutes is longer than any generation the
// 2000-token completion cap allows, and short enough that a crashed request does not lock a
// user out for a lesson.
const DefaultStaleAfter = 3 * time.Minute

// Manager enforces Limits against a store. It holds no mutable state: every decision is read
// from the database, so a restart or a second process does not reset anyone's quota.
type Manager struct {
	Store  *store.Store
	Limits Limits
	// NewID mints generation ids. Overridden in tests so the SSE `done` event is
	// byte-for-byte reproducible against testdata/sse fixtures.
	NewID id.Newer
}

// Usage is what a user has spent today, for /api/me and for refusal messages.
type Usage struct {
	Used     int
	Limit    int
	ResetsAt time.Time
}

// Reservation is one authorized generation. Settle must be called exactly once, from a defer:
// leaving it pending blocks the user until the reaper notices.
type Reservation struct {
	ID             string
	UserID         string
	ConversationID string
	Model          string
	PromptVersion  string
	// ReservedAt is the row's created_at, so a settle does not need a second clock read.
	ReservedAt time.Time
	manager    *Manager
}

// DayStart is the UTC midnight that begins the quota day containing t. UTC, not local time:
// the server's timezone must not silently move a user's reset, and the plan documents 00:00
// UTC to the user.
func DayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// NextReset is when the current quota day ends.
func NextReset(t time.Time) time.Time { return DayStart(t).Add(24 * time.Hour) }

// staledAfter is the honest Retry-After for a concurrent request: the run that is in the way
// will be reaped after StaleAfter at the latest, so that is the worst case the client has to
// wait.
func staledAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultStaleAfter
	}
	return d
}

// Request is everything Reserve needs from the caller.
type Request struct {
	UserID         string
	ConversationID string
	Model          string
	PromptVersion  string
}

// Reserve atomically checks every limit and inserts the pending row that authorizes the call.
//
// The order of checks is the order in which a user should be told what is wrong: in-flight
// first (the most immediate and most easily fixed), then the conversation cap (the user can
// start a new conversation), then the daily cap, then the global ceiling.
func (m *Manager) Reserve(ctx context.Context, req Request) (Reservation, error) {
	if req.UserID == "" {
		return Reservation{}, errors.New("quota: a reservation needs a user id")
	}
	if req.ConversationID == "" {
		return Reservation{}, errors.New("quota: a reservation needs a conversation id: turns are counted per conversation")
	}
	// Do not assign to m.NewID: Manager is shared by every concurrent request, and a lazy
	// fallback write here is a data race. Reading it into a local is enough.
	newID := m.NewID
	if newID == nil {
		newID = id.New
	}

	now := m.Store.Now()
	dayStart := DayStart(now)
	resetsAt := NextReset(now)
	res := Reservation{
		ID:             newID(),
		UserID:         req.UserID,
		ConversationID: req.ConversationID,
		Model:          req.Model,
		PromptVersion:  req.PromptVersion,
		ReservedAt:     now,
		manager:        m,
	}

	err := m.Store.WithTx(ctx, func(tx *store.Tx) error {
		// Reap inside the transaction: another request that died mid-flight must not block
		// this one, and doing it outside the transaction would race a concurrent reserve.
		if m.Limits.StaleAfter > 0 {
			if _, err := tx.ReapStalePending(ctx, req.UserID, now.Add(-m.Limits.StaleAfter)); err != nil {
				return err
			}
		}

		if m.Limits.MaxTurns > 0 {
			turns, err := tx.ConversationTurns(ctx, req.UserID, req.ConversationID)
			if err != nil {
				return err
			}
			if turns >= m.Limits.MaxTurns {
				return &Refusal{
					Reason:     ReasonTurns,
					Limit:      int64(m.Limits.MaxTurns),
					Used:       int64(turns),
					ResetsAt:   resetsAt,
					RetryAfter: resetsAt.Sub(now),
					Message: fmt.Sprintf(
						"This conversation has used its %d attempts. Start a new conversation to keep going.",
						m.Limits.MaxTurns),
				}
			}
		}

		if m.Limits.DailyGenerations > 0 {
			usage, err := tx.UserUsageSince(ctx, req.UserID, dayStart)
			if err != nil {
				return err
			}
			if usage.Count >= m.Limits.DailyGenerations {
				return &Refusal{
					Reason:     ReasonDaily,
					Limit:      int64(m.Limits.DailyGenerations),
					Used:       int64(usage.Count),
					ResetsAt:   resetsAt,
					RetryAfter: resetsAt.Sub(now),
					Message: fmt.Sprintf("Daily limit reached (%d). Resets %s.",
						m.Limits.DailyGenerations, FormatReset(resetsAt)),
				}
			}
		}

		if m.Limits.DailyCostMicros > 0 {
			global, err := tx.GlobalUsageSince(ctx, dayStart)
			if err != nil {
				return err
			}
			if global.CostMicros >= m.Limits.DailyCostMicros {
				return &Refusal{
					Reason:     ReasonGlobal,
					Limit:      m.Limits.DailyCostMicros,
					Used:       global.CostMicros,
					ResetsAt:   resetsAt,
					RetryAfter: resetsAt.Sub(now),
					Message:    "The AI budget for today is spent. It resets at 00:00 UTC; the simulator still works.",
				}
			}
		}

		// Last, and deliberately inside the same transaction: the INSERT is what makes the
		// decision final, and the partial unique index is what makes it correct.
		err := tx.InsertGeneration(ctx, store.Generation{
			ID:             res.ID,
			UserID:         res.UserID,
			ConversationID: res.ConversationID,
			Status:         store.StatusPending,
			Model:          res.Model,
			PromptVersion:  res.PromptVersion,
			CreatedAt:      now,
		})
		if errors.Is(err, store.ErrInFlight) {
			return &Refusal{
				Reason:     ReasonInFlight,
				ResetsAt:   resetsAt,
				RetryAfter: staledAfter(m.Limits.StaleAfter),
				Message:    "A reply is already being generated. Wait for it to finish.",
			}
		}
		return err
	})
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			// Both %w: callers match ErrExceeded with errors.Is and pull the Refusal out
			// with errors.As. Losing either one would force every handler to re-inspect a
			// string.
			return Reservation{}, fmt.Errorf("%w: %w", ErrExceeded, refusal)
		}
		return Reservation{}, err
	}
	return res, nil
}

// Outcome is how a generation ended.
type Outcome struct {
	// Status is ok, failed or canceled.
	Status store.Status
	// InputTokens, OutputTokens and CostMicros are whatever the provider reported. A
	// cancelled generation records the tokens actually consumed.
	InputTokens  int
	OutputTokens int
	CostMicros   int64
	// SceneValid is the browser's verdict, nil when it did not report. Advisory only.
	SceneValid *bool
}

// Settle records the outcome and frees the pending slot.
//
// A cancelled generation still counts against quota, on purpose. Settling it as `canceled`
// (rather than deleting the row or marking it failed) keeps it inside every `count(*)` and
// `sum(cost_micros)` window, so closing the tab and retrying is not a free unlimited plan.
// The tokens it burned are already spent; the row is the receipt.
//
// Settle logs and swallows its own error at the call site instead of returning it, because by
// the time it runs the response has already been streamed: there is nobody left to tell.
func (r Reservation) Settle(ctx context.Context, out Outcome) error {
	if r.manager == nil {
		return errors.New("quota: this reservation was not created by Reserve")
	}
	status := out.Status
	switch status {
	case store.StatusOK, store.StatusFailed, store.StatusCanceled:
	default:
		return fmt.Errorf("quota: cannot settle a reservation as %q: want ok, failed or canceled", status)
	}

	// Settle gets its own short deadline: the request context is canceled exactly when the
	// client disconnects, which is the moment a cancelled settle has to be written. Using
	// it would guarantee the write fails.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	return r.manager.Store.Settle(settleCtx, store.Generation{
		ID:           r.ID,
		Status:       status,
		InputTokens:  out.InputTokens,
		OutputTokens: out.OutputTokens,
		CostMicros:   out.CostMicros,
		SceneValid:   out.SceneValid,
	})
}

// Today reports a user's usage for /api/me and for the composer's status line.
func (m *Manager) Today(ctx context.Context, userID string) (Usage, error) {
	now := m.Store.Now()
	usage, err := m.Store.UserUsageSince(ctx, userID, DayStart(now))
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		Used:     usage.Count,
		Limit:    m.Limits.DailyGenerations,
		ResetsAt: NextReset(now),
	}, nil
}
