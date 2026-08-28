package upstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A Kling access-key pair, signed into a short-lived JWT per request.
//
// This platform does not take a static token. It takes a JWT the caller signs
// with their own secret, and the one it accepts is valid for half an hour.
// Storing the resulting token as the provider's credential therefore works for
// half an hour and then answers 401 on every request -- the same shape as a
// service-account key, and the same reason this is a mode rather than "paste
// the token in".
//
// Written here rather than pulled in as a dependency for the same reason the
// AWS signature is: it is HMAC-SHA256 over two base64url segments, the whole of
// it is below, and a JWT library would bring an algorithm registry this has no
// use for.

// klingTokenTTL is how long a minted token claims to be valid.
//
// Half an hour is what this vendor's own documentation specifies. It is not a
// tuning knob: a longer one is rejected upstream, and a shorter one costs a
// signature per request for nothing, since the token is minted per request
// anyway.
const klingTokenTTL = 30 * time.Minute

// klingNotBeforeSkew backdates the token slightly. Clock skew between this
// deployment and the vendor is the ordinary cause of a token that is refused
// for being issued in the future, and that failure reads as a bad credential.
const klingNotBeforeSkew = 5 * time.Second

// klingKeypair is the stored credential shape.
type klingKeypair struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

const klingKeypairShape = `{"access_key": "...", "secret_key": "..."}`

func parseKlingKeypair(secret string) (klingKeypair, error) {
	var k klingKeypair
	if err := decodeCredential(secret, &k, klingKeypairShape); err != nil {
		return klingKeypair{}, err
	}
	if strings.TrimSpace(k.AccessKey) == "" || strings.TrimSpace(k.SecretKey) == "" {
		return klingKeypair{}, fmt.Errorf(
			"upstream: this provider's credential must be %s, with both fields set", klingKeypairShape)
	}
	return k, nil
}

// setKlingBearer mints a token for this request and writes it.
//
// Minted per request rather than cached. The service-account mode caches
// because its token costs a network round trip; this one costs one HMAC over a
// few dozen bytes, and a cache would trade that for a stale-token window and an
// eviction policy to reason about.
func setKlingBearer(p Presentation) error {
	k, err := parseKlingKeypair(p.Secret)
	if err != nil {
		return err
	}
	token, err := signKlingJWT(k, time.Now())
	if err != nil {
		return err
	}
	p.HTTP.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// signKlingJWT builds the token this vendor's platform accepts.
//
// The header carries a non-standard `typ` of "JWT" alongside `alg`, which is
// what that platform documents; the claims are the issuer, an expiry and a
// not-before. There is no subject and no audience, and adding either would be
// inventing a claim the verifier does not read.
func signKlingJWT(k klingKeypair, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("upstream: kling: encode token header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": k.AccessKey,
		"exp": now.Add(klingTokenTTL).Unix(),
		"nbf": now.Add(-klingNotBeforeSkew).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("upstream: kling: encode token claims: %w", err)
	}
	signing := b64url(header) + "." + b64url(claims)
	mac := hmac.New(sha256.New, []byte(k.SecretKey))
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil)), nil
}

// b64url is base64url without padding, which is what a JWT segment is.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
