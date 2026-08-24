package crypto

import (
	"strings"
	"testing"
)

// The mask has one job that matters and one that is convenience, and only the
// first is a safety property: **the middle must never survive**.
func TestMaskSecretNeverLeaksTheMiddle(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	got := MaskSecret(secret)
	middle := secret[3 : len(secret)-4]
	if strings.Contains(got, middle) {
		t.Fatalf("the mask %q contains the secret's middle", got)
	}
	if !strings.HasSuffix(got, secret[len(secret)-4:]) {
		t.Fatalf("the mask %q dropped the trailing four characters", got)
	}
	if !strings.HasPrefix(got, "sk-") {
		t.Fatalf("the mask %q dropped the vendor prefix", got)
	}
}

// A short secret has no middle to elide, so nothing of it may show at all.
// Returning a "head…tail" of a six-character string would print the whole thing.
func TestMaskSecretHidesShortSecretsEntirely(t *testing.T) {
	for _, s := range []string{"", "a", "abcd"} {
		got := MaskSecret(s)
		if strings.Trim(got, "•") != "" {
			t.Errorf("MaskSecret(%q) = %q, which shows characters of the secret", s, got)
		}
		// Character count, not byte count: the fill is a multibyte rune, and
		// comparing bytes would make this assertion about UTF-8 rather than
		// about the mask.
		if len([]rune(got)) != len([]rune(s)) {
			t.Errorf("MaskSecret(%q) = %q, which has %d characters instead of %d",
				s, got, len([]rune(got)), len([]rune(s)))
		}
	}
}

// A secret with no vendor prefix still masks; the prefix is a nicety, not a
// precondition.
func TestMaskSecretWithoutPrefix(t *testing.T) {
	got := MaskSecret("abcdefghijklmnop")
	if got != "…mnop" {
		t.Fatalf("MaskSecret without a prefix = %q", got)
	}
}

// A dash too far into the string is not a vendor prefix — it is part of the
// secret, and keeping it would disclose more than four characters.
func TestMaskSecretIgnoresLateDash(t *testing.T) {
	got := MaskSecret("abcdefghij-klmnopqrst")
	if strings.Contains(got, "abcdefghij") {
		t.Fatalf("MaskSecret kept a late dash as a prefix: %q", got)
	}
}
