package gwdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

func TestResourceAffinityIsOrgScopedCredentialPinnedAndExpiring(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)

	var orgA, orgB, provider, model, route, key pgtype.UUID
	for slug, dst := range map[string]*pgtype.UUID{"affinity-a": &orgA, "affinity-b": &orgB} {
		*dst = orgtest.Create(t, pool, orgtest.Seed{Slug: slug})
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('affinity-provider', 'custom', ARRAY['openai'], 'https://upstream.test') RETURNING id`,
	).Scan(&provider); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug) VALUES ('openai/affinity') RETURNING id`,
	).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO model_routes (model_id, provider_id, provider_model_id)
		 VALUES ($1, $2, 'upstream-affinity') RETURNING id`,
		model, provider,
	).Scan(&route); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc)
		 VALUES ($1, 'primary', '\x01') RETURNING id`, provider,
	).Scan(&key); err != nil {
		t.Fatal(err)
	}

	params := gwdb.UpsertResourceAffinityParams{
		OrgID: orgA, Protocol: "openai", ResourceType: "response", UpstreamID: "resp_same",
		ModelID: model, RouteID: route, ProviderID: provider, ProviderKeyID: key,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}
	if err := q.UpsertResourceAffinity(ctx, params); err != nil {
		t.Fatal(err)
	}
	row, err := q.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: orgA, Protocol: "openai", ResourceType: "response", UpstreamID: "resp_same",
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RouteID != route || row.ProviderID != provider || row.ProviderKeyID != key {
		t.Fatalf("affinity lost its exact route or credential: %+v", row)
	}

	_, err = q.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: orgB, Protocol: "openai", ResourceType: "response", UpstreamID: "resp_same",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another org must not discover the id, got %v", err)
	}

	params.UpstreamID = "resp_expired"
	params.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	if err := q.UpsertResourceAffinity(ctx, params); err != nil {
		t.Fatal(err)
	}
	_, err = q.GetResourceAffinity(ctx, gwdb.GetResourceAffinityParams{
		OrgID: orgA, Protocol: "openai", ResourceType: "response", UpstreamID: "resp_expired",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an expired affinity must behave as missing, got %v", err)
	}
	if deleted, err := q.DeleteExpiredResourceAffinities(ctx); err != nil || deleted != 1 {
		t.Fatalf("expired affinity GC deleted %d rows: %v", deleted, err)
	}
}
