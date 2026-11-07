// Package id generates opaque identifiers for users, sessions and generations.
//
// It is a package rather than a call to a UUID library because tests need to make ids
// deterministic: the SSE contract fixtures in testdata/sse are compared byte-for-byte, and a
// random id in the `done` event would make that impossible.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Newer returns a fresh identifier. Swap it in tests (and only in tests) to make output
// reproducible; production code keeps New.
type Newer func() string

// New returns 128 bits from crypto/rand as lowercase hex.
//
// Hex, not base64url: ids appear in logs, database rows and SSE frames, and a format with
// no case or padding rules is one less thing to get wrong. 32 characters is also shorter
// than a UUID's 36 for the same entropy.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never fails on supported platforms; if it ever does, a
		// predictable id is worse than a crashed process.
		panic(fmt.Sprintf("id: crypto/rand is unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}
