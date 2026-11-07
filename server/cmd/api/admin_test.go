package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kaki-studios/newton/server/internal/store"
)

// The admin commands are the only way to ban someone in a distroless container (no sqlite3 shell),
// so they are tested against a real database rather than trusted.

func adminDB(t *testing.T) (path, userID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "newton.db")
	st, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	user, err := st.UpsertIdentity(context.Background(), store.Identity{
		Provider: "github", ProviderUserID: "1", Email: "Ada@Example.com", DisplayName: "Ada",
	})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if _, err := st.CreateSession(context.Background(), user.ID, "token", time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path, user.ID
}

func runAdminCommand(t *testing.T, db, command, email string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = runAdmin([]string{"-admin", command, "-db", db, email}, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestAdminBanRevokesSessions(t *testing.T) {
	db, userID := adminDB(t)

	stdout, _, err := runAdminCommand(t, db, "ban", "ada@example.com")
	if err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !strings.Contains(stdout, "banned") || !strings.Contains(stdout, "revoked") {
		t.Errorf("stdout = %q, want it to say what happened", stdout)
	}

	st, err := store.Open(context.Background(), store.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	user, err := st.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if !user.Disabled {
		t.Fatal("the user is not disabled after `ban`")
	}
	sessions, err := st.SessionsForUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("%d sessions survived the ban; they would come back on unban", len(sessions))
	}
}

func TestAdminUnbanRestoresAccess(t *testing.T) {
	db, userID := adminDB(t)
	if _, _, err := runAdminCommand(t, db, "ban", "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runAdminCommand(t, db, "unban", "ada@example.com")
	if err != nil {
		t.Fatalf("unban: %v", err)
	}
	if !strings.Contains(stdout, "re-enabled") {
		t.Errorf("stdout = %q", stdout)
	}

	st, err := store.Open(context.Background(), store.Options{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	user, err := st.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.Disabled {
		t.Fatal("the user is still disabled after `unban`")
	}
}

func TestAdminMatchesTheEmailCaseInsensitively(t *testing.T) {
	db, _ := adminDB(t)
	// The identity was stored as Ada@Example.com; an operator will type it in lowercase.
	if _, _, err := runAdminCommand(t, db, "ban", "  ADA@example.COM "); err != nil {
		t.Fatalf("ban with a differently-cased address: %v", err)
	}
}

func TestAdminUnknownEmailNamesTheFix(t *testing.T) {
	db, _ := adminDB(t)
	_, _, err := runAdminCommand(t, db, "ban", "nobody@example.com")
	if err == nil {
		t.Fatal("banning an unknown user succeeded")
	}
	if !strings.Contains(err.Error(), "no user has the verified email") {
		t.Errorf("err = %q, want it to explain the lookup", err)
	}
	if !strings.Contains(err.Error(), "nobody@example.com") {
		t.Errorf("err = %q, want it to echo the address that was searched for", err)
	}
}

func TestAdminSessionsListsThem(t *testing.T) {
	db, _ := adminDB(t)
	stdout, _, err := runAdminCommand(t, db, "sessions", "ada@example.com")
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	for _, want := range []string{"Ada", "disabled=false", "1 session(s)", "live"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout is missing %q:\n%s", want, stdout)
		}
	}
}

func TestAdminRejectsBadInvocations(t *testing.T) {
	db, _ := adminDB(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no command", []string{"-admin"}, "needs a command"},
		{"unknown command", []string{"-admin", "delete"}, "unknown command"},
		{"no email", []string{"-admin", "ban", "-db", db}, "expected one email"},
		{"two emails", []string{"-admin", "ban", "-db", db, "a@example.com", "b@example.com"}, "expected one email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runAdmin(tc.args, &out, &errOut)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want %q", err, tc.want)
			}
		})
	}
}

func TestAdminMissingDatabaseNamesTheFix(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := runAdmin([]string{"-admin", "ban", "-db", filepath.Join(t.TempDir(), "nope", "newton.db"), "a@example.com"}, &out, &errBuf)
	if err == nil {
		t.Fatal("a missing database was accepted")
	}
	// Either the directory creation failed or the file could not be opened: both must name the path.
	if !strings.Contains(err.Error(), "newton.db") {
		t.Errorf("err = %q, want it to name the database path", err)
	}
}

func TestAdminIsDetectedBeforeConfigValidation(t *testing.T) {
	// The whole point: -admin must run inside a container whose config the caller does not need to
	// reproduce, so the detection is on the raw argv.
	for _, args := range [][]string{
		{"-admin", "ban", "a@b.c"},
		{"-admin", "sessions", "-db", "x.db", "a@b.c"},
		{"-db", "x.db", "-admin", "ban", "a@b.c"},
	} {
		if _, _, ok := adminRequested(args); !ok {
			t.Errorf("adminRequested(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{{}, {"-static", "public"}, {"-healthcheck"}} {
		if _, _, ok := adminRequested(args); ok {
			t.Errorf("adminRequested(%v) = true, want false", args)
		}
	}
}
