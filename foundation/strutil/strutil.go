// Package strutil holds the string and pointer helpers that every handler
// package used to carry a private copy of. They are byte-oriented on purpose:
// the callers bound storage and log sizes, which are bytes, and changing that
// silently would change what gets stored.
package strutil

// Truncate cuts s to at most n bytes.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Ellipsize cuts s to at most n bytes and marks the cut with "…", for logs and
// error messages where the reader should know something was dropped.
func Ellipsize(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Ptr returns a pointer to s, or nil for the empty string: the DTO convention
// where an absent optional field and an empty one mean the same thing.
func Ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deref is the inverse of Ptr.
func Deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
