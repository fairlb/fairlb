package gwstaffapi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The default tier seeded by the migration must be listable, and it must be the
// one tier that admits everything -- a fresh install has to serve rather than
// refuse. What says so is allow_all_models; model_count is zero either way and
// on its own means nothing.
func TestTierListSeedsDefault(t *testing.T) {
	s, _, _ := newServer(t)

	tiers := mustListTiers(t, s)
	if len(tiers) != 1 {
		t.Fatalf("initially there should be only the default tier: %d", len(tiers))
	}
	d := tiers[0]
	if d.Slug != "default" || !d.IsDefault {
		t.Fatalf("the default tier has the wrong shape: %+v", d)
	}
	if !d.AllowAllModels {
		t.Error("the seeded default tier must admit every model, or a fresh install refuses every request")
	}
	if d.ModelCount != 0 || d.OrgCount != 0 {
		t.Errorf("an allow-all tier lists nothing and starts with no members: models=%d orgs=%d",
			d.ModelCount, d.OrgCount)
	}
}

// Tier CRUD end to end, plus whole-set replacement semantics.
func TestTierCRUDAndModelSet(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	tier := mustCreateTier(t, s, "vip")
	if tier.IsDefault {
		t.Error("a newly created tier must not become the default on its own")
	}

	// A duplicate slug is a 409, not a 500.
	if _, err := s.CreateGatewayTier(ctx, gwstaffapi.CreateGatewayTierRequestObject{
		Body: &gwstaffapi.GatewayTierInput{Slug: strPtrT("vip")},
	}); err == nil {
		t.Error("a duplicate slug should be refused")
	}

	// The slug is the stable identifier organisations are assigned by, so it
	// cannot be changed.
	if _, err := s.UpdateGatewayTier(ctx, gwstaffapi.UpdateGatewayTierRequestObject{
		TierId: tier.Id,
		Body:   &gwstaffapi.GatewayTierInput{Slug: strPtrT("vip2")},
	}); err == nil {
		t.Error("the slug must not be changeable")
	}

	// Partial update: renaming must not disturb status.
	//
	// The new name is deliberately non-ASCII: it is compared byte for byte
	// against what comes back, so this assertion doubles as the round trip of
	// a multi-byte name through JSON, pgx and back. Replacing it with an
	// ASCII name would keep the test green while quietly dropping that.
	name := "Премиум"
	updated, err := s.UpdateGatewayTier(ctx, gwstaffapi.UpdateGatewayTierRequestObject{
		TierId: tier.Id, Body: &gwstaffapi.GatewayTierInput{Name: &name},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := gwstaffapi.GatewayTier(updated.(gwstaffapi.UpdateGatewayTier200JSONResponse))
	if got.Name == nil || *got.Name != name {
		t.Errorf("the rename did not take effect: %v", got.Name)
	}
	if got.Status != "active" {
		t.Errorf("renaming alone must not also change status: %s", got.Status)
	}

	// Whole-set replacement: the PUT body is the complete set.
	m1 := mustModel(t, s, "openai/tier-a")
	m2 := mustModel(t, s, "openai/tier-b")

	set := mustSetTierModels(t, s, tier.Id, []uuid.UUID{m1.Id, m2.Id})
	if len(set) != 2 {
		t.Fatalf("2 models should be admitted: %d", len(set))
	}

	// PUT a single-element set: the other one is removed, because this
	// replaces rather than appends.
	set = mustSetTierModels(t, s, tier.Id, []uuid.UUID{m1.Id})
	if len(set) != 1 || set[0].Id != m1.Id {
		t.Fatalf("whole-set replacement semantics are wrong, only m1 should remain: %+v", set)
	}

	// Duplicate ids are deduplicated.
	set = mustSetTierModels(t, s, tier.Id, []uuid.UUID{m1.Id, m1.Id, m2.Id})
	if len(set) != 2 {
		t.Errorf("duplicate ids should deduplicate to 2: %d", len(set))
	}

	// The empty set means unrestricted -- a legitimate state, not an error.
	if set = mustSetTierModels(t, s, tier.Id, []uuid.UUID{}); len(set) != 0 {
		t.Errorf("an empty set should be accepted (= unrestricted): %d", len(set))
	}

	// A nonexistent model id is a 400, not a 500.
	if _, err := s.SetGatewayTierModels(ctx, gwstaffapi.SetGatewayTierModelsRequestObject{
		TierId: tier.Id,
		Body:   &gwstaffapi.SetGatewayTierModelsJSONRequestBody{ModelIds: []uuid.UUID{uuid.New()}},
	}); err == nil {
		t.Error("a nonexistent model_id should be refused")
	}

	// A non-default tier with no members can be deleted.
	if _, err := s.DeleteGatewayTier(ctx, gwstaffapi.DeleteGatewayTierRequestObject{
		TierId: tier.Id,
	}); err != nil {
		t.Fatalf("a non-default tier with no members should be deletable: %v", err)
	}
}

// All three default-tier guards must produce an actionable error at the API
// layer rather than surfacing the raw text of a database constraint.
func TestTierDefaultGuards(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	tiers := mustListTiers(t, s)
	def := tiers[0]

	// The default tier cannot be deleted.
	_, err := s.DeleteGatewayTier(ctx, gwstaffapi.DeleteGatewayTierRequestObject{TierId: def.Id})
	if err == nil {
		t.Fatal("the default tier must not be deletable")
	}
	// Anchored on "default": what this asserts is that the message explains
	// *why* the delete was refused. It used to match a Chinese fragment and
	// went red the day the detail was translated — which is the correct
	// behaviour for an assertion of this kind, not a fault in it.
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("the error should say the reason is that it is the default tier: %v", err)
	}

	// The default tier cannot be disabled.
	disabled := gwstaffapi.GatewayTierInputStatusDisabled
	if _, err := s.UpdateGatewayTier(ctx, gwstaffapi.UpdateGatewayTierRequestObject{
		TierId: def.Id, Body: &gwstaffapi.GatewayTierInput{Status: &disabled},
	}); err == nil {
		t.Error("the default tier must not be disablable")
	}

	// Moving the default: the new tier takes over and the old one steps
	// aside, so there is still exactly one.
	vip := mustCreateTier(t, s, "vip")
	res, err := s.SetDefaultGatewayTier(ctx, gwstaffapi.SetDefaultGatewayTierRequestObject{
		TierId: vip.Id,
	})
	if err != nil {
		t.Fatalf("switching the default tier failed: %v", err)
	}
	if !gwstaffapi.GatewayTier(res.(gwstaffapi.SetDefaultGatewayTier200JSONResponse)).IsDefault {
		t.Error("after the switch the new tier should be the default")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_tiers WHERE is_default`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("there should be exactly one global default tier: %d", n)
	}
	// Having stepped aside, the old tier can now be disabled.
	if _, err := s.UpdateGatewayTier(ctx, gwstaffapi.UpdateGatewayTierRequestObject{
		TierId: def.Id, Body: &gwstaffapi.GatewayTierInput{Status: &disabled},
	}); err != nil {
		t.Errorf("the old tier that stepped aside should be disablable: %v", err)
	}
	// And a disabled tier cannot be made the default again, which would
	// refuse service to every organization without an explicit tier.
	if _, err := s.SetDefaultGatewayTier(ctx, gwstaffapi.SetDefaultGatewayTierRequestObject{
		TierId: def.Id,
	}); err == nil {
		t.Error("a disabled tier must not be settable as the default")
	}
	// A refused move must not clear the current default: otherwise a rejected
	// operation leaves the system with no default at all.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_tiers WHERE is_default`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after a failed switch there should still be exactly one default tier (the transaction must roll ClearDefault back): %d", n)
	}
}

// A tier with members cannot be deleted, and the error has to carry the member
// count: "cannot delete" alone does not tell anyone how many to move.
func TestTierDeleteReportsMemberCount(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	tier := mustCreateTier(t, s, "enterprise")
	for range 2 {
		org := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Name: "o"}))
		if _, err := pool.Exec(ctx,
			`INSERT INTO org_gateway_settings (org_id, tier_id) VALUES ($1,$2)`, org, tier.Id); err != nil {
			t.Fatal(err)
		}
	}

	_, err := s.DeleteGatewayTier(ctx, gwstaffapi.DeleteGatewayTierRequestObject{TierId: tier.Id})
	if err == nil {
		t.Fatal("a tier with members must not be deletable")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("the error should carry the member count, or the operator does not know how many to migrate: %v", err)
	}

	// The member count in the list has to agree.
	for _, tr := range mustListTiers(t, s) {
		if tr.Id == tier.Id && tr.OrgCount != 2 {
			t.Errorf("the member count should be 2: %d", tr.OrgCount)
		}
	}
}

// ===== Fixtures =====

func strPtrT(s string) *string { return &s }

func mustListTiers(t *testing.T, s *gwstaffapi.Server) []gwstaffapi.GatewayTier {
	t.Helper()
	res, err := s.ListGatewayTiers(context.Background(), gwstaffapi.ListGatewayTiersRequestObject{})
	if err != nil {
		t.Fatalf("list access tiers: %v", err)
	}
	return res.(gwstaffapi.ListGatewayTiers200JSONResponse).Items
}

func mustCreateTier(t *testing.T, s *gwstaffapi.Server, slug string) gwstaffapi.GatewayTier {
	t.Helper()
	res, err := s.CreateGatewayTier(context.Background(), gwstaffapi.CreateGatewayTierRequestObject{
		Body: &gwstaffapi.GatewayTierInput{Slug: &slug},
	})
	if err != nil {
		t.Fatalf("create access tier: %v", err)
	}
	return gwstaffapi.GatewayTier(res.(gwstaffapi.CreateGatewayTier201JSONResponse))
}

func mustSetTierModels(
	t *testing.T, s *gwstaffapi.Server, tier uuid.UUID, ids []uuid.UUID,
) []gwstaffapi.GatewayTierModel {
	t.Helper()
	res, err := s.SetGatewayTierModels(context.Background(),
		gwstaffapi.SetGatewayTierModelsRequestObject{
			TierId: tier,
			Body:   &gwstaffapi.SetGatewayTierModelsJSONRequestBody{ModelIds: ids},
		})
	if err != nil {
		t.Fatalf("set the models in the tier: %v", err)
	}
	return res.(gwstaffapi.SetGatewayTierModels200JSONResponse).Items
}
