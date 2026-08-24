// Package keyfmt defines the outward format of a virtual API key:
//
//	sk-flb-v1-<base62 of 32 random bytes><CRC32 checksum, 6 base62 characters>
//
// Generation and validation live together so that whoever issues a key and
// whoever verifies one share a single definition. The version segment lets a
// future format be dispatched on, and the checksum allows a malformed key to be
// rejected locally before any database lookup — and lets leak scanners
// recognize one.
package keyfmt

import (
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
)

const (
	// Prefix is the fixed prefix: a type segment, a product segment and a
	// version segment.
	Prefix = "sk-flb-v1-"
	// randomBytes is the entropy of the random body. 256 bits is well beyond
	// the usual 128, and there is no reason to shorten it.
	randomBytes = 32
	// checkLen is the fixed checksum length; six base62 characters cover the
	// full CRC32 range, since 62^6 > 2^32.
	checkLen = 6
	// minBodyLen is the lower bound on the random body: both the display
	// prefix and the checksum need enough characters to exist. 256 bits is
	// about 43 base62 characters, so this is a defensive floor rather than an
	// exact figure.
	minBodyLen = 8
)

// New generates one complete key plaintext.
func New() (string, error) {
	raw := make([]byte, randomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var n big.Int
	n.SetBytes(raw)
	body := n.Text(62)
	if len(body) < minBodyLen {
		return "", fmt.Errorf("random body too short: %d", len(body))
	}
	return Prefix + body + checksum(body), nil
}

// Valid checks the prefix, the minimum length and the checksum, entirely
// locally. It never touches the database: it answers "is this the shape of a key
// we issue", not "does this key exist". Those two must be indistinguishable from
// the outside, or the difference becomes enumeration feedback.
func Valid(key string) bool {
	if !strings.HasPrefix(key, Prefix) {
		return false
	}
	rest := key[len(Prefix):]
	if len(rest) < minBodyLen+checkLen {
		return false
	}
	body, check := rest[:len(rest)-checkLen], rest[len(rest)-checkLen:]
	return check == checksum(body)
}

// checksum computes the trailing check characters over the random body:
// CRC32-IEEE, base62 encoded, left-padded to a fixed length. The checksum is not
// a secret — the key is — so it needs neither an HMAC nor a constant-time
// comparison.
func checksum(body string) string {
	var n big.Int
	n.SetUint64(uint64(crc32.ChecksumIEEE([]byte(body))))
	s := n.Text(62)
	if len(s) < checkLen {
		s = strings.Repeat("0", checkLen-len(s)) + s
	}
	return s
}
