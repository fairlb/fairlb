package apikeys_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
)

// Self-service editing of a key's control fields. The gate has always enforced
// the model allowlist and the token limit; what was missing was a way to set
// them without an operator editing the database.

// Changing them must broadcast a cache invalidation: the data plane's cached
// identity carries the model gate, so without it a newly set allowlist would
// not apply until the cache expired. These are security semantics, and
// "takes effect in a little while" is not acceptable for them.
func TestUpdateInvalidatesKeyCache(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	var invalidated []string
	f.svc = apikeys.NewService(apikeys.ServiceConfig{
		Database: f.pool,
		Admin:    allowKeyAdmin,
		Invalidator: func(_ context.Context, hash string) {
			invalidated = append(invalidated, hash)
		},
	})

	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})
	rpm := int32(60)
	if _, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, RateLimitRpm: &rpm,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.q.RecordByOrg(context.Background(), key.ID, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidated) != 1 || invalidated[0] != stored.KeyHash {
		t.Errorf("changing the controls should invalidate that key's cache entry, got %v", invalidated)
	}
}

// The allowlist and the token limit survive a round trip; those are exactly the
// two the gate consumes.
func TestUpdatePersistsModelAccessAndTokenLimit(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})

	tpm := int32(120000)
	row, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID,
		ModelAccess:  &apikeys.ModelAccess{Models: []string{"openai/gpt-4o", "anthropic/claude"}},
		RateLimitTpm: &tpm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ModelAccess.AllowAll {
		t.Error("supplying an allowlist should turn the unrestricted flag off")
	}
	if len(row.ModelAccess.Models) != 2 {
		t.Errorf("the allowlist was not stored correctly: %+v", row.ModelAccess.Models)
	}
	if row.RateLimitTpm == nil || *row.RateLimitTpm != tpm {
		t.Errorf("the token limit was not stored correctly: %+v", row.RateLimitTpm)
	}
}

// An empty allowlist is a value, not an absence: it refuses every model. Read
// back as "unrestricted" it would do the exact opposite of what was asked, and
// nothing about the stored row would show it.
func TestUpdateEmptyAllowlistDeniesEverything(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})

	row, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID,
		ModelAccess: &apikeys.ModelAccess{Models: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ModelAccess.AllowAll || len(row.ModelAccess.Models) != 0 {
		t.Errorf("an empty allowlist must stay an empty allowlist, got allow_all=%v models=%+v",
			row.ModelAccess.AllowAll, row.ModelAccess.Models)
	}
}

// Turning the unrestricted flag on clears the list, so a stored non-empty list
// always means a restricted key. The database CHECK says the same thing; this
// witnesses that the write path does not have to be told twice.
func TestUpdateAllowAllClearsTheList(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	ctx := context.Background()
	key := f.mkKey(t, org, actor, apikeys.CreateInput{
		Name:        "k1",
		ModelAccess: apikeys.ModelAccess{Models: []string{"openai/gpt-4o"}},
	})
	row, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID,
		ModelAccess: &apikeys.ModelAccess{AllowAll: true, Models: []string{"openai/gpt-4o"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.ModelAccess.AllowAll || len(row.ModelAccess.Models) != 0 {
		t.Errorf("allow-all should store an empty list, got allow_all=%v models=%+v",
			row.ModelAccess.AllowAll, row.ModelAccess.Models)
	}
}

// Not mentioning the model gate leaves it alone. An omitted field must not be
// read as "unrestricted": every unrelated edit would then quietly widen the key.
func TestUpdateOmittedModelAccessIsUnchanged(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{
		Name:        "k1",
		ModelAccess: apikeys.ModelAccess{Models: []string{"openai/gpt-4o"}},
	})
	rpm := int32(7)
	row, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, RateLimitRpm: &rpm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ModelAccess.AllowAll || len(row.ModelAccess.Models) != 1 {
		t.Errorf("an unmentioned model gate should keep its value, got allow_all=%v models=%+v",
			row.ModelAccess.AllowAll, row.ModelAccess.Models)
	}
}

// A key may not be pointed at a model its org cannot reach. Left unchecked the
// entry is not an error, just dead: it resolves to 404 at call time while the
// key reads as configured for it.
func TestUpdateRefusesModelsOutsideTheOrgTier(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	f.svc = apikeys.NewService(apikeys.ServiceConfig{
		Database: f.pool,
		Admin:    allowKeyAdmin,
		ModelAdmission: func(_ context.Context, _ pgtype.UUID, slugs []string) ([]string, error) {
			return slugs, nil // nothing is admitted
		},
	})
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})
	_, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID,
		ModelAccess: &apikeys.ModelAccess{Models: []string{"openai/gpt-4o"}},
	})
	if err == nil {
		t.Fatal("a model the org cannot reach should be refused")
	}
	if !strings.Contains(err.Error(), "openai/gpt-4o") {
		t.Errorf("the message has to name the offending model, got %v", err)
	}
}

// Not supplying a field keeps it; only the clear flag removes it. The two mean
// opposite things and must stay distinguishable.
func TestUpdateClearVsOmit(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	limit := int64(5_000_000_000)
	interval := "monthly"
	rpm := int32(30)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{
		Name:               "k1",
		SpendLimitNano:     pgtype.Int8{Int64: limit, Valid: true},
		SpendLimitInterval: pgtype.Text{String: interval, Valid: true},
		RateLimitRpm:       pgtype.Int4{Int32: rpm, Valid: true},
	})
	ctx := context.Background()

	// Changing only the rate limit leaves the budget untouched.
	newRPM := int32(99)
	row, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, RateLimitRpm: &newRPM,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.SpendLimitNano == nil || *row.SpendLimitNano != limit {
		t.Errorf("an unmentioned budget should keep its value, got %+v", row.SpendLimitNano)
	}
	if row.RateLimitRpm == nil || *row.RateLimitRpm != newRPM {
		t.Errorf("the rate limit should now be %d, got %+v", newRPM, row.RateLimitRpm)
	}

	// Clearing one rate ceiling does not touch the other: they are separate
	// entries in the clear list precisely so that they can be.
	newTPM := int32(1000)
	if _, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, RateLimitTpm: &newTPM,
	}); err != nil {
		t.Fatal(err)
	}
	row, err = f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, ClearRateLimitTpm: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RateLimitTpm != nil {
		t.Errorf("the token ceiling should be cleared, got %+v", row.RateLimitTpm)
	}
	if row.RateLimitRpm == nil || *row.RateLimitRpm != newRPM {
		t.Errorf("clearing the token ceiling must not touch the request ceiling, got %+v", row.RateLimitRpm)
	}

	// Explicitly clearing the budget restores "unlimited" and clears the period
	// with it; an orphaned period means nothing.
	row, err = f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, ClearSpendLimit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.SpendLimitNano != nil || row.SpendLimitInterval != nil {
		t.Errorf("after clearing, both the budget and its period should be empty, got %+v / %+v",
			row.SpendLimitNano, row.SpendLimitInterval)
	}
	if row.RateLimitRpm == nil || *row.RateLimitRpm != newRPM {
		t.Errorf("clearing the budget must not touch the rate limit, got %+v", row.RateLimitRpm)
	}
}

// An amount with no period is refused: a spend cap without a window cannot be
// enforced.
func TestUpdateRejectsLimitWithoutInterval(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})
	limit := int64(1_000)
	if _, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, SpendLimitNano: &limit,
	}); err == nil {
		t.Error("an amount with no period should be refused")
	}
}

// A revoked key cannot be edited; changing controls must not resurrect it.
func TestUpdateRejectsRevokedKey(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	ctx := context.Background()
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})
	if err := f.svc.Revoke(ctx, org, actor, key.ID); err != nil {
		t.Fatal(err)
	}
	rpm := int32(10)
	if _, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, RateLimitRpm: &rpm,
	}); err == nil {
		t.Error("a revoked key should not be editable")
	}
}

// Another org's key cannot be edited, even knowing its id.
func TestUpdateRejectsCrossOrg(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})
	other := f.org(t, "other-org")
	rpm := int32(10)
	if _, err := f.svc.Update(context.Background(), apikeys.UpdateInput{
		OrgID: other, ActorID: actor, KeyID: key.ID, RateLimitRpm: &rpm,
	}); err == nil {
		t.Error("editing another org's key should fail")
	}
}

// The expiry can be set and cleared.
func TestUpdateExpiry(t *testing.T) {
	f, org, actor := newUpdateFixture(t)
	ctx := context.Background()
	key := f.mkKey(t, org, actor, apikeys.CreateInput{Name: "k1"})

	at := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	row, err := f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, ExpiresAt: &at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ExpiresAt == nil || !row.ExpiresAt.Equal(at) {
		t.Errorf("the expiry should be %v, got %+v", at, row.ExpiresAt)
	}

	row, err = f.svc.Update(ctx, apikeys.UpdateInput{
		OrgID: org, ActorID: actor, KeyID: key.ID, ClearExpires: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ExpiresAt != nil {
		t.Errorf("after clearing, the expiry should be empty, got %+v", row.ExpiresAt)
	}
}

// ===== Fixtures =====

// newUpdateFixture creates an org with an actor and returns the triple the
// tests use directly.
func newUpdateFixture(t *testing.T) (*fixture, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	f := newFixture(t)
	org := f.org(t, "u-"+strings.ToLower(t.Name()))
	return f, org, actor
}

func (f *fixture) mkKey(t *testing.T, org, actor pgtype.UUID, in apikeys.CreateInput) apikeys.Key {
	t.Helper()
	in.OrgID, in.ActorID = org, actor
	_, row, err := f.svc.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return row
}
