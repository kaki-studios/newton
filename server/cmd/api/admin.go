package main

// Operator commands. They exist because the final image is distroless: there is no `sqlite3` shell
// and no `psql`, so "one UPDATE bans a farm" needs a way to run that UPDATE inside the container.
//
//	docker exec <container> /app/newton -admin ban ada@example.com
//	docker exec <container> /app/newton -admin sessions ada@example.com
//	docker exec <container> /app/newton -admin unban ada@example.com
//
// They open the same database file, through the same pragma-checked path, and print a sentence a
// human can act on. They never touch the running server: SQLite's WAL mode makes a second process
// safe, and the running server's session reads see the change on the next request.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kaki-studios/newton/server/internal/store"
)

// adminRequested reports whether this invocation is an operator command, and with what.
func adminRequested(args []string) (command string, rest []string, ok bool) {
	for i, arg := range args {
		if arg == "-admin" || arg == "--admin" {
			if i+1 >= len(args) {
				return "", nil, true // -admin with no command: an error the caller reports
			}
			return args[i+1], args[i+2:], true
		}
	}
	return "", nil, false
}

// runAdmin executes one operator command.
func runAdmin(args []string, stdout, stderr io.Writer) error {
	command, rest, ok := adminRequested(args)
	if !ok {
		return errors.New("admin: not an admin invocation")
	}

	fs := flag.NewFlagSet("newton -admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "data/newton.db", "SQLite database file")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch command {
	case "":
		return fmt.Errorf("admin: -admin needs a command: %s", strings.Join(adminCommands, ", "))
	case "ban", "unban", "sessions":
	default:
		return fmt.Errorf("admin: unknown command %q: known are %s",
			command, strings.Join(adminCommands, ", "))
	}

	// The address is the last non-flag argument. Positional flags are not a thing in the flag
	// package, so it has to be the only one.
	if fs.NArg() != 1 {
		return fmt.Errorf("admin %s: expected one email address, got %d arguments. "+
			"Usage: newton -admin %s -db %s ada@example.com", command, fs.NArg(), command, *db)
	}
	email := fs.Arg(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := store.Open(ctx, store.Options{Path: *db})
	if err != nil {
		return err
	}
	defer st.Close()

	user, err := st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("admin: no user has the verified email %q. "+
			"Check the address, or list users with: newton -admin sessions -db %s <email>", email, *db)
	}
	if err != nil {
		return err
	}

	switch command {
	case "ban", "unban":
		disabled := command == "ban"
		if err := st.SetDisabled(ctx, user.ID, disabled); err != nil {
			return err
		}
		verb := "banned"
		if !disabled {
			verb = "re-enabled"
		}
		fmt.Fprintf(stdout, "%s %s (%s).", verb, email, user.ID)
		if disabled {
			fmt.Fprintln(stdout, " Their sessions were revoked; they will be refused at the next sign-in.")
		}
		return nil

	case "sessions":
		sessions, err := st.SessionsForUser(ctx, user.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s (%s), disabled=%v, %d session(s):\n",
			user.DisplayName, user.ID, user.Disabled, len(sessions))
		now := time.Now().UTC()
		for _, s := range sessions {
			state := "live"
			if !s.Expiry.After(now) {
				state = "expired"
			}
			fmt.Fprintf(stdout, "  created %s, expires %s (%s)\n",
				s.CreatedAt.Format(store.TimeFormat), s.Expiry.Format(store.TimeFormat), state)
		}
		return nil
	}
	return nil
}

var adminCommands = []string{"ban", "unban", "sessions"}
