package gwstaffapi_test

import (
	"context"
	"testing"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// Access tiers and pricing plans page, and the default row stays first across
// the whole walk (ADR-0191).
//
// Both lists used to answer with everything they had: tiers behind a silent cap
// of 200, plans behind no bound at all. What makes this test worth writing is
// not the count but the key. The order is (is_default DESC, slug), which mixes
// directions, so the cursor condition cannot be the usual `> (a, b)` tuple
// compare -- get it wrong and the walk either repeats the default row on every
// page or drops every non-default row after the first. Both failures look like
// a working list until you count.
func TestConfigListsPageWithoutLosingOrDuplicatingRows(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	// 12 of each, plus whatever the fixture seeds. Small pages (5) mean the walk
	// crosses the default row's boundary rather than fitting in one response.
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_tiers (slug, name, allow_all_models)
		SELECT 'paged-tier-' || lpad(i::text, 3, '0'), 'T' || i, true
		FROM generate_series(1, 12) AS i`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pricing_plans (slug, name, default_multiplier_bps)
		SELECT 'paged-plan-' || lpad(i::text, 3, '0'), 'P' || i, 10000
		FROM generate_series(1, 12) AS i`); err != nil {
		t.Fatal(err)
	}

	limit := 5
	t.Run("tiers", func(t *testing.T) {
		seen, first := map[string]int{}, ""
		var cursor *string
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatal("the walk did not terminate: next_cursor keeps coming back non-empty, " +
					"which means the cursor condition is not advancing past the rows it returned")
			}
			res, err := s.ListGatewayTiers(ctx, gwstaffapi.ListGatewayTiersRequestObject{
				Params: gwstaffapi.ListGatewayTiersParams{Cursor: cursor, Limit: &limit},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := res.(gwstaffapi.ListGatewayTiers200JSONResponse)
			for _, row := range got.Items {
				seen[row.Slug]++
				if first == "" {
					first = row.Slug
				}
			}
			if got.NextCursor == nil {
				break
			}
			cursor = got.NextCursor
		}
		assertWalkedEachOnce(t, seen, "paged-tier-", 12)

		// The default tier is the fixture's own, and it has to be the first row of
		// the first page -- not merely present somewhere in the walk.
		var defaultSlug string
		if err := pool.QueryRow(ctx,
			`SELECT slug FROM model_tiers WHERE is_default`).Scan(&defaultSlug); err != nil {
			t.Fatal(err)
		}
		if first != defaultSlug {
			t.Fatalf("first row of the walk = %q, want the default tier %q: the default has to "+
				"lead the list, and a cursor that reorders it is a cursor that changed the answer",
				first, defaultSlug)
		}
	})

	t.Run("plans", func(t *testing.T) {
		seen := map[string]int{}
		var cursor *string
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatal("the plan walk did not terminate")
			}
			res, err := s.ListGatewayPricingPlans(ctx, gwstaffapi.ListGatewayPricingPlansRequestObject{
				Params: gwstaffapi.ListGatewayPricingPlansParams{Cursor: cursor, Limit: &limit},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := res.(gwstaffapi.ListGatewayPricingPlans200JSONResponse)
			for _, row := range got.Items {
				seen[row.Slug]++
			}
			if got.NextCursor == nil {
				break
			}
			cursor = got.NextCursor
		}
		assertWalkedEachOnce(t, seen, "paged-plan-", 12)
	})

	// Search runs in the database, not over the page the client happens to hold.
	// The proof is that it finds a row the first page does not contain: without
	// a server-side `q` this returns nothing at all.
	t.Run("search reaches past the first page", func(t *testing.T) {
		q := "paged-tier-012"
		res, err := s.ListGatewayTiers(ctx, gwstaffapi.ListGatewayTiersRequestObject{
			Params: gwstaffapi.ListGatewayTiersParams{Q: &q, Limit: &limit},
		})
		if err != nil {
			t.Fatal(err)
		}
		items := res.(gwstaffapi.ListGatewayTiers200JSONResponse).Items
		if len(items) != 1 || items[0].Slug != q {
			t.Fatalf("search for %q returned %d rows: the tier picker on an org's access page "+
				"has to reach a tier that is not on the first page", q, len(items))
		}

		qp := "paged-plan-012"
		pres, err := s.ListGatewayPricingPlans(ctx, gwstaffapi.ListGatewayPricingPlansRequestObject{
			Params: gwstaffapi.ListGatewayPricingPlansParams{Q: &qp, Limit: &limit},
		})
		if err != nil {
			t.Fatal(err)
		}
		pitems := pres.(gwstaffapi.ListGatewayPricingPlans200JSONResponse).Items
		if len(pitems) != 1 || pitems[0].Slug != qp {
			t.Fatalf("search for %q returned %d rows", qp, len(pitems))
		}
	})
}

// assertWalkedEachOnce checks that every seeded row came back exactly once. A
// duplicate means the cursor did not advance; a miss means it advanced too far.
func assertWalkedEachOnce(t *testing.T, seen map[string]int, prefix string, want int) {
	t.Helper()
	found, dup := 0, []string{}
	for slug, n := range seen {
		if n > 1 {
			dup = append(dup, slug)
		}
		if len(slug) > len(prefix) && slug[:len(prefix)] == prefix {
			found++
		}
	}
	if len(dup) > 0 {
		t.Fatalf("%d rows came back on more than one page (%v): the cursor is not advancing "+
			"past what it returned", len(dup), dup)
	}
	if found != want {
		t.Fatalf("walked %d rows with prefix %q, want %d: rows are being dropped between pages",
			found, prefix, want)
	}
}
