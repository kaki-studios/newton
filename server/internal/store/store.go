// Package store owns the SQLite file: opening it with the right pragmas, running the
// embedded migrations, and the small set of queries the rest of the server needs.
//
// Nothing above this package writes SQL. Handlers ask for a user, a generation or a count;
// they never build a query. That keeps the one place that could accidentally concatenate
// untrusted input visible in review.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	sqlite3 "modernc.org/sqlite" // also registers the "sqlite" database/sql driver

	"github.com/kaki-studios/newton/server/migrations"
)

// ErrNotFound is returned when a lookup matched no row. Handlers map it to 404/401; callers
// never have to compare against sql.ErrNoRows.
var ErrNotFound = errors.New("store: not found")

// ErrInFlight means the user already has a pending generation. The database's partial unique
// index is the authority, so this is safe across processes.
var ErrInFlight = errors.New("store: a generation is already pending for this user")

// ErrConflict is a unique-constraint violation the caller is expected to handle deliberately
// (for example: attaching an identity that another user already owns).
var ErrConflict = errors.New("store: unique constraint violated")

// Store is a handle on one SQLite database. Every method is safe for concurrent use.
type Store struct {
	db *sql.DB
	// now is the clock for anything the *database* would otherwise timestamp itself.
	// Injected so quota tests can jump a day boundary instead of sleeping.
	now func() time.Time
}

// Options are the knobs worth varying in tests.
type Options struct {
	// Path is the database file. Parent directories are created.
	Path string
	// Now defaults to time.Now.
	Now func() time.Time
}

// Open opens (creating if needed) the database at opts.Path, applies pending migrations, and
// returns a ready Store.
//
// Migrations run here, before any listener opens, so a replica either comes up with the
// schema the binary expects or fails loudly at boot. With replicas: 1 that is safe; a second
// process applying migrations concurrently would need an advisory lock, which SQLite does not
// have and this deployment does not need.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("store: Options.Path is required, e.g. data/newton.db")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	// The file lives on a Docker volume; the directory may not exist on a fresh node.
	if dir := filepath.Dir(opts.Path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: creating %s for the database: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", DSN(opts.Path))
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", opts.Path, err)
	}

	// One connection, deliberately. SQLite has a single writer anyway; serializing here
	// means a read-then-write transaction can never lose a lock upgrade to another
	// connection, so there is no SQLITE_BUSY retry loop anywhere in this package. This
	// workload measures writes in single digits per minute, so it costs nothing.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connecting to %s: %w", opts.Path, err)
	}

	s := &Store{db: db, now: opts.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DSN builds the connection string. Pragmas travel in the DSN rather than as a `PRAGMA`
// statement after connecting because they are then applied to *every* connection the pool
// opens, including the ones Litestream and the migration runner use.
//
//   - journal_mode(WAL): readers do not block the writer; Litestream needs WAL to replicate.
//   - busy_timeout(5000): a blocked statement waits up to 5s instead of failing immediately.
//   - foreign_keys(1): SQLite defaults this OFF; the schema's cascades depend on it.
//   - synchronous(NORMAL): safe with WAL, and durable enough for sessions and quotas.
//   - _txlock=immediate: `BEGIN IMMEDIATE` for every transaction, so a read-then-write
//     reserve acquires the write lock up front instead of failing on upgrade.
func DSN(path string) string {
	q := url.Values{}
	for _, pragma := range []string{
		"journal_mode(WAL)",
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"synchronous(NORMAL)",
	} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")

	// A path may contain characters that are meaningful in a URI (the tests use
	// t.TempDir(), which is fine, but a directory with a space would not be).
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped + "?" + q.Encode()
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for migrations and health checks. Normal code should use the typed
// methods; this exists so `internal/web` can run `SELECT 1`.
func (s *Store) DB() *sql.DB { return s.db }

// Now returns the store's clock, so callers compute day boundaries with the same clock the
// quota queries use.
func (s *Store) Now() time.Time { return s.now().UTC() }

func (s *Store) migrate(ctx context.Context) error {
	goose.SetBaseFS(migrations.FS)
	// goose logs through log.Printf by default, which bypasses the structured logger and lands
	// unlabelled in the same stream as the JSON logs. Route it through slog instead.
	goose.SetLogger(gooseLogger{})
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("store: goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, s.db, "."); err != nil {
		return fmt.Errorf("store: applying the embedded migrations (server/migrations): %w", err)
	}
	return nil
}

// gooseLogger adapts goose's two-method logger to slog. Migrations that ran are debug-level detail:
// the version they reached is logged once at info by the caller, which is what an operator wants.
type gooseLogger struct{}

func (gooseLogger) Printf(format string, v ...any) {
	slog.Default().Debug(fmt.Sprintf("goose: "+format, v...))
}

func (gooseLogger) Fatalf(format string, v ...any) {
	// A library calling Fatal would os.Exit and skip this package's cleanup. The error is
	// already returned to the caller, which decides what to do about it.
	slog.Default().Error(fmt.Sprintf("goose: "+format, v...))
}

// Version is the applied goose version, reported by /api/health so a deploy can tell which
// binary's schema is live without opening the file.
func (s *Store) Version(ctx context.Context) (int64, error) {
	v, err := goose.GetDBVersionContext(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	return v, nil
}

// tx runs fn inside a transaction, rolling back on error or panic. Every write path in this
// package goes through it so nothing is ever left half-applied.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		// Rollback errors are uninteresting next to the reason we are rolling back.
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	return nil
}

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE from sqlite3.h, spelled out here rather
// than imported from modernc.org/sqlite/lib so this package's boundary is the driver's public
// Error type alone. 2067 is the *extended* code for a UNIQUE (or primary key) violation; the
// generic SQLITE_CONSTRAINT is 19, so the comparison is exact and an unrelated constraint
// failure (NOT NULL, CHECK, foreign key) is not mistaken for a duplicate.
const sqliteConstraintUnique = 2067

// classify maps a driver error onto this package's sentinels. Callers that need to tell
// "already pending" apart from "two users, same email" keep their own wrapping.
//
// A duplicate *primary key* is not a unique-index violation in the sense the callers care about:
// a colliding generation id is a bug in id minting, not "this user already has a request
// running". The message text is the only reliable way to tell them apart, because SQLite reports
// both as SQLITE_CONSTRAINT_UNIQUE.
func classify(err error, uniqueErr error) error {
	if err == nil {
		return nil
	}
	var sqlErr *sqlite3.Error
	if errors.As(err, &sqlErr) && sqlErr.Code() == sqliteConstraintUnique {
		if strings.Contains(sqlErr.Error(), "PRIMARY KEY") {
			// Not a business-rule conflict: two rows were minted the same id. That is a
			// programming error and must surface as one, not as a 409 the user could retry.
			return fmt.Errorf("store: duplicate primary key (ids must be unique): %w", err)
		}
		if uniqueErr != nil {
			return fmt.Errorf("%w: %v", uniqueErr, err)
		}
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// TimeFormat is the only timestamp format in the database: ISO-8601 UTC, millisecond
// precision, so lexicographic order equals chronological order and Go can round-trip it.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t the way every timestamp column stores it.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a timestamp column.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parsing timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// TrimEmail normalizes an identity email for the "link by verified email" rule. Case folding
// is all it does: providers normalize their own domains, and inventing further rules creates
// accounts that cannot be matched back.
func TrimEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
