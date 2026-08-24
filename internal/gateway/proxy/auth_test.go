package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

type authFixture struct {
	pool     *pgxpool.Pool
	keyStore *apikeys.Store
	orgStore *organizations.Store
	keys     *apikeys.Service
	auth     *proxy.Authenticator
	mem      cache.Store
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	pool := testpg.Start(t)
	mem, err := cache.NewMemory(pool, 64)
	if err != nil {
		t.Fatal(err)
	}
	return &authFixture{
		pool: pool, keyStore: apikeys.NewStore(pool), orgStore: organizations.New(pool),
		keys: apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: allowKeyAdmin}),
		auth: proxy.NewAuthenticator(apikeys.NewStore(pool), organizations.New(pool), gwdb.New(pool), mem),
		mem:  mem,
	}
}

// allowKeyAdmin: the dataplane tests do not evaluate membership, which belongs
// to the management plane. What this file tests is whether a key authenticates.
func allowKeyAdmin(context.Context, pgtype.UUID, pgtype.UUID) error { return nil }

// org creates one organization and returns its id, for tests that need two keys
// to share one -- which is what an organization-wide limit is measured across.
func (f *authFixture) org(t *testing.T, _ string) pgtype.UUID {
	t.Helper()
	return orgtest.Create(t, f.pool, orgtest.Seed{Name: "o"})
}

// seedKey creates a key, in a fresh org unless the input names one.
//
// It deliberately creates no users or memberships: a zero actor id writes a NULL
// creator. These tests must not depend on tables that only exist in a larger
// deployment, or the package would not run at all where those tables are
// absent.
func (f *authFixture) seedKey(t *testing.T, in apikeys.CreateInput) (string, apikeys.Key, pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	var user pgtype.UUID
	org := in.OrgID
	if !org.Valid {
		org = f.org(t, "")
	}
	in.OrgID, in.ActorID = org, user
	if in.Name == "" {
		in.Name = "k"
	}
	plaintext, row, err := f.keys.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext, row, org
}

func wantCode(t *testing.T, e *proxy.Error, code string) {
	t.Helper()
	if e == nil {
		t.Fatalf("expected error %s, but it passed", code)
	}
	if e.Code != code {
		t.Fatalf("error code = %s, want %s", e.Code, code)
	}
}

// Credential extraction. A pure function that touches no database, so it needs
// no fixture.
func TestCredentialOf(t *testing.T) {
	const key = "sk-flb-v1-real"
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"bearer only", map[string]string{"Authorization": "Bearer " + key}, key},
		{"scheme name is case-insensitive per RFC 7235", map[string]string{"Authorization": "bearer " + key}, key},
		{"x-api-key only, which is the Anthropic SDK default", map[string]string{"x-api-key": key}, key},
		{"both headers agree", map[string]string{"Authorization": "Bearer " + key, "x-api-key": key}, key},
		// Leftovers in the environment, with both an auth token and an API key
		// set: take the standard header and do not error, or the user hits a
		// 401 they cannot diagnose.
		{"headers disagree, Authorization wins", map[string]string{"Authorization": "Bearer " + key, "x-api-key": "sk-flb-v1-other"}, key},
		// Basic supplies no usable credential, so "Authorization wins" does not
		// arise here.
		{"a non-bearer scheme falls back to x-api-key", map[string]string{"Authorization": "Basic abc", "x-api-key": key}, key},
		{"x-api-key is trimmed", map[string]string{"x-api-key": "  " + key + "  "}, key},
		{"neither header present", nil, ""},
		{"bearer with no token and nothing to fall back to", map[string]string{"Authorization": "Bearer "}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := proxy.CredentialOf(r); got != tc.want {
				t.Fatalf("CredentialOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// The main authentication path and each failure branch.
func TestAuthenticate(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, row, org := f.seedKey(t, apikeys.CreateInput{})

	id, e := f.auth.Authenticate(ctx, plaintext)
	if e != nil {
		t.Fatalf("a valid key should pass: %v", e)
	}
	if id.KeyID != row.ID || id.OrgID != org {
		t.Fatalf("identity does not match: %+v", id)
	}

	// Every shape of invalid credential collapses to one code: splitting them
	// only gives an enumerator feedback. Malformed header shapes -- Basic, a
	// missing token -- are covered by TestCredentialOf; they never reach here,
	// because a failed extraction hands over an empty string.
	for _, c := range []string{"", "wrong-prefix", "sk-flb", "sk-flb-v1-nonexistent"} {
		_, e := f.auth.Authenticate(ctx, c)
		wantCode(t, e, errcode.GatewayInvalidApiKey)
	}
}

// Recording last use is throttled to the cache load: written on a miss, not on
// a hit. Three stages: the first authentication must write; a second within the
// TTL, which hits, must not; and after the cache is cleared the next
// authentication must move it forward.
func TestAuthenticateTouchesLastUsedPerCacheLoad(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, row, _ := f.seedKey(t, apikeys.CreateInput{})

	readTS := func() pgtype.Timestamptz {
		t.Helper()
		var ts pgtype.Timestamptz
		if err := f.pool.QueryRow(ctx,
			`SELECT last_used_at FROM api_keys WHERE id = $1`, row.ID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}

	if _, e := f.auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatal(e)
	}
	first := readTS()
	if !first.Valid {
		t.Fatal("last use is still NULL after the first authentication, which was a cache miss -- the writer is not wired up")
	}

	if _, e := f.auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatal(e)
	}
	if second := readTS(); second.Time != first.Time {
		t.Fatalf("a cache hit must not write to the database: %v -> %v", first.Time, second.Time)
	}

	if err := f.mem.Delete(ctx, proxy.KeyCacheKey(crypto.HashToken(plaintext))); err != nil {
		t.Fatal(err)
	}
	if _, e := f.auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatal(e)
	}
	if third := readTS(); !third.Time.After(first.Time) {
		t.Fatalf("authenticating again after the cache was cleared should move last use forward: %v -> %v", first.Time, third.Time)
	}
}

// Revocation takes effect at once: the callback must cut through the cache, or
// a leaked key keeps spending for the rest of the TTL.
func TestRevokeInvalidatesCache(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, row, org := f.seedKey(t, apikeys.CreateInput{})

	// Construct the key service with the same invalidation outlet as production.
	f.keys = apikeys.NewService(apikeys.ServiceConfig{
		Database: f.pool,
		Admin:    allowKeyAdmin,
		Invalidator: func(ctx context.Context, keyHash string) {
			if err := f.mem.Delete(ctx, proxy.KeyCacheKey(keyHash)); err != nil {
				t.Errorf("invalidation failed: %v", err)
			}
		},
	})

	// Authenticate once to load the snapshot into the cache.
	if _, e := f.auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatal(e)
	}

	var owner pgtype.UUID // zero: revocation does not judge the actor; the injected admin check does
	if err := f.keys.Revoke(ctx, org, owner, row.ID); err != nil {
		t.Fatal(err)
	}

	_, e := f.auth.Authenticate(ctx, plaintext)
	wantCode(t, e, errcode.GatewayKeyRevoked)
}

// Expiry with no cache, reading straight from the database -- the
// authenticator's authoritative path.
func TestExpiredKeyRejectedUncached(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, row, _ := f.seedKey(t, apikeys.CreateInput{})
	if _, err := f.pool.Exec(ctx,
		`UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	uncached := proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil)
	_, e := uncached.Authenticate(ctx, plaintext)
	wantCode(t, e, errcode.GatewayKeyExpired)
}

// A suspended org gets a 403 on the dataplane.
func TestSuspendedOrgRejected(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	if _, err := f.pool.Exec(ctx, `UPDATE orgs SET status = 'suspended' WHERE id = $1`, org); err != nil {
		t.Fatal(err)
	}
	uncached := proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil)
	_, e := uncached.Authenticate(ctx, plaintext)
	wantCode(t, e, errcode.GatewayOrgSuspended)
}

// The scope check.
func TestRequireScope(t *testing.T) {
	id := proxy.Identity{Scopes: []string{"inference"}}
	if e := proxy.RequireScope(id, "inference"); e != nil {
		t.Fatalf("should pass: %v", e)
	}
	e := proxy.RequireScope(proxy.Identity{Scopes: []string{"other"}}, "inference")
	wantCode(t, e, errcode.GatewayInsufficientScope)
}

// The cache round trip must not lose the expiry instant -- the classic trap of
// an unexported field not surviving JSON serialisation.
func TestCachedIdentityRetainsExpiry(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	plaintext, row, _ := f.seedKey(t, apikeys.CreateInput{})
	if _, err := f.pool.Exec(ctx, `UPDATE api_keys SET expires_at = $2 WHERE id = $1`, row.ID, future); err != nil {
		t.Fatal(err)
	}

	first, e := f.auth.Authenticate(ctx, plaintext) // fills the cache
	if e != nil {
		t.Fatal(e)
	}
	second, e := f.auth.Authenticate(ctx, plaintext) // hits the cache
	if e != nil {
		t.Fatal(e)
	}
	if second.ExpiresAt.IsZero() {
		t.Fatal("the cache round trip lost the expiry instant -- an expired key would keep working for the rest of the TTL")
	}
	if !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("the expiry instant differs across the cache: %v vs %v", first.ExpiresAt, second.ExpiresAt)
	}
}
