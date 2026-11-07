package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Sessions are opaque random tokens in a cookie. Only their sha256 is stored, so a database
// dump (or a Litestream replica in a bucket) contains nothing that can be replayed as a
// login. Revocation is a DELETE, and a redeploy does not sign anyone out.

// CreateSession stores a new session for userID and returns its expiry. token is the raw
// cookie value; the caller has already generated it.
//
// Pre-login sessions are rotated by the caller (delete the old row, insert a new one), which
// is what stops a fixed pre-auth session from being a foothold after login.
func (s *Store) CreateSession(ctx context.Context, userID, token string, ttl time.Duration) (time.Time, error) {
	expiry := s.now().Add(ttl)
	if _, err := s.db.ExecContext(ctx,
		`insert into sessions (token_hash, user_id, expiry, created_at) values (?, ?, ?, ?)`,
		HashToken(token), userID, FormatTime(expiry), FormatTime(s.now())); err != nil {
		return time.Time{}, fmt.Errorf("store: creating session: %w", err)
	}
	return expiry, nil
}

// Session is what a cookie value resolves to: the user, and whether this read extended the
// session's life. Extended is what lets the caller refresh the cookie only when it changed,
// instead of on every request.
type Session struct {
	User     User
	Expiry   time.Time
	Extended bool
}

// SessionUser resolves a cookie value to its user. It also deletes the row when it finds an
// expired session, so expiry is enforced on read without a background sweeper being required
// for correctness (one still runs, to keep the table from growing).
//
// ttl is the sliding-expiry window: when the session is more than halfway to its expiry, the
// expiry moves out to now+ttl, so an active user is not logged out mid-lesson. A zero ttl
// disables sliding.
func (s *Store) SessionUser(ctx context.Context, token string, ttl time.Duration) (Session, error) {
	hash := HashToken(token)
	var (
		userID string
		expiry string
	)
	err := s.db.QueryRowContext(ctx,
		`select user_id, expiry from sessions where token_hash = ?`, hash).Scan(&userID, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: loading session: %w", err)
	}

	expiresAt, err := ParseTime(expiry)
	if err != nil {
		return Session{}, err
	}
	now := s.now()
	if !expiresAt.After(now) {
		// Expired: delete it and report a miss. Deleting here is what makes the read path
		// self-healing; the sweeper is only housekeeping.
		_ = s.DeleteSession(ctx, token)
		return Session{}, ErrNotFound
	}

	user, err := userByID(ctx, s.db, userID)
	if errors.Is(err, ErrNotFound) {
		// The user row is gone but the FK cascade did not fire (only possible if foreign
		// keys were off at some point). Treat it as a miss rather than a 500.
		_ = s.DeleteSession(ctx, token)
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}

	out := Session{User: user, Expiry: expiresAt}
	if ttl > 0 && expiresAt.Sub(now) < ttl/2 {
		out.Expiry = now.Add(ttl)
		out.Extended = true
		if _, err := s.db.ExecContext(ctx,
			`update sessions set expiry = ? where token_hash = ?`, FormatTime(out.Expiry), hash); err != nil {
			return Session{}, fmt.Errorf("store: extending session: %w", err)
		}
	}
	return out, nil
}

// DeleteSession revokes one session. Deleting a row that is already gone is not an error:
// logout has to be idempotent, because the cookie may outlive the row.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx,
		`delete from sessions where token_hash = ?`, HashToken(token)); err != nil {
		return fmt.Errorf("store: deleting session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions is the housekeeping sweeper, run from a ticker in cmd/api.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`delete from sessions where expiry <= ?`, FormatTime(s.now()))
	if err != nil {
		return 0, fmt.Errorf("store: sweeping expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// RowsAffected is not essential; the sweep succeeded.
		return 0, nil
	}
	return n, nil
}

// HashToken is the only derivation applied to a cookie value before storage. sha256 is
// enough: the input is 256 bits of crypto/rand, so there is nothing to brute-force, and the
// lookup is an indexed equality match with no timing signal worth defending against.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
