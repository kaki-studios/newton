package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kaki-studios/newton/server/internal/id"
)

// User is the account a session resolves to.
type User struct {
	ID          string
	Email       string // empty when the provider did not supply a verified one
	DisplayName string
	CreatedAt   time.Time
	Disabled    bool
}

// Identity is what an OAuth provider knows the user as. One user may have several (GitHub
// and Google), which is why it is a separate table keyed by (provider, providerUserID).
type Identity struct {
	Provider       string
	ProviderUserID string
	Email          string // the provider's *verified* email, or empty
	DisplayName    string
}

// UpsertIdentity resolves an OAuth login to a user id, creating or linking as needed.
//
// The order matters and is the whole reason this lives in one transaction:
//
//  1. If (provider, provider_user_id) is already known, that is the answer. A returning user
//     keeps their account even if they changed their email at the provider.
//  2. Otherwise, if a user row already exists with the same verified email, attach the new
//     identity to it. This is what stops one person signing in with GitHub and Google from
//     ending up with two accounts and two quotas.
//  3. Otherwise, create the user.
//
// Step 2 only ever runs on a *verified* email (gothic's GitHub provider refuses to return an
// unverified primary address), so an unverified address cannot take over an account. Doing
// step 1 first matters just as much: linking by email before checking the provider id would
// silently move an existing login to whoever currently controls that address.
func (s *Store) UpsertIdentity(ctx context.Context, ident Identity) (User, error) {
	if ident.Provider == "" || ident.ProviderUserID == "" {
		return User{}, errors.New("store: an identity needs both provider and provider user id")
	}
	email := TrimEmail(ident.Email)

	var user User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		userID, err := identityUser(ctx, tx, ident.Provider, ident.ProviderUserID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			u, err := userByID(ctx, tx, userID)
			if err != nil {
				return err
			}
			// Refresh the display name: providers are the source of truth for it.
			if ident.DisplayName != "" && ident.DisplayName != u.DisplayName {
				if _, err := tx.ExecContext(ctx,
					`update users set display_name = ? where id = ?`, ident.DisplayName, u.ID); err != nil {
					return fmt.Errorf("store: updating display name: %w", err)
				}
				u.DisplayName = ident.DisplayName
			}
			user = u
			return nil
		}

		if email != "" {
			existing, err := userByEmail(ctx, tx, email)
			switch {
			case err == nil:
				userID = existing.ID
			case errors.Is(err, ErrNotFound):
				userID = ""
			default:
				return err
			}
		}

		if userID == "" {
			userID = id.New()
			now := FormatTime(s.now())
			if _, err := tx.ExecContext(ctx,
				`insert into users (id, email, display_name, created_at) values (?, ?, ?, ?)`,
				userID, nullString(email), nullString(ident.DisplayName), now); err != nil {
				return classify(err, ErrConflict)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`insert into identities (provider, provider_user_id, user_id) values (?, ?, ?)`,
			ident.Provider, ident.ProviderUserID, userID); err != nil {
			return classify(err, ErrConflict)
		}

		u, err := userByID(ctx, tx, userID)
		if err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// UserByID looks a user up by primary key.
func (s *Store) UserByID(ctx context.Context, userID string) (User, error) {
	return userByID(ctx, s.db, userID)
}

// UserByEmail looks a user up by their verified address, which is what an operator actually knows
// about a person they want to ban. Case-folded, the same rule the identity linking uses.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	email = TrimEmail(email)
	if email == "" {
		return User{}, errors.New("store: UserByEmail needs an address")
	}
	return userByEmail(ctx, s.db, email)
}

// Session is the subset of a session row an operator wants to see. Token hashes stay in the store.
type SessionInfo struct {
	UserID    string
	Expiry    time.Time
	CreatedAt time.Time
}

// SessionsForUser lists a user's live sessions, for `newton -admin sessions <email>`.
func (s *Store) SessionsForUser(ctx context.Context, userID string) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`select user_id, expiry, created_at from sessions where user_id = ? order by created_at desc`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing sessions for %s: %w", userID, err)
	}
	defer rows.Close()

	var out []SessionInfo
	for rows.Next() {
		var (
			info            SessionInfo
			expiry, created string
		)
		if err := rows.Scan(&info.UserID, &expiry, &created); err != nil {
			return nil, fmt.Errorf("store: scanning a session row: %w", err)
		}
		if info.Expiry, err = ParseTime(expiry); err != nil {
			return nil, err
		}
		if info.CreatedAt, err = ParseTime(created); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

// SetDisabled bans or unbans a user, revoking every session in the same transaction.
//
// Two reads of "a ban takes effect now" matter here, and only doing both makes them true: the
// `disabled_at` write is what refuses the *next* login, and deleting the session rows is what ends
// the *current* ones. Leaving the rows would also mean un-banning silently resurrects sessions
// nobody can see.
func (s *Store) SetDisabled(ctx context.Context, userID string, disabled bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var ts any
		if disabled {
			ts = FormatTime(s.now())
		}
		res, err := tx.ExecContext(ctx, `update users set disabled_at = ? where id = ?`, ts, userID)
		if err != nil {
			return fmt.Errorf("store: setting disabled_at for user %s: %w", userID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return ErrNotFound
		}
		if disabled {
			if _, err := tx.ExecContext(ctx, `delete from sessions where user_id = ?`, userID); err != nil {
				return fmt.Errorf("store: revoking sessions for banned user %s: %w", userID, err)
			}
		}
		return nil
	})
}

// queryer is the subset of *sql.DB and *sql.Tx these helpers need, so the same query runs
// inside a transaction and outside one.
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func identityUser(ctx context.Context, q queryer, provider, providerUserID string) (string, error) {
	var userID string
	err := q.QueryRowContext(ctx,
		`select user_id from identities where provider = ? and provider_user_id = ?`,
		provider, providerUserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: looking up identity %s/%s: %w", provider, providerUserID, err)
	}
	return userID, nil
}

func userByID(ctx context.Context, q queryer, userID string) (User, error) {
	row := q.QueryRowContext(ctx,
		`select id, email, display_name, created_at, disabled_at from users where id = ?`, userID)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: loading user %s: %w", userID, err)
	}
	return u, nil
}

func userByEmail(ctx context.Context, q queryer, email string) (User, error) {
	row := q.QueryRowContext(ctx,
		`select id, email, display_name, created_at, disabled_at from users where email = ?`, email)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: loading user by email: %w", err)
	}
	return u, nil
}

func scanUser(row *sql.Row) (User, error) {
	var (
		u                User
		email, name, dis sql.NullString
		createdAt        string
	)
	if err := row.Scan(&u.ID, &email, &name, &createdAt, &dis); err != nil {
		return User{}, err
	}
	u.Email = email.String
	u.DisplayName = name.String
	u.Disabled = dis.Valid
	created, err := ParseTime(createdAt)
	if err != nil {
		return User{}, err
	}
	u.CreatedAt = created
	return u, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
