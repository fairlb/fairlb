package gwstaffapi_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// A probe being in flight is a fact of its own, kept apart from the verdict.
//
// The alternative was a fifth `status`, and it fails on the case below: the
// catalogue publishes endpoints found `ok`, so a pending value living in
// `status` would take the route out of the catalogue for as long as the
// re-probe ran -- the probe mechanism injuring exactly what it exists to
// protect (ADR-0224).
func TestTheInFlightMarkerLeavesTheStandingVerdictAlone(t *testing.T) {
	s, pool, _ := newServer(t)
	model := mustModel(t, s, "openai/reprobe-keeps-verdict")
	prov := mustProvider(t, s, catalog.ProtocolOpenAI, "https://api.example.com")
	route := mustRoute(t, s, model.Id, prov, "gpt-5", []string{"chat"})

	markInFlight(t, pool, route, "chat")

	probe := readProbe(t, s, model.Id, route, "chat")
	if probe.Status != "ok" {
		t.Fatalf("the standing verdict became %q while a probe was in flight", probe.Status)
	}
	if probe.ProbeEnqueuedAt == nil {
		t.Fatal("the marker did not reach the contract, so the interface cannot say a probe is " +
			"running -- which on an endpoint that costs a real generation means paying twice")
	}
	// The catalogue must still publish it. This is the assertion the fifth
	// status value would have failed.
	if !publishesEndpoint(t, pool, model.Id, "chat") {
		t.Fatal("the route left the catalogue while a re-probe was in flight")
	}
}

// The operator's own verdict ends whatever was in flight: the row has an answer
// now, and it is theirs.
func TestAnOperatorVerdictClearsTheInFlightMarker(t *testing.T) {
	s, pool, _ := newServer(t)
	model := mustModel(t, s, "openai/verdict-clears-marker")
	prov := mustProvider(t, s, catalog.ProtocolOpenAI, "https://api.example.com")
	route := mustRoute(t, s, model.Id, prov, "gpt-5", nil)

	markInFlight(t, pool, route, "chat")
	written := mustVerdict(t, s, model.Id, route, "chat", "ok")
	if written.ProbeEnqueuedAt != nil {
		t.Fatal("the marker outlived the verdict; an endpoint stuck in flight can never be " +
			"asked about again")
	}
}

// Naming no endpoint means "every one the worker probes on its own", and the
// marker has to follow that rule rather than a wider one. Marking an endpoint
// nobody is about to probe leaves it in flight forever: no verdict is coming to
// clear it, and the interface would refuse to let anyone ask.
func TestTheMarkerFollowsTheWorkersOwnEndpointRule(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	model := mustModel(t, s, "openai/marker-follows-auto")
	prov := mustProvider(t, s, catalog.ProtocolOpenAI, "https://api.example.com")
	route := mustRoute(t, s, model.Id, prov, "gpt-5", nil)

	q := gwdb.New(pool)
	if err := q.MarkRouteProbesEnqueued(ctx, gwdb.MarkRouteProbesEnqueuedParams{
		RouteID: pgUUID(route), Endpoints: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	// `images` is never probed automatically -- one probe is a real generation
	// -- so a blanket ask does not cover it.
	if inFlight(t, s, model.Id, route, "images") {
		t.Fatal("a blanket probe marked the paid endpoint as in flight; nothing will ever " +
			"clear it, and the operator can never ask for the one probe they meant to buy")
	}
	if !inFlight(t, s, model.Id, route, "chat") {
		t.Fatal("a blanket probe left the automatically probed endpoint unmarked")
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func markInFlight(t *testing.T, pool *pgxpool.Pool, route uuid.UUID, endpoint string) {
	t.Helper()
	if err := gwdb.New(pool).MarkRouteProbesEnqueued(context.Background(),
		gwdb.MarkRouteProbesEnqueuedParams{RouteID: pgUUID(route), Endpoints: []string{endpoint}},
	); err != nil {
		t.Fatal(err)
	}
}

func inFlight(t *testing.T, s *gwstaffapi.Server, model, route uuid.UUID, endpoint string) bool {
	t.Helper()
	return readProbe(t, s, model, route, endpoint).ProbeEnqueuedAt != nil
}

func publishesEndpoint(t *testing.T, pool *pgxpool.Pool, model uuid.UUID, endpoint string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM model_published_endpoints WHERE model_id = $1 AND endpoint = $2`,
		model, endpoint).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func readProbe(
	t *testing.T, s *gwstaffapi.Server, model, route uuid.UUID, endpoint string,
) gwstaffapi.GatewayRouteProbe {
	t.Helper()
	res, err := s.ListGatewayRoutes(context.Background(),
		gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items {
		if r.Id != route || r.Probes == nil {
			continue
		}
		for _, p := range *r.Probes {
			if string(p.Endpoint) == endpoint {
				return p
			}
		}
	}
	t.Fatalf("no probe row for %s", endpoint)
	return gwstaffapi.GatewayRouteProbe{}
}
