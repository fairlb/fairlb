// Package testx holds the tiny assertions and fixtures that test files in both
// modules used to copy verbatim (ADR-0206).
package testx

import (
	"testing"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
)

// MustUUID parses s into a pgtype.UUID or fails the test.
func MustUUID(t testing.TB, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatal(err)
	}
	return u
}

// HasCJK reports whether s contains a Han character -- the probe for "this
// text leaked an operator-facing Chinese note into a client-facing message".
func HasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
