package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// Service-account credentials, exchanged for a short-lived access token.
//
// The exchange is the whole difficulty. A service-account key is not a bearer
// token: it is signed into an assertion, posted to a token endpoint, and what
// comes back is valid for an hour. Storing the resulting token as the
// provider's credential works for exactly one hour and then every request to
// that provider answers 401 -- which is why a gateway that cannot refresh
// cannot honestly claim to support these endpoints.

// cloudPlatformScope is the scope the prediction endpoints check. It is fixed
// rather than configurable: a narrower scope does not exist for these
// endpoints, and a wider one is not a setting anybody should be offered.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// defaultTokenURI is where the assertion is exchanged when the key does not
// name an endpoint. Every key issued in the last decade names one.
const defaultTokenURI = "https://oauth2.googleapis.com/token"

// tokenExchangeTimeout bounds the exchange. It sits on the request path when a
// token is due for renewal, so an unreachable token endpoint must not hold a
// forwarded request open indefinitely.
const tokenExchangeTimeout = 10 * time.Second

// serviceAccountKey is the stored credential shape: the vendor's own key file,
// verbatim. Nothing is unpacked into the profile, because every field of it is
// secret-adjacent and the profile is returned by the admin API.
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

const serviceAccountShape = `the service-account key file, as downloaded`

// tokenSources caches one token source per distinct credential.
//
// Caching is not an optimisation here, it is the feature. A token source is
// what holds the unexpired token; building a new one per request would mint a
// fresh assertion and perform a fresh exchange every single time, adding a
// round trip to every forward and spending the token endpoint's quota on work
// whose whole point was to be reused.
//
// Bounded rather than a plain map: the key is derived from the credential, so
// an install that rotates credentials would otherwise accumulate an entry per
// generation for the lifetime of the process. Evicting one costs a single
// extra exchange.
var (
	tokenSourcesMu sync.Mutex
	tokenSources   = mustCache(32)
)

func mustCache(size int) *lru.Cache[string, oauth2.TokenSource] {
	c, err := lru.New[string, oauth2.TokenSource](size)
	if err != nil {
		// New only fails on a non-positive size, which is a constant here.
		panic(fmt.Sprintf("upstream: token source cache: %v", err))
	}
	return c
}

// exchangeContext is the context the token exchange runs under.
//
// Deliberately *not* the forwarded request's context. A token source outlives
// the request that first built it, and a refresh an hour later would then run
// under a context cancelled the moment that first request finished -- so the
// provider would work until its first token expired and fail permanently
// afterwards. That failure appears an hour after the change that caused it,
// which is the hardest kind to attribute.
//
// The client carries this deployment's own dial settings, and a timeout,
// because the token source's interface has no place to pass one in later.
func exchangeContext() context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Timeout:   tokenExchangeTimeout,
		Transport: httpx.UpstreamTransport(),
	})
}

func parseServiceAccountKey(secret string) (*jwt.Config, error) {
	var k serviceAccountKey
	if err := decodeCredential(secret, &k, serviceAccountShape); err != nil {
		return nil, err
	}
	if k.Type != "" && k.Type != "service_account" {
		// Naming what was found is worth the line: the other key files a
		// console offers look similar enough that pasting the wrong one is the
		// likeliest mistake, and "not valid JSON" would be a lie about it.
		return nil, fmt.Errorf(
			"upstream: this provider's credential must be %s, and this one is a %q key",
			serviceAccountShape, k.Type)
	}
	if strings.TrimSpace(k.ClientEmail) == "" || strings.TrimSpace(k.PrivateKey) == "" {
		return nil, fmt.Errorf(
			"upstream: this provider's credential must be %s, with client_email and private_key",
			serviceAccountShape)
	}
	tokenURI := k.TokenURI
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	return &jwt.Config{
		Email:        k.ClientEmail,
		PrivateKey:   []byte(k.PrivateKey),
		PrivateKeyID: k.PrivateKeyID,
		TokenURL:     tokenURI,
		Scopes:       []string{cloudPlatformScope},
	}, nil
}

// tokenSourceFor returns the cached token source for this credential, building
// it if necessary. The returned source hands back the token it already holds
// until that token is close to expiry, and exchanges a new one when it is not.
func tokenSourceFor(secret string) (oauth2.TokenSource, error) {
	sum := sha256.Sum256([]byte(secret))
	key := hex.EncodeToString(sum[:])

	// The lock covers the lookup as well as the insert. Two concurrent first
	// requests would otherwise each parse the key and each install a source,
	// and the loser's cached token is thrown away -- a wasted exchange every
	// time a cold provider gets two requests at once, which is the normal shape
	// of traffic arriving.
	tokenSourcesMu.Lock()
	defer tokenSourcesMu.Unlock()
	if ts, ok := tokenSources.Get(key); ok {
		return ts, nil
	}
	cfg, err := parseServiceAccountKey(secret)
	if err != nil {
		return nil, err
	}
	ts := cfg.TokenSource(exchangeContext())
	tokenSources.Add(key, ts)
	return ts, nil
}

// setGCPBearer puts a live access token on the request.
func setGCPBearer(_ context.Context, p Presentation) error {
	ts, err := tokenSourceFor(p.Secret)
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("upstream: exchanging the service-account key for a token: %w", err)
	}
	tok.SetAuthHeader(p.HTTP)
	return nil
}
