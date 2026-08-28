package upstream

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

const klingSecret = `{"access_key":"AK-1","secret_key":"SK-abcdef"}`

// A token this gateway mints has to be one the vendor's own verifier accepts,
// which means the signature has to check out against the same secret and the
// two time claims have to be in the window that platform documents.
func TestKlingTokenVerifiesAndIsShortLived(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	token, err := signKlingJWT(klingKeypair{AccessKey: "AK-1", SecretKey: "SK-abcdef"}, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has three segments, got %d", len(parts))
	}

	mac := hmac.New(sha256.New, []byte("SK-abcdef"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if parts[2] != want {
		t.Error("the signature does not verify against the secret it was signed with")
	}

	var header struct{ Alg, Typ string }
	decodeSegment(t, parts[0], &header)
	if header.Alg != "HS256" || header.Typ != "JWT" {
		t.Errorf("header is %+v; this platform reads both fields", header)
	}

	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
		Nbf int64  `json:"nbf"`
	}
	decodeSegment(t, parts[1], &claims)
	if claims.Iss != "AK-1" {
		t.Errorf("issuer is %q; this platform reads the access key from it", claims.Iss)
	}
	if got := claims.Exp - now.Unix(); got != int64(klingTokenTTL/time.Second) {
		t.Errorf("the token claims %ds of life; this platform accepts %v", got, klingTokenTTL)
	}
	if claims.Nbf > now.Unix() {
		t.Error("not-before is in the future; ordinary clock skew would make the vendor " +
			"refuse a token this gateway had just minted, and that reads as a bad credential")
	}
}

// A stored credential of the wrong shape has to say what shape was expected.
// The operator pasting it has the vendor's console open, not this source file.
func TestKlingRefusesACredentialThatIsNotAKeypair(t *testing.T) {
	for _, secret := range []string{
		"", "not json", `{"access_key":"AK-1"}`, `{"secret_key":"SK-1"}`, `{}`,
	} {
		if _, err := parseKlingKeypair(secret); err == nil {
			t.Errorf("%q was accepted as a key pair", secret)
		} else if !strings.Contains(err.Error(), "access_key") {
			t.Errorf("the refusal must name the shape expected, got %q", err)
		}
	}
}

// The mode has to be one this package claims and one Present can carry out.
// A mode listed in the catalog but not handled here writes no credential at
// all, and the upstream's 401 points at the key rather than at the wiring.
func TestKlingModeIsHandledAndWritesABearerToken(t *testing.T) {
	if !Handles(catalog.AuthKlingJWT) {
		t.Fatal("the catalog offers this mode but this package does not claim it")
	}
	req := httptest.NewRequest(http.MethodPost, "https://api-beijing.klingai.com/v1/videos/text2video", nil)
	if err := Present(t.Context(), Presentation{
		Mode: catalog.AuthKlingJWT, Secret: klingSecret, HTTP: req,
	}); err != nil {
		t.Fatal(err)
	}
	got := req.Header.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") || strings.Count(got, ".") != 2 {
		t.Fatalf("Authorization is %q; it must carry a signed token", got)
	}
	// Nothing beyond the one header: this mode adds no others, and the
	// outbound header set is checked for exactness.
	if names := ExtraHeaders(catalog.AuthKlingJWT, klingSecret); len(names) != 0 {
		t.Errorf("this mode declares extra headers %v that it does not write", names)
	}
}

// Two requests a moment apart must not reuse a token, because the token is
// minted rather than cached and the expiry travels inside it.
func TestKlingMintsAFreshTokenPerRequest(t *testing.T) {
	k := klingKeypair{AccessKey: "AK-1", SecretKey: "SK-abcdef"}
	first, err := signKlingJWT(k, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := signKlingJWT(k, time.Unix(1_800_000_060, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("the same token came back a minute later; its expiry would be a minute stale " +
			"and eventually in the past")
	}
}

func decodeSegment(t *testing.T, segment string, dst any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("segment is not base64url without padding: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
}
