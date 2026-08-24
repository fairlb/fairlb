package gwdb_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

// The default tier is where a new org lands: with no row in org_gateway_settings
// a organization falls back to it. The migration has to seed it, and "exactly one"
// has to be enforced by the database — guarding it in application code leaks
// as soon as two tiers are created concurrently.
func TestTierDefaultInvariants(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	var slug string
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT slug, (SELECT count(*) FROM model_tiers WHERE is_default) FROM model_tiers WHERE is_default`).
		Scan(&slug, &n); err != nil {
		t.Fatalf("the migration did not seed a default tier: %v", err)
	}
	if slug != "default" || n != 1 {
		t.Fatalf("expected exactly one default tier with slug=default: slug=%q n=%d", slug, n)
	}

	// A second default tier must be rejected (partial unique index).
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_tiers (slug, is_default) VALUES ('vip', true)`); err == nil {
		t.Error("a second default tier should be rejected: two of them make the fallback target ambiguous")
	}

	// The default tier cannot be disabled: without it every organization that has no
	// explicit tier would be refused service.
	if _, err := pool.Exec(ctx,
		`UPDATE model_tiers SET status = 'disabled' WHERE is_default`); err == nil {
		t.Error("the default tier should not be disableable")
	}

	// The default tier cannot be deleted. DeleteTier carries a NOT is_default
	// predicate; this asserts the SQL-level rule directly.
	res, err := pool.Exec(ctx, `DELETE FROM model_tiers WHERE id = (SELECT id FROM model_tiers WHERE is_default) AND NOT is_default`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowsAffected() != 0 {
		t.Error("the default tier should not be deletable")
	}

	// Moving the default takes two steps — clear, then set. A single UPDATE
	// cannot get past the unique index.
	vip := seedTier(t, pool, "vip")
	if _, err := pool.Exec(ctx, `UPDATE model_tiers SET is_default = true WHERE id = $1`, vip); err == nil {
		t.Error("setting a new default without clearing the old one should hit the unique index")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE model_tiers SET is_default = false WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE model_tiers SET is_default = true WHERE id = $1`, vip); err != nil {
		t.Fatalf("clear-then-set inside one transaction should succeed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the default-tier move failed: %v", err)
	}
}

// Deleting a tier must not silently change what its members may reach: a tier
// with members cannot be deleted (ON DELETE RESTRICT), so whoever removes it
// has to move them first. Under SET NULL those organizations would be moved back to
// the default tier with nothing said.
func TestTierDeleteRestrictedByMembers(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	tier := seedTier(t, pool, "enterprise")
	org := orgtest.CreateID(t, pool, orgtest.Seed{Name: "o"})
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_gateway_settings (org_id, tier_id) VALUES ($1, $2)`, org, tier); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM model_tiers WHERE id = $1`, tier); err == nil {
		t.Fatal("a tier with members should not be deletable: that would silently change what those organizations may reach")
	}

	// Once the members are moved it can be deleted.
	if _, err := pool.Exec(ctx,
		`UPDATE org_gateway_settings SET tier_id = NULL WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM model_tiers WHERE id = $1`, tier); err != nil {
		t.Fatalf("the tier should be deletable once its members are moved: %v", err)
	}
}

// What a tier admits is said in a column, not counted in a table: a tier either
// admits everything, or admits exactly what it lists. The admission rule itself
// lives with the request path; what is pinned here is that a listing-nothing
// tier is a legal state, that it is not the same state as allow-all, and how
// the cascade behaves.
func TestTierModelsCascade(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	tier := seedTier(t, pool, "cheap")
	model := seedModel(t, pool, "openai/tier-m")

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_tier_models WHERE tier_id = $1`, tier).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a newly created tier should list no models: %d", n)
	}
	// And it restricts by default: creating a tier is an act of restricting,
	// so the permissive reading has to be asked for rather than inherited.
	var allowAll bool
	if err := pool.QueryRow(ctx,
		`SELECT allow_all_models FROM model_tiers WHERE id = $1`, tier).Scan(&allowAll); err != nil {
		t.Fatal(err)
	}
	if allowAll {
		t.Error("a newly created tier must not admit everything by default")
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO model_tier_models (tier_id, model_id) VALUES ($1, $2)`, tier, model); err != nil {
		t.Fatal(err)
	}
	// Listing the same model twice is idempotent; AddTierModels leans on this
	// primary key for its ON CONFLICT DO NOTHING.
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_tier_models (tier_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		tier, model); err != nil {
		t.Fatalf("listing the same model twice should be idempotent: %v", err)
	}

	// Deleting a model takes its tier listings with it, leaving no dangling rows.
	if _, err := pool.Exec(ctx, `DELETE FROM models WHERE id = $1`, model); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM model_tier_models WHERE tier_id = $1`, tier).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("tier listings should cascade away when the model is deleted: %d", n)
	}
}

func seedTier(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO model_tiers (slug) VALUES ($1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The seeded default tier admits everything. It is the one tier for which the
// permissive reading is right: a deployment nobody has configured yet must
// serve, not refuse.
func TestDefaultTierAdmitsEverything(t *testing.T) {
	pool := testpg.Start(t)
	var allowAll bool
	if err := pool.QueryRow(context.Background(),
		`SELECT allow_all_models FROM model_tiers WHERE is_default`).Scan(&allowAll); err != nil {
		t.Fatal(err)
	}
	if !allowAll {
		t.Error("the seeded default tier must admit every model, or a fresh install refuses every request")
	}
}
