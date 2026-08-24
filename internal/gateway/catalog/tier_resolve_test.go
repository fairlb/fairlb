package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// Access-tier filtering, the middle of the three admission layers.
//
// A tier says what it admits in one of two ways: everything, or exactly what it
// lists. Both have to work, and the difference between them must not be
// inferred from how many models happen to be listed -- inferring it is what
// made "the last model was removed from this tier" mean "this tier now admits
// the whole catalogue".
func TestResolveTierFiltering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p-tier", "openai")
	allowed := f.model(t, "openai/allowed")
	denied := f.model(t, "openai/denied")
	f.route(t, allowed, prov, "up-allowed", []string{"chat"})
	f.route(t, denied, prov, "up-denied", []string{"chat"})

	// A tier that admits everything does so without listing anything.
	open := f.tier(t, "open", true)
	for _, slug := range []string{"openai/allowed", "openai/denied"} {
		if _, err := f.svc.Resolve(ctx, slug, catalog.SurfaceChat, open); err != nil {
			t.Fatalf("an allow-all tier should not restrict anything, yet %s was refused: %v", slug, err)
		}
	}

	// A restricting tier that lists nothing admits nothing. This is the state
	// the old shape could not express at all.
	empty := f.tier(t, "empty", false)
	for _, slug := range []string{"openai/allowed", "openai/denied"} {
		if _, err := f.svc.Resolve(ctx, slug, catalog.SurfaceChat, empty); !errors.Is(err, catalog.ErrModelUnavailable) {
			t.Errorf("a tier that lists no models should admit none, yet %s gave %v", slug, err)
		}
	}

	// With one model listed, that one is admitted and the other is not.
	tier := f.tier(t, "cheap", false)
	f.tierModel(t, tier, allowed)
	if _, err := f.svc.Resolve(ctx, "openai/allowed", catalog.SurfaceChat, tier); err != nil {
		t.Errorf("a model inside the tier should be allowed: %v", err)
	}
	if _, err := f.svc.Resolve(ctx, "openai/denied", catalog.SurfaceChat, tier); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Errorf("a model outside the tier should be unavailable, got %v", err)
	}

	// The zero tier applies no filter, for paths with no caller context such
	// as the public catalog.
	if _, err := f.svc.Resolve(ctx, "openai/denied", catalog.SurfaceChat, pgtype.UUID{}); err != nil {
		t.Errorf("a zero-value tier should not filter at all: %v", err)
	}

	// Another tier is unaffected: admission is isolated per tier.
	other := f.tier(t, "rich", true)
	if _, err := f.svc.Resolve(ctx, "openai/denied", catalog.SurfaceChat, other); err != nil {
		t.Errorf("another tier must not be affected by what the cheap tier admits: %v", err)
	}
}

// A tier that no longer exists admits nothing. A cached identity outlives the
// tier it names, and answering "allowed" for a policy that has been deleted
// would serve requests under an admission decision nobody can look up.
func TestResolveRefusesAVanishedTier(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p-gone", "openai")
	model := f.model(t, "openai/gone")
	f.route(t, model, prov, "up-gone", []string{"chat"})

	tier := f.tier(t, "doomed", true)
	// Self-check: it resolves while the tier exists, so a failure below is the
	// deletion and not a broken fixture.
	if _, err := f.svc.Resolve(ctx, "openai/gone", catalog.SurfaceChat, tier); err != nil {
		t.Fatalf("the model should resolve while its tier exists: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM model_tiers WHERE id = $1`, tier); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(ctx, "openai/gone", catalog.SurfaceChat, tier); !errors.Is(err, catalog.ErrModelUnavailable) {
		t.Errorf("a deleted tier should admit nothing, got %v", err)
	}
}

// A model that is not listed must also not be callable. When the hot path tests
// only enabled and ignores visibility, the two become identical in effect and a
// model the operator believes is hidden can still be called. This is the
// regression anchor for that.
func TestResolveRejectsNonPublicVisibility(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prov := f.provider(t, "p-vis", "openai")

	for _, vis := range []string{"hidden", "beta"} {
		slug := "openai/" + vis
		id := f.model(t, slug)
		f.route(t, id, prov, "up-"+vis, []string{"chat"})

		// First prove it works while public, ruling out the false negative
		// where the fixture itself was never wired correctly.
		if _, err := f.svc.Resolve(ctx, slug, catalog.SurfaceChat, pgtype.UUID{}); err != nil {
			t.Fatalf("%s should be available under public visibility (self-check for this case): %v", slug, err)
		}

		if _, err := f.pool.Exec(ctx,
			`UPDATE models SET visibility = $1 WHERE id = $2::uuid`, vis, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Resolve(ctx, slug, catalog.SurfaceChat, pgtype.UUID{}); !errors.Is(err, catalog.ErrModelUnavailable) {
			t.Errorf("visibility=%s should not be callable, got %v", vis, err)
		}
	}
}

func (f *fixture) tier(t *testing.T, slug string, allowAll bool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO model_tiers (slug, allow_all_models) VALUES ($1, $2) RETURNING id`,
		slug, allowAll).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) tierModel(t *testing.T, tier pgtype.UUID, model string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO model_tier_models (tier_id, model_id) VALUES ($1, $2::uuid)`,
		tier, model); err != nil {
		t.Fatal(err)
	}
}
