package gwstaffapi_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// A organization that was never configured explicitly reads back as the default tier
// with no discount, and tier_explicit=false lets the UI tell that this is a
// fallback rather than somebody's deliberate choice.
func TestOrgSettingsFallsBackToDefault(t *testing.T) {
	s, pool, _ := newServer(t)
	org := seedOrgPublic(t, pool)

	got := mustGetOrgSettings(t, s, org)
	if got.TierSlug != "default" {
		t.Errorf("with nothing configured it should fall back to the default tier: %s", got.TierSlug)
	}
	if got.TierExplicit || got.RowExists {
		t.Errorf("with nothing configured both explicit flags should be false: %+v", got)
	}
}

// Put handles the access tier only: there is no per-organization discount parameter
// in either direction, because discounts belong to the pricing plan.
func TestOrgSettingsPutChangesAccessOnly(t *testing.T) {
	s, pool, _ := newServer(t)
	org := seedOrgPublic(t, pool)
	vip := mustCreateTier(t, s, "vip")

	got := mustPutOrgSettings(t, s, org, &vip.Id)
	if got.TierSlug != "vip" {
		t.Fatalf("the assignment did not take effect: %+v", got)
	}
	if !got.TierExplicit || !got.RowExists {
		t.Errorf("once configured explicitly both flags should be true: %+v", got)
	}

	// A null tier_id means "back to the default".
	got = mustPutOrgSettings(t, s, org, nil)
	if got.TierSlug != "default" {
		t.Errorf("passing null should return to the default tier: %s", got.TierSlug)
	}
	if got.TierExplicit {
		t.Error("after returning to the default tier, tier_explicit should be false")
	}
	if !got.RowExists {
		t.Error("the access-settings row survives the return to the default tier, so row_exists should be true")
	}
}

// A reason is mandatory: this is an operator action that changes what a organization
// is billed.
func TestOrgSettingsRequiresReason(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	org := seedOrgPublic(t, pool)

	for _, blank := range []string{"", "   "} {
		if _, err := s.PutOrgGatewaySettings(ctx, gwstaffapi.PutOrgGatewaySettingsRequestObject{
			OrgId: org,
			Body: &gwstaffapi.PutOrgGatewaySettingsJSONRequestBody{
				Reason: blank,
			},
		}); err == nil {
			t.Errorf("an empty reason %q should be refused", blank)
		}
	}
}

// A disabled tier cannot be assigned: that would put the organization in a state
// where the configuration looks fine and every request is refused.
func TestOrgSettingsRejectsDisabledTier(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	org := seedOrgPublic(t, pool)
	tier := mustCreateTier(t, s, "sunset")

	disabled := gwstaffapi.GatewayTierInputStatusDisabled
	if _, err := s.UpdateGatewayTier(ctx, gwstaffapi.UpdateGatewayTierRequestObject{
		TierId: tier.Id, Body: &gwstaffapi.GatewayTierInput{Status: &disabled},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.PutOrgGatewaySettings(ctx, gwstaffapi.PutOrgGatewaySettingsRequestObject{
		OrgId: org,
		Body: &gwstaffapi.PutOrgGatewaySettingsJSONRequestBody{
			TierId: &tier.Id, Reason: "probe",
		},
	}); err == nil {
		t.Error("a disabled tier must not be assignable")
	}

	// A nonexistent tier is refused before the write too, rather than hitting
	// the foreign key and surfacing as a 500.
	ghost := uuid.New()
	if _, err := s.PutOrgGatewaySettings(ctx, gwstaffapi.PutOrgGatewaySettingsRequestObject{
		OrgId: org,
		Body: &gwstaffapi.PutOrgGatewaySettingsJSONRequestBody{
			TierId: &ghost, Reason: "probe",
		},
	}); err == nil {
		t.Error("a nonexistent tier must not be assignable")
	}
}

// The central behaviour: after a tier or discount change, the data-plane cache
// for every active key of that org is cleared immediately.
//
// Skipping it means up to one cache TTL of requests still admitted and priced
// on the old values -- a stretch of time in which the operator believes the
// change landed while the bill says otherwise, with no signal either way.
func TestOrgSettingsInvalidatesKeyCache(t *testing.T) {
	s, pool, brk := newServer(t)
	ctx := context.Background()
	org := seedOrgPublic(t, pool)
	orgUUID, err := publicid.Parse(publicid.Org, org)
	if err != nil {
		t.Fatal(err)
	}

	mem, err := cache.NewMemory(pool, 128)
	if err != nil {
		t.Fatal(err)
	}
	s = serverForPool(t, pool, brk, func(cfg *gwstaffapi.ServerConfig) {
		cfg.Cache = mem
	})

	// Two active keys plus one revoked.
	active := []string{"hash-a", "hash-b"}
	for _, h := range active {
		seedKey(t, pool, publicid.UUIDString(orgUUID), h, "active")
	}
	seedKey(t, pool, publicid.UUIDString(orgUUID), "hash-revoked", "revoked")

	// Another org's key must not be swept up.
	otherOrg := seedOrgPublic(t, pool)
	otherUUID, _ := publicid.Parse(publicid.Org, otherOrg)
	seedKey(t, pool, publicid.UUIDString(otherUUID), "hash-other", "active")

	all := append(append([]string{}, active...), "hash-revoked", "hash-other")
	for _, h := range all {
		if err := mem.Set(ctx, proxy.KeyCacheKey(h), []byte(`{}`), 0); err != nil {
			t.Fatal(err)
		}
	}

	vip := mustCreateTier(t, s, "cache-vip")
	mustPutOrgSettings(t, s, org, &vip.Id)

	for _, h := range active {
		if _, ok, _ := mem.Get(ctx, proxy.KeyCacheKey(h)); ok {
			t.Errorf("the cache for active key %s should have been cleared -- otherwise the new discount waits for the TTL", h)
		}
	}
	// The other org is unaffected: invalidation is scoped to this one org.
	if _, ok, _ := mem.Get(ctx, proxy.KeyCacheKey("hash-other")); !ok {
		t.Error("another org's key cache was cleared by mistake")
	}
}

// ===== Fixtures =====

// seedOrgPublic creates an org and returns its prefixed public ID, which is
// what the endpoint takes -- not a bare UUID.
func seedOrgPublic(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return publicid.Format(publicid.Org, orgtest.Create(t, pool, orgtest.Seed{Name: "o"}))
}

// seedKey inserts a key directly rather than going through the key service:
// what these tests need is a hash present in the database, not a full issuance
// flow.
func seedKey(t *testing.T, pool *pgxpool.Pool, orgUUID, hash, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (org_id, name, prefix, key_hash, status)
		 VALUES ($1::uuid, $2, 'sk-flb-v1-1', $3, $4)`,
		orgUUID, "k-"+hash, hash, status); err != nil {
		t.Fatal(err)
	}
}

func mustGetOrgSettings(t *testing.T, s *gwstaffapi.Server, org string) gwstaffapi.OrgGatewaySettings {
	t.Helper()
	res, err := s.GetOrgGatewaySettings(context.Background(),
		gwstaffapi.GetOrgGatewaySettingsRequestObject{OrgId: org})
	if err != nil {
		t.Fatalf("read organization access settings: %v", err)
	}
	return gwstaffapi.OrgGatewaySettings(res.(gwstaffapi.GetOrgGatewaySettings200JSONResponse))
}

func mustPutOrgSettings(
	t *testing.T, s *gwstaffapi.Server, org string, tier *uuid.UUID,
) gwstaffapi.OrgGatewaySettings {
	t.Helper()
	res, err := s.PutOrgGatewaySettings(context.Background(),
		gwstaffapi.PutOrgGatewaySettingsRequestObject{
			OrgId: org,
			Body: &gwstaffapi.PutOrgGatewaySettingsJSONRequestBody{
				TierId: tier, Reason: "test case",
			},
		})
	if err != nil {
		t.Fatalf("write organization access settings: %v", err)
	}
	return gwstaffapi.OrgGatewaySettings(res.(gwstaffapi.PutOrgGatewaySettings200JSONResponse))
}
