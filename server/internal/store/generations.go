package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Status is a generation's lifecycle. The set is enforced by a CHECK constraint in the
// schema, so an invalid status fails at the database rather than in a log line nobody reads.
type Status string

const (
	// StatusPending is written *before* the upstream request. The partial unique index
	// `one_pending` makes a second one per user impossible.
	StatusPending Status = "pending"
	// StatusOK means the model finished; usage is recorded.
	StatusOK Status = "ok"
	// StatusFailed means the provider (or the network) returned an error.
	StatusFailed Status = "failed"
	// StatusCanceled means the client went away. It still counts against quota: otherwise
	// abort-and-retry would be an unlimited plan.
	StatusCanceled Status = "canceled"
)

// Generation is one row of the audit/quota log.
type Generation struct {
	ID             string
	UserID         string
	ConversationID string
	Status         Status
	Model          string
	PromptVersion  string
	InputTokens    int
	OutputTokens   int
	CostMicros     int64
	// SceneValid is nil when the browser did not report, 0 for reported invalid, 1 for valid.
	SceneValid *bool
	CreatedAt  time.Time
}

// Tx is a transaction-scoped view of the same queries. `internal/quota` uses it to read the
// counters and insert the reservation in one BEGIN IMMEDIATE transaction, so the decision and
// the row it authorizes cannot be interleaved with another request.
//
// The policy stays in quota; this type only knows how to ask the database questions.
type Tx struct {
	q   queryer
	now func() time.Time
}

// WithTx runs fn inside one transaction. It is the only exported way to group writes, which
// keeps `BEGIN`/`COMMIT` out of every caller.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return fn(&Tx{q: tx, now: s.now})
	})
}

// InsertGeneration reserves a slot. It returns ErrInFlight when the user already has a
// pending row.
//
// It deliberately does NOT reap stale rows: quota.Reserve does that first, in the same
// transaction, so the reap cannot race a concurrent reserve. Keeping the two separate means
// this function stays a single INSERT and its failure mode is one sentinel.
func (s *Store) InsertGeneration(ctx context.Context, g Generation) error {
	return insertGeneration(ctx, s.db, s.now, g)
}

// InsertGeneration is the transactional form of Store.InsertGeneration.
func (t *Tx) InsertGeneration(ctx context.Context, g Generation) error {
	return insertGeneration(ctx, t.q, t.now, g)
}

// ReapStalePending is the transactional form of Store.ReapStalePending.
func (t *Tx) ReapStalePending(ctx context.Context, userID string, cutoff time.Time) (int64, error) {
	return reapStalePending(ctx, t.q, userID, cutoff)
}

// UserUsageSince is the transactional form of Store.UserUsageSince.
func (t *Tx) UserUsageSince(ctx context.Context, userID string, since time.Time) (UsageSince, error) {
	return userUsageSince(ctx, t.q, userID, since)
}

// GlobalUsageSince is the transactional form of Store.GlobalUsageSince.
func (t *Tx) GlobalUsageSince(ctx context.Context, since time.Time) (UsageSince, error) {
	return globalUsageSince(ctx, t.q, since)
}

// ConversationTurns is the transactional form of Store.ConversationTurns.
func (t *Tx) ConversationTurns(ctx context.Context, userID, conversationID string) (int, error) {
	return conversationTurns(ctx, t.q, userID, conversationID)
}

func insertGeneration(ctx context.Context, q queryer, now func() time.Time, g Generation) error {
	createdAt := g.CreatedAt
	if createdAt.IsZero() {
		createdAt = now()
	}
	_, err := q.ExecContext(ctx,
		`insert into generations
		   (id, user_id, conversation_id, status, model, prompt_version, scene_valid, created_at)
		 values (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.UserID, g.ConversationID, string(g.Status), g.Model, g.PromptVersion,
		boolToNullInt(g.SceneValid), FormatTime(createdAt))
	if err != nil {
		return classify(err, ErrInFlight)
	}
	return nil
}

// Settle records how a generation ended. It is idempotent in the sense that matters: settling
// a row that is no longer pending still writes the final numbers, because the token counts are
// a fact and losing them corrupts the cost ledger.
func (s *Store) Settle(ctx context.Context, g Generation) error {
	res, err := s.db.ExecContext(ctx,
		`update generations set status = ?, input_tokens = ?, output_tokens = ?,
		        cost_micros = ?, scene_valid = coalesce(?, scene_valid)
		 where id = ?`,
		string(g.Status), g.InputTokens, g.OutputTokens, g.CostMicros,
		boolToNullInt(g.SceneValid), g.ID)
	if err != nil {
		return fmt.Errorf("store: settling generation %s: %w", g.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("store: settling generation %s: %w", g.ID, ErrNotFound)
	}
	return nil
}

// ReapStalePending marks pending rows older than cutoff as failed, so a process that died
// mid-request cannot lock its user out of chat forever. Returns how many rows it freed.
func (s *Store) ReapStalePending(ctx context.Context, userID string, cutoff time.Time) (int64, error) {
	return reapStalePending(ctx, s.db, userID, cutoff)
}

func reapStalePending(ctx context.Context, q queryer, userID string, cutoff time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		`update generations set status = 'failed'
		  where user_id = ? and status = 'pending' and created_at < ?`,
		userID, FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: reaping stale pending generations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the update happened; the count is a log detail
	}
	return n, nil
}

// UsageSince is the aggregate the quota checks need: how many generations a user (or the
// whole install) has started since a boundary, and what they cost.
type UsageSince struct {
	Count      int
	CostMicros int64
}

// UserUsageSince counts a user's *settled or pending* generations created at or after since.
// Pending rows count: the slot is already spent, and a user cannot dodge a daily cap by
// keeping requests in flight.
func (s *Store) UserUsageSince(ctx context.Context, userID string, since time.Time) (UsageSince, error) {
	return userUsageSince(ctx, s.db, userID, since)
}

func userUsageSince(ctx context.Context, q queryer, userID string, since time.Time) (UsageSince, error) {
	var u UsageSince
	err := q.QueryRowContext(ctx,
		`select count(*), coalesce(sum(cost_micros), 0)
		   from generations where user_id = ? and created_at >= ?`,
		userID, FormatTime(since)).Scan(&u.Count, &u.CostMicros)
	if err != nil {
		return UsageSince{}, fmt.Errorf("store: user usage since %s: %w", FormatTime(since), err)
	}
	return u, nil
}

// GlobalUsageSince is the same aggregate across every user, for the install-wide ceiling.
func (s *Store) GlobalUsageSince(ctx context.Context, since time.Time) (UsageSince, error) {
	return globalUsageSince(ctx, s.db, since)
}

func globalUsageSince(ctx context.Context, q queryer, since time.Time) (UsageSince, error) {
	var u UsageSince
	err := q.QueryRowContext(ctx,
		`select count(*), coalesce(sum(cost_micros), 0) from generations where created_at >= ?`,
		FormatTime(since)).Scan(&u.Count, &u.CostMicros)
	if err != nil {
		return UsageSince{}, fmt.Errorf("store: global usage since %s: %w", FormatTime(since), err)
	}
	return u, nil
}

// ConversationTurns counts how many generations belong to one conversation. The per-
// conversation cap is what bounds a repair loop, which is the failure mode a daily cap alone
// does not catch: six repairs in two minutes are still only six of ten daily generations.
func (s *Store) ConversationTurns(ctx context.Context, userID, conversationID string) (int, error) {
	return conversationTurns(ctx, s.db, userID, conversationID)
}

func conversationTurns(ctx context.Context, q queryer, userID, conversationID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`select count(*) from generations where user_id = ? and conversation_id = ?`,
		userID, conversationID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting turns in conversation %s: %w", conversationID, err)
	}
	return n, nil
}

// PendingCount reports how many generations the user has in flight (0 or 1 by construction).
// Used by /api/me so the UI can show "generating…" after a page reload.
func (s *Store) PendingCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`select count(*) from generations where user_id = ? and status = 'pending'`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting pending generations: %w", err)
	}
	return n, nil
}

// GenerationByID loads one row, for tests and for a future /api/generations/:id.
func (s *Store) GenerationByID(ctx context.Context, id string) (Generation, error) {
	var (
		g                    Generation
		status, createdAt    string
		input, output, valid sql.NullInt64
		cost                 sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`select id, user_id, conversation_id, status, model, prompt_version,
		        input_tokens, output_tokens, cost_micros, scene_valid, created_at
		   from generations where id = ?`, id).
		Scan(&g.ID, &g.UserID, &g.ConversationID, &status, &g.Model, &g.PromptVersion,
			&input, &output, &cost, &valid, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, ErrNotFound
	}
	if err != nil {
		return Generation{}, fmt.Errorf("store: loading generation %s: %w", id, err)
	}
	g.Status = Status(status)
	g.InputTokens = int(input.Int64)
	g.OutputTokens = int(output.Int64)
	g.CostMicros = cost.Int64
	if valid.Valid {
		b := valid.Int64 != 0
		g.SceneValid = &b
	}
	if g.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Generation{}, err
	}
	return g, nil
}

func boolToNullInt(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}
