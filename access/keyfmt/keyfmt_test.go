package keyfmt

import (
	"strings"
	"testing"
)

// A generated key must pass its own validation and carry every segment.
func TestNewProducesValidKey(t *testing.T) {
	for range 100 {
		key, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !strings.HasPrefix(key, Prefix) {
			t.Fatalf("wrong prefix: %q", key)
		}
		if len(key) < len(Prefix)+minBodyLen+checkLen {
			t.Fatalf("too short: %q", key)
		}
		if !Valid(key) {
			t.Fatalf("a key we generated ourselves failed validation: %q", key)
		}
	}
}

// Changing any single character must be rejected; both the body and the
// checksum are probed.
func TestValidRejectsTamper(t *testing.T) {
	key, err := New()
	if err != nil {
		t.Fatal(err)
	}
	flip := func(s string, i int) string {
		c := byte('x')
		if s[i] == 'x' {
			c = 'y'
		}
		return s[:i] + string(c) + s[i+1:]
	}
	bodyPos := len(Prefix) + 2          // inside the random body
	checkPos := len(key) - checkLen + 1 // inside the checksum
	if Valid(flip(key, bodyPos)) {
		t.Fatal("a key with a mutated body still passed validation")
	}
	if Valid(flip(key, checkPos)) {
		t.Fatal("a key with a mutated checksum still passed validation")
	}
}

// A key in the previous format, and every kind of fragment, must be rejected.
func TestValidRejectsLegacyAndMalformed(t *testing.T) {
	bad := []string{
		"",
		"sk-plb-" + strings.Repeat("a", 43), // the previous prefix
		Prefix,                              // prefix only
		Prefix + "abc",                      // too short
		"Bearer " + Prefix,                  // scheme not stripped
	}
	for _, k := range bad {
		if Valid(k) {
			t.Fatalf("should not have passed: %q", k)
		}
	}
}

// The checksum is always the same length, including for inputs whose CRC needs
// padding, and is deterministic for a given input.
func TestChecksumFixedLength(t *testing.T) {
	inputs := []string{"a", "0", strings.Repeat("z", 50), "3zopxb"}
	for _, in := range inputs {
		c := checksum(in)
		if len(c) != checkLen {
			t.Fatalf("checksum(%q) length %d != %d", in, len(c), checkLen)
		}
		if c != checksum(in) {
			t.Fatalf("checksum(%q) is not deterministic", in)
		}
	}
}
