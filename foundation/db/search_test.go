package db

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSearchTermEscapesAndBounds(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"  acme ":        "acme",
		"100%":           `100\%`,
		"a_b":            `a\_b`,
		`c:\path`:        `c:\\path`,
		"%%%%":           `\%\%\%\%`,
		"plain-search.1": "plain-search.1",
	}
	for in, want := range cases {
		if got := SearchTerm(in); got != want {
			t.Errorf("SearchTerm(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 250)
	if got := SearchTerm(long); len(got) != maxSearchBytes {
		t.Errorf("long term should be bounded to %d bytes, got %d", maxSearchBytes, len(got))
	}
	// A bound that lands inside a multi-byte character must back off to the
	// previous whole character, not hand the database an invalid string.
	han := strings.Repeat("汉", 100) // 300 bytes
	got := SearchTerm(han)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 66 {
		t.Errorf("multi-byte bound: valid=%v runes=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
	}
}
