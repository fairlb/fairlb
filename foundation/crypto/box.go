package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Box is the application-level AES-256-GCM encryptor used for values that must
// not sit in the database in the clear, such as TOTP secrets and upstream
// provider credentials. The ciphertext is nonce || ciphertext, with the GCM tag
// inside the latter.
type Box struct {
	aead cipher.AEAD
}

// NewBox builds an encryptor from a 32-byte master key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("crypto: master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: init AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: init GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// DecodeKeyHex parses the 64-hex-character master key, which is how SECRET_KEY
// is supplied.
func DecodeKeyHex(s string) ([]byte, error) {
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, errors.New("crypto: SECRET_KEY must be 64 hex characters (32 bytes)")
	}
	return key, nil
}

// Seal encrypts and returns nonce||ciphertext. aad is authenticated but not
// stored — passing the row's primary key there prevents a ciphertext from being
// moved to a different row.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts what Seal produced; a tampered ciphertext or a mismatched aad
// returns an error.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	if len(sealed) < b.aead.NonceSize() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ct := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	pt, err := b.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, errors.New("crypto: decryption failed (wrong key, or the ciphertext was tampered with)")
	}
	return pt, nil
}
