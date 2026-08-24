package db

import "strings"

// maxSearchBytes bounds a free-text search term. The OpenAPI specs declare
// maxLength: 200 on every `q` parameter, but no request validator enforces it;
// the bound has to be applied where the term is used or it is not a bound.
const maxSearchBytes = 200

// SearchTerm normalises a free-text search term for an `ILIKE '%' || $1 || '%'`
// predicate: trims it, bounds its length, and escapes the LIKE metacharacters.
//
// Unescaped, `%` matches everything (defeating the point of a server-side
// search) and `_` matches any single character; both also amplify the
// leading-wildcard scan these predicates already pay for. PostgreSQL's default
// LIKE escape character is the backslash, so no ESCAPE clause is needed.
func SearchTerm(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxSearchBytes {
		s = s[:maxSearchBytes]
		// Do not cut a multi-byte character in half.
		for len(s) > 0 && !isRuneStart(s[len(s)-1]) {
			s = s[:len(s)-1]
		}
		if len(s) > 0 && s[len(s)-1] >= 0xC0 {
			s = s[:len(s)-1]
		}
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
