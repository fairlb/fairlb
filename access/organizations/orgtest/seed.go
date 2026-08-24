// Package orgtest seeds organization rows for tests in either product.
//
// Every test that needs "an organization exists" used to write the INSERT
// itself -- 48 statements across both modules, all but a handful of them the
// same three columns with a different slug. That is not explicitness, it is a
// schema change waiting to break 48 places at once, and it had already produced
// two spellings of the same row (`(slug, name, kind)` and `(name, slug, kind)`).
//
// Seeding goes through organizations.Store rather than raw SQL, so "what a row
// looks like" is defined once, by the code that owns the table.
//
// Two kinds of caller keep their own INSERT on purpose, and should:
//
//   - organizations' own tests, which must not seed through the store they are
//     testing; and
//   - schema tests, whose subject *is* the statement -- constraints, partition
//     routing, RLS policies. There the SQL is the assertion.
package orgtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// Seed describes the organization to create. Every field is optional: Slug
// defaults to a value guaranteed unique within the process, Name to the slug,
// and Kind to "team" -- a test that says nothing about them is a test they do
// not matter to.
type Seed struct {
	Slug     string
	Name     string
	Kind     string
	Status   string
	Currency string
}

// seq makes generated slugs unique. Several call sites used to write
// `substr(md5(random()::text), 1, 8)` for the same purpose, which is unique
// with high probability rather than unique -- and a collision there fails the
// unique index at a point that says nothing about why.
var seq atomic.Uint64

func (s Seed) withDefaults() Seed {
	if s.Slug == "" {
		s.Slug = fmt.Sprintf("org-%d", seq.Add(1))
	}
	if s.Name == "" {
		s.Name = s.Slug
	}
	if s.Kind == "" {
		s.Kind = "team"
	}
	return s
}

// CreateID is Create for the callers that want the id as a string. Three test
// packages had written the same three-line wrapper around Create for exactly
// this, all named seedOrg.
func CreateID(t *testing.T, pool *pgxpool.Pool, in Seed) string {
	t.Helper()
	return publicid.UUIDString(Create(t, pool, in))
}

// Create seeds one organization and returns its id.
func Create(t *testing.T, pool *pgxpool.Pool, in Seed) pgtype.UUID {
	t.Helper()
	in = in.withDefaults()
	var id pgtype.UUID
	err := db.WithSystemTx(context.Background(), pool, func(tx pgx.Tx) error {
		store := organizations.New(pool).WithTx(tx)
		row, err := store.Create(context.Background(), organizations.CreateOrganization{
			Slug: in.Slug, Name: in.Name, Kind: in.Kind, Currency: in.Currency,
		})
		if err != nil {
			return err
		}
		id = row.ID
		if in.Status != "" && in.Status != "active" {
			_, err = store.SetStatus(context.Background(), id, in.Status)
		}
		return err
	})
	if err != nil {
		t.Fatalf("seed organization %q: %v", in.Slug, err)
	}
	return id
}
