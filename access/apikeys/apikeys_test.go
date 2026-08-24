package apikeys_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
)

type fixture struct {
	svc  *apikeys.Service
	pool *pgxpool.Pool
	q    *apikeys.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testpg.Start(t)
	q := apikeys.NewStore(pool)
	// A permissive stub authorizer. What this package tests is key material and
	// lifecycle, not membership rules. The membership cases (an outsider gets
	// 404, a plain member gets 403) live with their implementation, in the
	// package that owns the membership table.
	return &fixture{svc: apikeys.NewService(apikeys.ServiceConfig{Database: pool, Admin: allowKeyAdmin}), pool: pool, q: q}
}

func allowKeyAdmin(context.Context, pgtype.UUID, pgtype.UUID) error { return nil }

// actor is a placeholder for the acting user. The public service itself owns
// no user table; deployments that need creator attribution inject a recorder.
var actor pgtype.UUID

func (f *fixture) org(t *testing.T, slug string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	id = orgtest.Create(t, f.pool, orgtest.Seed{Slug: slug, Name: "T"})
	return id
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *httpx.CodeError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestCreateKeyShapeAndOnceOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-a")

	plaintext, row, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	// Shape: the fixed prefix plus a base62 body; the display form is the prefix
	// plus eight characters; only a SHA-256 is stored.
	if !strings.HasPrefix(plaintext, "sk-flb-v1-") || len(plaintext) < len("sk-flb-v1-")+30 {
		t.Fatalf("key has the wrong shape: %q", plaintext)
	}
	if row.Prefix != plaintext[:len("sk-flb-v1-")+8] {
		t.Fatalf("display prefix does not match: %q vs %q", row.Prefix, plaintext)
	}
	if row.Status != "active" || len(row.Scopes) != 1 || row.Scopes[0] != "inference" {
		t.Fatalf("unexpected defaults: %+v", row)
	}
	// Data-plane authentication primitive: the hash lookup finds the key.
	got, err := f.q.RecordByHash(ctx, crypto.HashToken(plaintext))
	if err != nil || got.ID != row.ID {
		t.Fatalf("lookup by hash: %v", err)
	}
	if got.KeyHash != crypto.HashToken(plaintext) {
		t.Fatal("what is stored should be the SHA-256 of the plaintext")
	}
	// A duplicate name is refused.
	_, _, err = f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"})
	wantCode(t, err, errcode.CommonValidation)
	// An empty name is refused.
	_, _, err = f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "  "})
	wantCode(t, err, errcode.CommonValidation)
}

func TestKeyRevokeLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-rv")
	other := f.org(t, "keys-rv2")

	_, row, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Revoke(ctx, org, actor, row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, err := f.q.RecordByOrg(ctx, row.ID, org)
	if err != nil || got.Status != "revoked" {
		t.Fatalf("should be revoked: %+v %v", got, err)
	}
	// Revoking twice is idempotent; from another org it is 404, which hides
	// whether the key exists at all.
	if err := f.svc.Revoke(ctx, org, actor, row.ID); err != nil {
		t.Fatalf("revoking twice should be idempotent: %v", err)
	}
	wantCode(t, f.svc.Revoke(ctx, other, actor, row.ID), errcode.CommonNotFound)
	// The list includes revoked keys, so the history stays auditable.
	rows, err := f.svc.List(ctx, org, actor, 200, pgtype.Timestamptz{}, pgtype.UUID{})
	if err != nil || len(rows) != 1 || rows[0].Status != "revoked" {
		t.Fatalf("list: %v %v", rows, err)
	}
}

func TestKeyCreateWithLimitsAndSpendPrimitive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-lim")

	exp := time.Now().Add(24 * time.Hour)
	_, row, err := f.svc.Create(ctx, apikeys.CreateInput{
		OrgID: org, ActorID: actor, Name: "budget",
		ExpiresAt:          pgtype.Timestamptz{Time: exp, Valid: true},
		SpendLimitNano:     pgtype.Int8{Int64: 1000, Valid: true},
		SpendLimitInterval: pgtype.Text{String: "daily", Valid: true},
		RateLimitRpm:       pgtype.Int4{Int32: 60, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.SpendLimitNano == nil || *row.SpendLimitNano != 1000 ||
		row.SpendLimitInterval == nil || *row.SpendLimitInterval != "daily" ||
		row.RateLimitRpm == nil || *row.RateLimitRpm != 60 || row.ExpiresAt == nil {
		t.Fatalf("restriction columns were not stored: %+v", row)
	}
	// The daily spend primitive the budget gate is built on: the window filter
	// is applied correctly.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for _, d := range []struct {
		day   time.Time
		spent int64
	}{{today.AddDate(0, 0, -2), 5}, {today, 3}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO api_key_daily_spend (api_key_id, day, spent_nano)
			VALUES ($1, $2, $3)`, row.ID, d.day, d.spent); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.q.SpendSince(ctx, row.ID, pgtype.Date{Time: today, Valid: true})
	if err != nil || got != 3 {
		t.Fatalf("the windowed sum should be 3: %d %v", got, err)
	}
}

func TestKeyInfraFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-inf")
	f.pool.Close()
	if _, _, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "x"}); err == nil {
		t.Fatal("creating a key should fail when the database is down")
	}
	if _, err := f.svc.List(ctx, org, actor, 200, pgtype.Timestamptz{}, pgtype.UUID{}); err == nil {
		t.Fatal("listing should fail when the database is down")
	}
	if err := f.svc.Revoke(ctx, org, actor, actor); err == nil {
		t.Fatal("revoking should fail when the database is down")
	}
}

// After revocation the same name can be reused, because the unique index is
// partial and covers active keys only.
func TestKeyNameReusableAfterRevoke(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-rotate")

	_, k1, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	// While the original is active, the name is still refused.
	if _, _, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"}); err == nil {
		t.Fatal("a duplicate name should be refused while the original is active")
	}
	// Once revoked, the name is free again — this is what key rotation needs.
	if err := f.svc.Revoke(ctx, org, actor, k1.ID); err != nil {
		t.Fatal(err)
	}
	_, k2, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"})
	if err != nil {
		t.Fatalf("the name should be reusable after revocation: %v", err)
	}
	if k2.ID == k1.ID {
		t.Fatal("should be a new key")
	}
	// The new active key now holds that name.
	if _, _, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "prod"}); err == nil {
		t.Fatal("the name should be refused again now that a new key holds it")
	}
}

// A suspended org cannot write keys (403) but can still read them.
func TestSuspendedOrgBlocksKeyWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "key-suspend")

	_, existing, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE orgs SET status='suspended' WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}

	// Create and revoke are refused; listing is allowed.
	_, _, err = f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "after"})
	wantCode(t, err, errcode.CommonOrgSuspended)
	wantCode(t, f.svc.Revoke(ctx, org, actor, existing.ID), errcode.CommonOrgSuspended)
	if _, err := f.svc.List(ctx, org, actor, 200, pgtype.Timestamptz{}, pgtype.UUID{}); err != nil {
		t.Fatalf("listing should still be allowed while suspended: %v", err)
	}

	// Reinstating restores both.
	if _, err := f.pool.Exec(ctx, `UPDATE orgs SET status='active' WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "after"}); err != nil {
		t.Fatalf("creating a key should work again after reinstatement: %v", err)
	}
}

// The revocation invalidation callback. The data plane caches a key-to-org
// snapshot under the key's hash, so revocation has to call back with that same
// hash; otherwise a leaked key keeps spending until the cache entry expires.
func TestKeyRevokeInvalidatesByHash(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	org := f.org(t, "keys-inv")

	var invalidated []string
	f.svc = apikeys.NewService(apikeys.ServiceConfig{
		Database: f.pool,
		Admin:    allowKeyAdmin,
		Invalidator: func(_ context.Context, keyHash string) {
			invalidated = append(invalidated, keyHash)
		},
	})

	plaintext, row, err := f.svc.Create(ctx, apikeys.CreateInput{OrgID: org, ActorID: actor, Name: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidated) != 0 {
		t.Fatalf("creating a key should not invalidate anything: %v", invalidated)
	}

	if err := f.svc.Revoke(ctx, org, actor, row.ID); err != nil {
		t.Fatal(err)
	}
	// The callback must carry the hash the data plane authenticates by — the
	// SHA-256 of the plaintext — or the cache entry is never cleared.
	want := crypto.HashToken(plaintext)
	if len(invalidated) != 1 || invalidated[0] != want {
		t.Fatalf("the invalidation callback should carry the key hash: %v, want %s", invalidated, want)
	}

	// A repeated revocation still broadcasts: invalidation is idempotent, and
	// one extra broadcast beats a surviving cache entry.
	if err := f.svc.Revoke(ctx, org, actor, row.ID); err != nil {
		t.Fatal(err)
	}
	if len(invalidated) != 2 {
		t.Fatalf("a repeated revocation should broadcast again: %v", invalidated)
	}

	// Revocation still works with a separately constructed service that has no
	// callback, which is the case when there is no data-plane cache.
	withoutInvalidation := apikeys.NewService(apikeys.ServiceConfig{Database: f.pool, Admin: allowKeyAdmin})
	if err := withoutInvalidation.Revoke(ctx, org, actor, row.ID); err != nil {
		t.Fatalf("revocation should work with no callback installed: %v", err)
	}
}
