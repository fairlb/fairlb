package proxy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The key's model gate: unrestricted lets everything through, a list lets
// through only what is on it, and an empty list lets nothing through. A refusal
// uses model_not_found rather than forbidden -- do not confirm the existence of
// a model the caller is not allowed to use.
func TestGuardModelAllowlist(t *testing.T) {
	f := newAuthFixture(t)
	g := proxy.NewGuard(f.keyStore, nil)

	if e := g.CheckModel(proxy.Identity{AllowAllModels: true}, "openai/gpt-5.4"); e != nil {
		t.Fatalf("an unrestricted key should let it through: %v", e)
	}

	id := proxy.Identity{AllowedModels: []string{"openai/gpt-5.4"}}
	if e := g.CheckModel(id, "openai/gpt-5.4"); e != nil {
		t.Fatalf("a model on the list should be let through: %v", e)
	}
	wantCode(t, g.CheckModel(id, "anthropic/claude"), errcode.GatewayModelNotFound)
}

// An empty allowlist refuses everything. This is the case the old jsonb shape
// could not express at all: emptiness meant "unrestricted", so a key whose last
// model was removed silently gained the whole catalogue.
func TestGuardEmptyAllowlistRefusesEverything(t *testing.T) {
	f := newAuthFixture(t)
	g := proxy.NewGuard(f.keyStore, nil)
	id := proxy.Identity{AllowedModels: []string{}}
	wantCode(t, g.CheckModel(id, "openai/gpt-5.4"), errcode.GatewayModelNotFound)
}

// The model gate survives the identity cache's JSON round trip. It is two
// fields now, and a snapshot that lost either of them would read as a different
// policy than the one that was stored.
func TestModelGateSurvivesTheIdentityCache(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, row, _ := f.seedKey(t, apikeys.CreateInput{
		Name:        "gated",
		ModelAccess: apikeys.ModelAccess{Models: []string{"openai/gpt-5.4"}},
	})
	_ = row

	auth := proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), f.mem)
	// The first call loads from the database and writes the snapshot; the
	// second reads it back.
	if _, e := auth.Authenticate(ctx, plaintext); e != nil {
		t.Fatalf("first authentication failed: %v", e)
	}
	id, e := auth.Authenticate(ctx, plaintext)
	if e != nil {
		t.Fatalf("second authentication failed: %v", e)
	}
	if id.AllowAllModels {
		t.Error("a restricted key must not come back out of the cache unrestricted")
	}
	if len(id.AllowedModels) != 1 || id.AllowedModels[0] != "openai/gpt-5.4" {
		t.Errorf("the allowlist did not survive the cache: %+v", id.AllowedModels)
	}
}

// The three budget periods: total reads the denormalised column, daily and
// monthly sum the per-day table.
func TestGuardBudget(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	g := proxy.NewGuard(f.keyStore, nil)

	// No budget configured means no limit.
	if e := g.CheckBudget(ctx, proxy.Identity{}); e != nil {
		t.Fatalf("no budget should let it through: %v", e)
	}

	// total: decided from the running column.
	_, row, _ := f.seedKey(t, apikeys.CreateInput{})
	base := proxy.Identity{KeyID: row.ID, SpendLimitNano: 1000, SpendLimitInterval: "total"}
	if e := g.CheckBudget(ctx, base); e != nil {
		t.Fatalf("under budget should be let through: %v", e)
	}
	spent := base
	spent.TotalSpentNano = 1000
	wantCode(t, g.CheckBudget(ctx, spent), errcode.GatewayKeyBudgetExceeded)

	// daily: summed from the per-day table; today's spend reaching the limit
	// refuses.
	daily := proxy.Identity{KeyID: row.ID, SpendLimitNano: 500, SpendLimitInterval: "daily"}
	if e := g.CheckBudget(ctx, daily); e != nil {
		t.Fatalf("nothing spent today should be let through: %v", e)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO api_key_daily_spend (api_key_id, day, spent_nano)
		 VALUES ($1, (now() AT TIME ZONE 'UTC')::date, 500)`, row.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, g.CheckBudget(ctx, daily), errcode.GatewayKeyBudgetExceeded)

	// The monthly window is wider: the same spend today also counts this month.
	monthly := proxy.Identity{KeyID: row.ID, SpendLimitNano: 500, SpendLimitInterval: "monthly"}
	wantCode(t, g.CheckBudget(ctx, monthly), errcode.GatewayKeyBudgetExceeded)

	// Last month's spend does not count against this month's budget.
	monthlyLoose := proxy.Identity{KeyID: row.ID, SpendLimitNano: 100000, SpendLimitInterval: "monthly"}
	if e := g.CheckBudget(ctx, monthlyLoose); e != nil {
		t.Fatalf("below the limit should be let through: %v", e)
	}
}

// RPM and TPM. TPM is consumed in proportion to the estimated tokens.
func TestGuardRateLimits(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	lim := ratelimit.NewMemory()
	g := proxy.NewGuard(f.keyStore, lim)

	_, row, _ := f.seedKey(t, apikeys.CreateInput{})
	id := proxy.Identity{KeyID: row.ID, RateLimitRPM: 2}

	for i := range 2 {
		if e := g.CheckRate(ctx, id, 0); e != nil {
			t.Fatalf("request %d should be let through: %v", i+1, e)
		}
	}
	e := g.CheckRate(ctx, id, 0)
	wantCode(t, e, errcode.GatewayRateLimited)
	if e.RetryAfter <= 0 {
		t.Fatal("a 429 must carry Retry-After")
	}

	// TPM: a limit of 100 and an estimate of 60 means two requests exceed it.
	_, row2, _ := f.seedKey(t, apikeys.CreateInput{Name: "k2"})
	tpmID := proxy.Identity{KeyID: row2.ID, RateLimitTPM: 100}
	if e := g.CheckRate(ctx, tpmID, 60); e != nil {
		t.Fatalf("the first 60 of 100 should be let through: %v", e)
	}
	wantCode(t, g.CheckRate(ctx, tpmID, 60), errcode.GatewayRateLimited)

	// An estimate of zero consumes no TPM allowance.
	_, row3, _ := f.seedKey(t, apikeys.CreateInput{Name: "k3"})
	zeroID := proxy.Identity{KeyID: row3.ID, RateLimitTPM: 1}
	for range 3 {
		if e := g.CheckRate(ctx, zeroID, 0); e != nil {
			t.Fatalf("an estimate of zero must not consume TPM: %v", e)
		}
	}
}

// Both levels are measured, and the organization's goes first.
//
// The ordering is the point: with the organization at its ceiling, the key's
// own allowance must still be intact afterwards. A doomed request that ate the
// key's quota on the way to being refused would make one organization's overuse
// spend a second organization's -- or a second application's -- budget.
func TestGuardOrgRateLimitIsCheckedBeforeTheKeys(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	lim := ratelimit.NewMemory()
	g := proxy.NewGuard(f.keyStore, lim)

	org := f.org(t, "rate-limited-org")
	_, row, _ := f.seedKey(t, apikeys.CreateInput{Name: "k1", OrgID: org})
	id := proxy.Identity{
		KeyID: row.ID, OrgID: org,
		OrgRateLimitRPM: 1,
		RateLimitRPM:    5,
	}
	if e := g.CheckRate(ctx, id, 0); e != nil {
		t.Fatalf("the first request should be let through: %v", e)
	}
	e := g.CheckRate(ctx, id, 0)
	wantCode(t, e, errcode.GatewayRateLimited)
	// The message names which ceiling was hit: "rate limit exceeded" with no
	// subject leaves a organization unable to tell "this key is small" from "the
	// whole account is at its ceiling", and those have different fixes.
	if !strings.Contains(e.Message, "Organization") {
		t.Errorf("the refusal must name the level it came from, got %q", e.Message)
	}

	// The key's own allowance was not spent by the refused request: a second
	// key on the same org, under the same ceiling, still has its four left.
	_, row2, _ := f.seedKey(t, apikeys.CreateInput{Name: "k2", OrgID: org})
	other := proxy.Identity{KeyID: row2.ID, OrgID: org, RateLimitRPM: 5}
	for i := range 5 {
		if e := g.CheckRate(ctx, other, 0); e != nil {
			t.Fatalf("the org ceiling must not have consumed key %d's allowance: %v", i+1, e)
		}
	}
}

// The organization's token ceiling is measured the same way, against the same
// estimate the key's is.
func TestGuardOrgTokenLimit(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	g := proxy.NewGuard(f.keyStore, ratelimit.NewMemory())

	org := f.org(t, "token-limited-org")
	_, row, _ := f.seedKey(t, apikeys.CreateInput{Name: "k1", OrgID: org})
	id := proxy.Identity{KeyID: row.ID, OrgID: org, OrgRateLimitTPM: 100}
	if e := g.CheckRate(ctx, id, 60); e != nil {
		t.Fatalf("the first 60 of 100 should be let through: %v", e)
	}
	e := g.CheckRate(ctx, id, 60)
	wantCode(t, e, errcode.GatewayRateLimited)
	if !strings.Contains(e.Message, "Organization") {
		t.Errorf("the refusal must name the level it came from, got %q", e.Message)
	}
}

// A broken rate-limiter driver does not block the request: a capacity gate
// fails open, and the security gates -- allowlist, budget -- ran earlier.
func TestGuardRateLimiterFailureIsOpen(t *testing.T) {
	f := newAuthFixture(t)
	g := proxy.NewGuard(f.keyStore, brokenLimiter{})
	_, row, _ := f.seedKey(t, apikeys.CreateInput{})
	id := proxy.Identity{KeyID: row.ID, RateLimitRPM: 1, RateLimitTPM: 1}
	if e := g.CheckRate(context.Background(), id, 10); e != nil {
		t.Fatalf("a broken rate-limiter driver should let it through: %v", e)
	}
}

type brokenLimiter struct{}

func (brokenLimiter) Allow(context.Context, string, int, time.Duration) (ratelimit.Result, error) {
	return ratelimit.Result{}, errContext
}

func (brokenLimiter) AllowN(context.Context, string, int, int, time.Duration) (ratelimit.Result, error) {
	return ratelimit.Result{}, errContext
}

var errContext = &driverErr{}

type driverErr struct{}

func (*driverErr) Error() string { return "driver down" }

// An entirely unusable admission tier fails closed.
//
// Both cases refuse and both use model_tier_disabled: the tier is disabled, and
// there is no default tier at all. It deliberately does *not* fall back to the
// default -- falling back would mean the organization keeps spending under an
// admission policy and a price list they do not know about. The code is kept
// apart from model_not_found because here every model is refused: reusing 404
// would show the organization "all the models suddenly vanished" and leave them
// guessing.
func TestGuardTierFailsClosed(t *testing.T) {
	f := newAuthFixture(t)
	g := proxy.NewGuard(f.keyStore, nil)

	active := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	if e := g.CheckTier(proxy.Identity{ModelTierID: active, ModelTierStatus: "active"}); e != nil {
		t.Fatalf("an active tier should be let through: %v", e)
	}

	wantCode(t, g.CheckTier(proxy.Identity{
		ModelTierID: active, ModelTierStatus: "disabled",
	}), errcode.GatewayModelTierDisabled)

	// The tier cannot be resolved at all, with no default tier in the
	// database: also refused, never let through as "unrestricted".
	wantCode(t, g.CheckTier(proxy.Identity{
		ModelTierID: pgtype.UUID{}, ModelTierStatus: "",
	}), errcode.GatewayModelTierDisabled)
}

// The Identity must carry the effective admission tier when it is loaded.
// Without it, CheckTier judges every request as "tier unresolvable" and
// magnifies one configuration-read problem into a deployment-wide outage.
func TestAuthLoadsEffectiveTier(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	plaintext, _, _ := f.seedKey(t, apikeys.CreateInput{})

	uncached := proxy.NewAuthenticator(f.keyStore, f.orgStore, gwdb.New(f.pool), nil)
	id, e := uncached.Authenticate(ctx, plaintext)
	if e != nil {
		t.Fatalf("authentication failed: %v", e)
	}
	// Never explicitly configured, so it falls back to the default tier that a
	// migration seeds, and that tier is active.
	if !id.ModelTierID.Valid {
		t.Fatal("an org with nothing configured should fall back to the default tier, not get an empty one")
	}
	if id.ModelTierStatus != "active" {
		t.Errorf("the default tier should be active: %q", id.ModelTierStatus)
	}
	if e := proxy.NewGuard(f.keyStore, nil).CheckTier(id); e != nil {
		t.Errorf("an identity on the default tier should pass the gate: %v", e)
	}
}
