package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// NewToken generates a URL-safe opaque token with n bytes of entropy, for
// sessions and single-use links. The plaintext exists only at issue time;
// anything stored goes through HashToken.
func NewToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the token's SHA-256 as hex, which indexes well.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// humanCodeStrip removes the two non-alphanumeric characters of base64url.
// These codes have to be read aloud and copied by hand, and both `-` and `_` are
// error-prone in exactly those situations.
var humanCodeStrip = strings.NewReplacer("-", "", "_", "")

// HumanCode generates an n-character uppercase alphanumeric code for humans to
// transcribe, such as an invite or redemption code.
//
// It must loop until it has enough characters, because how many survive the
// stripping is random. Drawing a single 9-byte block (12 base64url characters)
// and slicing the first 10 looks equivalent, but 12 characters contain three or
// more stripped characters about 0.54% of the time — roughly one panic in every
// 184 codes. A defect that is right most of the time is not something random
// test runs catch; it has to be removed structurally.
//
// It lives here rather than being copied by the second caller precisely because
// that lesson was paid for once. A copy is a fresh chance for it to come back.
func HumanCode(n int) (string, error) {
	var b strings.Builder
	for b.Len() < n {
		tok, err := NewToken(12)
		if err != nil {
			return "", err
		}
		b.WriteString(strings.ToUpper(humanCodeStrip.Replace(tok)))
	}
	return b.String()[:n], nil
}
