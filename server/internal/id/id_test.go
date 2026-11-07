package id

import (
	"strings"
	"testing"
)

func TestNewIsUniqueAndOpaque(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for range n {
		got := New()
		if len(got) != 32 {
			t.Fatalf("id %q is %d characters, want 32 (128 bits as hex)", got, len(got))
		}
		if strings.ToLower(got) != got {
			t.Fatalf("id %q is not lowercase: it appears in logs and URLs", got)
		}
		if seen[got] {
			t.Fatalf("New returned %q twice in %d draws", got, n)
		}
		seen[got] = true
	}
}

// Hex, so every character is URL-safe, log-safe and case-insensitive-comparable without a decoder.
func TestNewIsHex(t *testing.T) {
	for range 100 {
		for _, r := range New() {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("non-hex character %q in an id", r)
			}
		}
	}
}
