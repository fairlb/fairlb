package gwdb_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

// A row-level security policy on the parent table has to cover the partitions
// themselves: querying a partition directly must not step around the isolation.
//
// The test lives next to the table it covers. Which package a SQL test belongs
// to is decided by the table name in the query, and a table name is a string —
// no compile-time rule can place it for you, so it has to be placed by hand.
func TestRLSAppliesToPartitionsDirectly(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	mk := func(slug string) string {
		var org string
		org = publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Slug: slug, Name: "T"}))
		var key string
		if err := pool.QueryRow(ctx,
			`INSERT INTO api_keys (org_id,name,prefix,key_hash,scopes)
			 VALUES ($1::uuid,'k','sk-flb-v1-1',$1,ARRAY['inference']) RETURNING id::text`,
			org).Scan(&key); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_logs (org_id,api_key_id,request_id,surface,model_slug,
			                         status,http_status,charged_nano,charged_currency)
			 VALUES ($1::uuid,$2::uuid,'req-'||$1,'chat_completions','m','ok',200,1,'USD')`,
			org, key); err != nil {
			t.Fatal(err)
		}
		return org
	}
	orgA := mk("part-a")
	_ = mk("part-b")

	partition := "usage_logs_" + time.Now().UTC().Format("2006_01")
	if err := db.WithOrgTx(ctx, pool, orgA, func(tx pgx.Tx) error {
		var viaParent, viaPartition int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM usage_logs`).Scan(&viaParent); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+partition).Scan(&viaPartition); err != nil {
			return err
		}
		if viaParent != 1 {
			t.Errorf("through the parent table this org should see only its own 1 row, got %d", viaParent)
		}
		if viaPartition != 1 {
			t.Errorf("querying partition %s directly returned %d rows: the parent policy does not cover the partition, so isolation can be stepped around",
				partition, viaPartition)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
