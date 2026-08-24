// Package crypto holds the shared cryptographic primitives: argon2id password
// hashing, generation and hashing of single-use tokens, and application-level
// AES-256-GCM encryption.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// These parameters define the only accepted password baseline. Changing them
// requires an explicit rehash flow; verification does not accept an older set.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 4
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// HashPassword produces an argon2id hash in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword compares a password against the current PHC baseline in
// constant time. It returns whether it matched and a parse error; a mismatch is
// not an error.
func VerifyPassword(password, phc string) (bool, error) {
	memory, time, threads, salt, key, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(key))) //nolint:gosec // the key length is always argonKeyLen
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

func parsePHC(phc string) (memory, time uint32, threads uint8, salt, key []byte, err error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, errors.New("crypto: not an argon2id PHC string")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return 0, 0, 0, nil, nil, errors.New("crypto: unsupported argon2 version")
	}
	if parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", argonMemoryKiB, argonTime, argonThreads) {
		return 0, 0, 0, nil, nil, errors.New("crypto: unsupported argon2 parameters")
	}
	memory, time, threads = argonMemoryKiB, argonTime, argonThreads
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return 0, 0, 0, nil, nil, errors.New("crypto: cannot decode salt")
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != argonKeyLen {
		return 0, 0, 0, nil, nil, errors.New("crypto: cannot decode hash")
	}
	return memory, time, threads, salt, key, nil
}

// DummyPasswordHash returns a hash to verify against when there is no real one:
// the account does not exist, is disabled, or has no password set.
//
// Verifying anyway is the point. argon2id at these parameters takes ~100ms, so
// skipping it when the account is unknown makes the response time itself answer
// "is this email registered" -- an enumeration oracle that no amount of uniform
// error text closes.
//
// One value serves every caller. Verification cost depends on the encoded
// parameters, not on the plaintext behind them, so three separate dummies
// bought nothing over one; and being lazy, its ~100ms stays out of process
// startup, which matters because migrate subcommands and every test binary load
// these packages too.
func DummyPasswordHash() string { return dummyPasswordHash() }

var dummyPasswordHash = sync.OnceValue(func() string {
	h, err := HashPassword("fairlb-timing-equalizer")
	if err != nil {
		// Unreachable: HashPassword only fails if the system CSPRNG does, and a
		// process that cannot generate a salt cannot serve authentication.
		panic(err)
	}
	return h
})

// MaskSecret builds the display hint for a stored credential: a recognizable
// head and tail with the middle elided.
//
// One implementation, because there were two and they had drifted: the gateway
// admin page masked with `*` and kept a vendor prefix like `sk-`, while the BYOK
// page masked with `•` and kept a fixed six characters. The same key rendered
// differently depending on which screen you were on (ADR-0180).
//
// Two properties, and only the first is a safety one:
//
//   - **The middle never survives.** A secret with no middle to elide -- four
//     characters or fewer -- shows nothing at all, rather than a "head…tail"
//     that would print the whole thing.
//   - The vendor prefix is kept when there is one, so the reader can tell an
//     OpenAI key from an Anthropic key without revealing more of either. A dash
//     more than six characters in is part of the secret, not a prefix.
func MaskSecret(s string) string {
	const tail = 4
	if len(s) <= tail {
		return strings.Repeat("•", len([]rune(s)))
	}
	head := ""
	if i := strings.Index(s, "-"); i > 0 && i <= 6 {
		head = s[:i+1]
	}
	return head + "…" + s[len(s)-tail:]
}
