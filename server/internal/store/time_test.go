package store

import (
	"testing"
	"time"
)

func TestTimeFormatRoundTrips(t *testing.T) {
	want := time.Date(2026, 3, 10, 12, 34, 56, 789000000, time.UTC)
	got, err := ParseTime(FormatTime(want))
	if err != nil {
		t.Fatalf("ParseTime: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("round trip = %s, want %s", got, want)
	}
	if s := FormatTime(want); s != "2026-03-10T12:34:56.789Z" {
		t.Fatalf("FormatTime = %q, want ISO-8601 with milliseconds", s)
	}
}

// The day-boundary queries compare timestamp text with `>=`. That is only correct if the
// format sorts lexicographically in the same order it sorts chronologically, including
// across a millisecond carry and across UTC midnight.
func TestTimeFormatSortsChronologically(t *testing.T) {
	instants := []time.Time{
		time.Date(2026, 3, 9, 23, 59, 59, 999000000, time.UTC),
		time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 10, 0, 0, 0, 1000000, time.UTC),
		time.Date(2026, 3, 10, 9, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for i := 1; i < len(instants); i++ {
		prev, next := FormatTime(instants[i-1]), FormatTime(instants[i])
		if !(prev < next) {
			t.Fatalf("%s (%s) does not sort before %s (%s)", instants[i-1], prev, instants[i], next)
		}
	}
}

func TestParseTimeRejectsGarbage(t *testing.T) {
	if _, err := ParseTime("yesterday"); err == nil {
		t.Fatal("ParseTime accepted a non-timestamp")
	}
}

func TestTrimEmail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  Ada@Example.COM ", "ada@example.com"},
		{"", ""},
	} {
		if got := TrimEmail(tc.in); got != tc.want {
			t.Errorf("TrimEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
