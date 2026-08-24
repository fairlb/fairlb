package strutil

import "testing"

func TestTruncateAndEllipsize(t *testing.T) {
	if got := Truncate("abcdef", 3); got != "abc" {
		t.Fatalf("Truncate = %q", got)
	}
	if got := Truncate("ab", 3); got != "ab" {
		t.Fatalf("Truncate short = %q", got)
	}
	if got := Ellipsize("abcdef", 3); got != "abc…" {
		t.Fatalf("Ellipsize = %q", got)
	}
	if got := Ellipsize("abc", 3); got != "abc" {
		t.Fatalf("Ellipsize exact = %q", got)
	}
}

func TestPtrAndDeref(t *testing.T) {
	if Ptr("") != nil {
		t.Fatal("Ptr(\"\") should be nil")
	}
	if p := Ptr("x"); p == nil || *p != "x" || Deref(p) != "x" {
		t.Fatal("Ptr/Deref round trip")
	}
	if Deref(nil) != "" {
		t.Fatal("Deref(nil)")
	}
}
