// Package catalogtest seeds the catalog the way tests need it: a route plus
// what is known about its endpoints.
//
// A route declares nothing about what it serves; the probe table does. Tests
// that used to write `endpoints` on the route now write verdicts, and this
// package is where that one INSERT lives so that every test package seeds the
// same shape.
package catalogtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// DB is the slice of a pool or a transaction the seeders need.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SeedRoute inserts a route and records each of verified as `ok`, which is what
// the catalog publishes. Candidacy is wider than this: the data plane tries any
// endpoint not found unsupported, so a test about dispatch on an unlisted
// endpoint seeds nothing for it, and a test about exclusion calls SeedVerdict
// with `unsupported`.
func SeedRoute(t testing.TB, db DB, modelID, providerID any, upstream string, verified ...string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := db.QueryRow(context.Background(),
		`INSERT INTO model_routes (model_id, provider_id, provider_model_id)
		 VALUES ($1, $2, $3) RETURNING id`, modelID, providerID, upstream).Scan(&id); err != nil {
		t.Fatalf("catalogtest: seed route: %v", err)
	}
	for _, ep := range verified {
		SeedVerdict(t, db, id, ep, "ok")
	}
	return id
}

// SeedVerdict writes what is known about one endpoint of a route, the way the
// probe worker would.
func SeedVerdict(t testing.TB, db DB, routeID any, endpoint, status string) {
	t.Helper()
	protocol, ok := catalog.ProtocolForEndpoint(endpoint)
	if !ok {
		t.Fatalf("catalogtest: unknown endpoint %q", endpoint)
	}
	mode, _ := catalog.ProbeModeForEndpoint(endpoint)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status, checked_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (route_id, endpoint) DO UPDATE
		 SET status = excluded.status, source = 'probe', checked_at = now()`,
		routeID, endpoint, protocol, string(mode), status); err != nil {
		t.Fatalf("catalogtest: seed verdict: %v", err)
	}
}
