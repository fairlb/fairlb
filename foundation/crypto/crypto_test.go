package crypto

import (
	"strings"
	"testing"
)

func TestPasswordRoundtrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("PHC parameters do not match the expected ones: %s", phc)
	}
	ok, err := VerifyPassword("correct horse battery staple", phc)
	if err != nil || !ok {
		t.Fatalf("the correct password failed to verify: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong password", phc)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a wrong password must not verify")
	}
}

func TestPasswordHashUnique(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("the salt is random, so hashing the same password twice must differ")
	}
}

func TestVerifyPasswordMalformed(t *testing.T) {
	for _, phc := range []string{
		"",
		"plain",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=8192,t=1,p=1$MDEyMzQ1Njc4OWFiY2RlZg$YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE",
		"$argon2id$v=19junk$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=4,legacy=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!",
	} {
		if ok, err := VerifyPassword("pw", phc); err == nil || ok {
			t.Fatalf("the malformed string %q should error: ok=%v err=%v", phc, ok, err)
		}
	}
}

func TestTokenHash(t *testing.T) {
	tok, err := NewToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 43 { // 32 bytes of base64url, unpadded
		t.Fatalf("unexpected token length: %d", len(tok))
	}
	// Known answer: the hex sha256 of "test".
	if got := HashToken("test"); got != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("HashToken does not match the known answer: %s", got)
	}
	tok2, _ := NewToken(32)
	if tok == tok2 {
		t.Fatal("tokens must be random")
	}
}

func TestBoxRoundtrip(t *testing.T) {
	key, err := DecodeKeyHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("totp-secret"), []byte("row-id"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := box.Open(sealed, []byte("row-id"))
	if err != nil || string(pt) != "totp-secret" {
		t.Fatalf("decryption failed: %s %v", pt, err)
	}
	if _, err := box.Open(sealed, []byte("other-row")); err == nil {
		t.Fatal("a mismatched aad must fail")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := box.Open(sealed, []byte("row-id")); err == nil {
		t.Fatal("a tampered ciphertext must fail")
	}
}

func TestBoxKeyValidation(t *testing.T) {
	if _, err := NewBox(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key must be refused")
	}
	if _, err := DecodeKeyHex("zz"); err == nil {
		t.Fatal("a non-hex value must be refused")
	}
	if _, err := DecodeKeyHex("abcd"); err == nil {
		t.Fatal("a too-short value must be refused")
	}
}
