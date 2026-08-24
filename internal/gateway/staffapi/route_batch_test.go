package gwstaffapi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// Batch wiring. Three things are guarded here, each standing for a real way
// this can go wrong: every per-row verdict lands on the right row, a failed row
// does not take the successful ones with it, and creates happen before
// deletes.

func batchCreate(model uuid.UUID, upstream string) gwstaffapi.GatewayRouteBatchCreate {
	return gwstaffapi.GatewayRouteBatchCreate{ModelId: &model, ProviderModelId: upstream}
}

// batchCreateNew is the "upstream has it, the local catalog does not" row: it
// creates the catalog entry along the way. There is no protocol to state for
// it: a model owns none.
func batchCreateNew(slug, upstream string) gwstaffapi.GatewayRouteBatchCreate {
	return gwstaffapi.GatewayRouteBatchCreate{
		NewModel:        &gwstaffapi.GatewayRouteBatchNewModel{Slug: slug},
		ProviderModelId: upstream,
	}
}

func mustBatch(
	t *testing.T, s *gwstaffapi.Server, prov uuid.UUID,
	creates []gwstaffapi.GatewayRouteBatchCreate, deletes []gwstaffapi.GatewayRouteBatchDelete,
) []gwstaffapi.GatewayRouteBatchItemResult {
	t.Helper()
	res, err := s.BatchWireProviderRoutes(context.Background(),
		gwstaffapi.BatchWireProviderRoutesRequestObject{
			ProviderId: prov,
			Body: &gwstaffapi.GatewayRouteBatchInput{
				Creates: creates, Deletes: deletes,
			},
		})
	if err != nil {
		t.Fatalf("batch wiring: %v", err)
	}
	ok, is := res.(gwstaffapi.BatchWireProviderRoutes200JSONResponse)
	if !is {
		t.Fatalf("it should return 200: %T", res)
	}
	return ok.Results
}

// providerRouteUpstreams reads back through the provider-scoped listing, which
// is a different path from the one under test: an assertion should not be
// validated by the thing it is validating.
func providerRouteUpstreams(t *testing.T, s *gwstaffapi.Server, prov uuid.UUID) []string {
	t.Helper()
	res, err := s.ListGatewayProviderRoutes(context.Background(),
		gwstaffapi.ListGatewayProviderRoutesRequestObject{ProviderId: prov})
	if err != nil {
		t.Fatalf("look up the provider's routes: %v", err)
	}
	rows := res.(gwstaffapi.ListGatewayProviderRoutes200JSONResponse).Items
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ProviderModelId)
	}
	return out
}

func findResult(
	t *testing.T, results []gwstaffapi.GatewayRouteBatchItemResult,
	kind gwstaffapi.GatewayRouteBatchItemResultKind, model uuid.UUID, upstream string,
) gwstaffapi.GatewayRouteBatchItemResult {
	t.Helper()
	for _, r := range results {
		if r.Kind == kind && r.ModelId != nil && *r.ModelId == model &&
			r.ProviderModelId == upstream {
			return r
		}
	}
	t.Fatalf("%s %s/%s is not in the result: %+v", kind, model, upstream, results)
	return gwstaffapi.GatewayRouteBatchItemResult{}
}

// One row of each of the four outcomes in a single batch, each landing on the
// right row.
//
// A batch endpoint is often assumed to lose this granularity. It does not:
// granularity is decided by the shape of the response, not by whether the
// request carried one row or fifty.
func TestBatchWireMixedOutcomes(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	m1 := mustModel(t, s, "openai/m1")
	m2 := mustModel(t, s, "openai/m2")
	// A model that does not exist: the existence check refuses it, and this
	// row must come back failed rather than dragging the whole batch down.
	ghost := uuid.New()

	// Two existing routes: one to collide with, one to delete.
	mustRoute(t, s, m1.Id, prov, "taken", []string{"chat"})
	mustRoute(t, s, m2.Id, prov, "to-delete", []string{"chat"})
	existing := mustBatch(t, s, prov, nil, nil) // an empty batch must work too
	if len(existing) != 0 {
		t.Fatalf("an empty batch must not produce result rows: %+v", existing)
	}
	delRoute := routeIDOf(t, s, m2.Id, "to-delete")

	results := mustBatch(t, s, prov,
		[]gwstaffapi.GatewayRouteBatchCreate{
			batchCreate(m1.Id, "fresh"), // done
			batchCreate(m1.Id, "taken"), // already: unique violation
			batchCreate(ghost, "x"),     // failed: no such model
		},
		[]gwstaffapi.GatewayRouteBatchDelete{
			{ModelId: m2.Id, RouteId: delRoute},   // done
			{ModelId: m2.Id, RouteId: uuid.New()}, // already: never there
		})

	if got := len(results); got != 5 {
		t.Fatalf("the result must have one row per request row, got %d: %+v", got, results)
	}

	const (
		done    = gwstaffapi.GatewayRouteBatchItemResultOutcomeDone
		already = gwstaffapi.GatewayRouteBatchItemResultOutcomeAlready
		failed  = gwstaffapi.GatewayRouteBatchItemResultOutcomeFailed
	)
	fresh := findResult(t, results, gwstaffapi.Create, m1.Id, "fresh")
	if fresh.Outcome != done {
		t.Errorf("a create should be done, got %s (%v)", fresh.Outcome, fresh.Detail)
	}
	if fresh.RouteId == nil {
		t.Error("a successful create must return route_id -- the caller needs it for the later delete")
	}
	// A unique violation means the row is already there, which is what was
	// asked for. Calling it failed makes people retry a goal they reached.
	if got := findResult(t, results, gwstaffapi.Create, m1.Id, "taken").Outcome; got != already {
		t.Errorf("a unique-key collision should count as already, got %s", got)
	}
	missing := findResult(t, results, gwstaffapi.Create, ghost, "x")
	if missing.Outcome != failed {
		t.Errorf("a row naming a model that does not exist should be failed, got %s", missing.Outcome)
	}
	// A failure has to carry a code, or the caller is left guessing from prose
	// whether it is worth retrying.
	if missing.Code == nil || *missing.Code != errcode.CommonNotFound {
		t.Errorf("a missing-model failure should carry %s, got %v", errcode.CommonNotFound, missing.Code)
	}
	if missing.Detail == nil || *missing.Detail == "" {
		t.Error("a failure must carry a detail: it is the only clue the operator can read")
	}

	deleted := findResult(t, results, gwstaffapi.Delete, m2.Id, "to-delete")
	if deleted.Outcome != done {
		t.Errorf("deleting an existing route should be done, got %s", deleted.Outcome)
	}
	// A delete result has to carry the upstream name back: the caller matches
	// rows in its own table by the (model_id, provider_model_id) pair, and it
	// only sent a route_id in the request.
	if deleted.ProviderModelId != "to-delete" {
		t.Errorf("the delete result should return the upstream name, got %q", deleted.ProviderModelId)
	}
	if got := findResult(t, results, gwstaffapi.Delete, m2.Id, "").Outcome; got != already {
		t.Errorf("deleting a nonexistent route should count as already, got %s", got)
	}

	got := providerRouteUpstreams(t, s, prov)
	want := map[string]bool{"taken": true, "fresh": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("the end state should hold only taken and fresh, got %v", got)
	}
}

// A failed row does not take the successful ones with it.
//
// In Postgres any failing statement voids the whole transaction, so each row
// has to run on its own savepoint. Without savepoints, the symptom this test
// catches is that none of the successful rows reach the database either.
func TestBatchWireFailedRowDoesNotRollbackOthers(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	good := mustModel(t, s, "openai/good")
	// A nonexistent model id: the foreign key fails at the INSERT itself, so
	// this is a database-level error rather than one caught by validation
	// beforehand -- which is exactly the case that has to roll back only its
	// own row.
	ghost := uuid.New()

	results := mustBatch(t, s, prov, []gwstaffapi.GatewayRouteBatchCreate{
		batchCreate(good.Id, "a"),
		batchCreate(ghost, "b"),
		batchCreate(good.Id, "c"),
	}, nil)

	if got := findResult(t, results, gwstaffapi.Create, ghost, "b").Outcome; got != gwstaffapi.GatewayRouteBatchItemResultOutcomeFailed {
		t.Errorf("a nonexistent model should be failed, got %s", got)
	}
	for _, up := range []string{"a", "c"} {
		r := findResult(t, results, gwstaffapi.Create, good.Id, up)
		if r.Outcome != gwstaffapi.GatewayRouteBatchItemResultOutcomeDone {
			t.Errorf("%q should be done, got %s (%v)", up, r.Outcome, r.Detail)
		}
	}
	// A verdict of success is not enough; the truth is in the database.
	// Without savepoints this comes back empty.
	got := providerRouteUpstreams(t, s, prov)
	if len(got) != 2 {
		t.Fatalf("the two successful rows must actually land in the database, got %v", got)
	}
}

// The probe for "creates run before deletes".
//
// The order cannot be observed directly, so this builds a batch whose outcome
// differs depending on it: for one existing route, ask both to create the same
// one and to delete it.
//
//	create first: the create hits the unique key -> already; the delete
//	              succeeds -> done; final state has no route
//	delete first: the delete succeeds -> done; the create no longer conflicts
//	              -> done; final state has one new route
//
// The per-row verdicts and the final state both differ, so this test can really
// falsify the claim. Getting the order wrong matters: with deletes first, a
// batch whose creates all fail can leave the model with no routes at all.
func TestBatchWireCreatesRunBeforeDeletes(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	m := mustModel(t, s, "openai/order")
	mustRoute(t, s, m.Id, prov, "same", []string{"chat"})
	routeID := routeIDOf(t, s, m.Id, "same")

	results := mustBatch(t, s, prov,
		[]gwstaffapi.GatewayRouteBatchCreate{batchCreate(m.Id, "same")},
		[]gwstaffapi.GatewayRouteBatchDelete{{ModelId: m.Id, RouteId: routeID}})

	if got := findResult(t, results, gwstaffapi.Create, m.Id, "same").Outcome; got != gwstaffapi.GatewayRouteBatchItemResultOutcomeAlready {
		t.Errorf("with create-then-delete the create should hit the unique key and count as already, got %s -- the order is reversed", got)
	}
	if got := providerRouteUpstreams(t, s, prov); len(got) != 0 {
		t.Errorf("the end state of create-then-delete should hold no route, got %v -- the order is reversed", got)
	}
}

// A missing provider is a request-level error, not a per-row one: otherwise
// every row repeats the same "model or provider not found" and the reader has
// to get through fifty of them to learn there was only one problem.
func TestBatchWireUnknownProviderIsRequestLevel(t *testing.T) {
	s, _, _ := newServer(t)
	m := mustModel(t, s, "openai/x")
	_, err := s.BatchWireProviderRoutes(context.Background(),
		gwstaffapi.BatchWireProviderRoutesRequestObject{
			ProviderId: uuid.New(),
			Body: &gwstaffapi.GatewayRouteBatchInput{
				Creates: []gwstaffapi.GatewayRouteBatchCreate{batchCreate(m.Id, "a")},
				Deletes: []gwstaffapi.GatewayRouteBatchDelete{},
			},
		})
	var ce *httpx.CodeError
	if !errors.As(err, &ce) || ce.Code != errcode.CommonNotFound {
		t.Fatalf("a nonexistent provider should fail the whole request with %s, got %v", errcode.CommonNotFound, err)
	}
}

// routeIDOf looks a route id up by upstream name, since mustRoute does not
// return one.
func routeIDOf(t *testing.T, s *gwstaffapi.Server, model uuid.UUID, upstream string) uuid.UUID {
	t.Helper()
	res, err := s.ListGatewayRoutes(context.Background(),
		gwstaffapi.ListGatewayRoutesRequestObject{ModelId: model})
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	for _, r := range res.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items {
		if r.ProviderModelId == upstream {
			return r.Id
		}
	}
	t.Fatalf("model %s has no route with upstream name %q", model, upstream)
	return uuid.UUID{}
}

// ── The "upstream has it, the local catalog does not" row: create the catalog
// entry in place and wire it ──

// modelBySlug looks a model up by slug, to assert what was actually created.
func modelBySlug(t *testing.T, s *gwstaffapi.Server, slug string) *gwstaffapi.GatewayModel {
	t.Helper()
	res, err := s.ListGatewayModels(context.Background(),
		gwstaffapi.ListGatewayModelsRequestObject{})
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	for _, m := range res.(gwstaffapi.ListGatewayModels200JSONResponse).Items {
		if m.Slug == slug {
			return &m
		}
	}
	return nil
}

// A catalog entry created this way is always disabled and unpriced: the enable
// gate remains the only gate.
//
// This is the entire safety argument for letting an unknown row create its own
// model. The reason for banning automatic model creation was that it would
// manufacture models destined to fail closed; being born disabled is what makes
// that untrue, because no request can select them until somebody prices and
// enables them explicitly.
func TestBatchWireNewModelIsBornDisabled(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")

	results := mustBatch(t, s, prov, []gwstaffapi.GatewayRouteBatchCreate{
		batchCreateNew("openai/brand-new", "brand-new"),
	}, nil)

	if len(results) != 1 {
		t.Fatalf("the result should have 1 row: %+v", results)
	}
	r := results[0]
	if r.Outcome != gwstaffapi.GatewayRouteBatchItemResultOutcomeDone {
		t.Fatalf("it should be done, got %s (%v)", r.Outcome, r.Detail)
	}
	// The new model_id has to come back: the caller did not have it before,
	// and needs it afterwards to match the row to the catalog.
	if r.ModelId == nil {
		t.Fatal("creating a model must return model_id")
	}
	m := modelBySlug(t, s, "openai/brand-new")
	if m == nil {
		t.Fatal("the new entry should appear in the catalog")
	}
	if m.Id != *r.ModelId {
		t.Errorf("the returned model_id does not match the one in the catalog: %s vs %s", *r.ModelId, m.Id)
	}
	if m.Enabled {
		t.Error("a newly created catalog entry must always be disabled -- otherwise this really does produce a batch of models bound to fail closed")
	}
	// The route really was wired.
	if got := providerRouteUpstreams(t, s, prov); len(got) != 1 || got[0] != "brand-new" {
		t.Errorf("the route should be wired up, got %v", got)
	}
}

// A slug collision fails rather than reusing the model of the same name.
//
// Same name does not imply same protocol, so silently reusing it would attach
// this configuration to a different model, and the symptom of that is a
// run-time 404 -- indistinguishable from "the model was never created" or "the
// tier is not enabled". When a guess cannot be made confidently, ask rather
// than settle for something approximately right.
func TestBatchWireNewModelSlugConflictFailsInsteadOfReusing(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	existing := mustModel(t, s, "openai/taken-slug")

	results := mustBatch(t, s, prov, []gwstaffapi.GatewayRouteBatchCreate{
		batchCreateNew("openai/taken-slug", "some-upstream"),
	}, nil)

	if got := results[0].Outcome; got != gwstaffapi.GatewayRouteBatchItemResultOutcomeFailed {
		t.Fatalf("a slug collision should be failed, got %s", got)
	}
	if results[0].Code == nil || *results[0].Code != errcode.CommonConflict {
		t.Errorf("it should carry %s, got %v", errcode.CommonConflict, results[0].Code)
	}
	// Not a single route may be created: an implementation that reused the
	// same-named model would leave one behind here.
	if got := providerRouteUpstreams(t, s, prov); len(got) != 0 {
		t.Errorf("a slug collision must wire up no route at all, got %v", got)
	}
	// The existing model must be untouched as well.
	if m := modelBySlug(t, s, "openai/taken-slug"); m == nil || m.Id != existing.Id {
		t.Error("an existing model of the same name must not be replaced")
	}
}

// Creating the model and creating the route share one savepoint: if the route
// fails, the model must not survive either.
//
// Otherwise every failed wiring leaves an empty model nobody asked for in the
// catalog, holding a slug that cannot be changed -- so the next retry cannot
// even reuse the name.
func TestBatchWireNewModelRollsBackWhenRouteFails(t *testing.T) {
	s, pool, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	// No configuration-time rule refuses a route any more -- a model owns no
	// protocol, so there is nothing to mismatch -- which leaves the database
	// as the only place the route INSERT can fail after the model is in. A
	// trigger stands in for that: it fails exactly this row, and nothing
	// about the savepoint mechanism under test cares why the INSERT failed.
	if _, err := pool.Exec(context.Background(), `
		CREATE FUNCTION refuse_orphan() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.provider_model_id = 'orphan' THEN
				RAISE EXCEPTION 'injected: the route insert fails';
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER refuse_orphan BEFORE INSERT ON model_routes
			FOR EACH ROW EXECUTE FUNCTION refuse_orphan()`); err != nil {
		t.Fatal(err)
	}

	results := mustBatch(t, s, prov, []gwstaffapi.GatewayRouteBatchCreate{
		batchCreateNew("anthropic/orphan", "orphan"),
	}, nil)

	if got := results[0].Outcome; got != gwstaffapi.GatewayRouteBatchItemResultOutcomeFailed {
		t.Fatalf("a row whose route insert fails should be failed, got %s (%v)", got, results[0].Detail)
	}
	if m := modelBySlug(t, s, "anthropic/orphan"); m != nil {
		t.Error("when the route is not created, the catalog entry created along the way must roll back with it -- otherwise empty models pile up in the catalog")
	}
}

// Exactly one of model_id and new_model.
func TestBatchWireRejectsAmbiguousModelTarget(t *testing.T) {
	s, _, _ := newServer(t)
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	m := mustModel(t, s, "openai/both")

	both := batchCreate(m.Id, "x")
	both.NewModel = &gwstaffapi.GatewayRouteBatchNewModel{Slug: "openai/other"}
	neither := gwstaffapi.GatewayRouteBatchCreate{ProviderModelId: "y"}

	results := mustBatch(t, s, prov,
		[]gwstaffapi.GatewayRouteBatchCreate{both, neither}, nil)
	for i, r := range results {
		if r.Outcome != gwstaffapi.GatewayRouteBatchItemResultOutcomeFailed {
			t.Errorf("row %d should be failed, got %s", i, r.Outcome)
		}
	}
	// Neither form may leave a trace.
	if got := providerRouteUpstreams(t, s, prov); len(got) != 0 {
		t.Errorf("no route should have been created, got %v", got)
	}
	if m := modelBySlug(t, s, "openai/other"); m != nil {
		t.Error("when both are given, new_model must not be created")
	}
}

// One set, two projections, and both must read the same.
//
// "The model view and the provider view are two projections of one set" has
// exactly one executable form: change it from either side and the other side
// sees it immediately. An implementation that caches or filters separately on
// each side goes red here.
func TestRouteSetIsTheSameFromBothProjections(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	prov := mustProviderWith(t, s, []string{"openai"}, "https://up.test")
	m := mustModel(t, s, "openai/two-ways")

	// Write through the provider side.
	mustBatch(t, s, prov, []gwstaffapi.GatewayRouteBatchCreate{
		batchCreate(m.Id, "alias-1"),
		batchCreate(m.Id, "alias-2"),
	}, nil)

	fromProvider := providerRouteUpstreams(t, s, prov)
	byModel, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: m.Id})
	if err != nil {
		t.Fatal(err)
	}
	fromModel := make([]string, 0)
	for _, r := range byModel.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items {
		fromModel = append(fromModel, r.ProviderModelId)
	}
	if len(fromProvider) != 2 || len(fromModel) != 2 {
		t.Fatalf("both directions should see 2 rows: by provider %v / by model %v", fromProvider, fromModel)
	}

	// Delete one from the provider side; the model side must lose it at
	// once.
	del := routeIDOf(t, s, m.Id, "alias-1")
	mustBatch(t, s, prov, nil, []gwstaffapi.GatewayRouteBatchDelete{{ModelId: m.Id, RouteId: del}})

	after, err := s.ListGatewayRoutes(ctx, gwstaffapi.ListGatewayRoutesRequestObject{ModelId: m.Id})
	if err != nil {
		t.Fatal(err)
	}
	left := after.(gwstaffapi.ListGatewayRoutes200JSONResponse).Items
	if len(left) != 1 || left[0].ProviderModelId != "alias-2" {
		t.Errorf("the by-model view should hold only alias-2, got %+v", left)
	}
	if got := providerRouteUpstreams(t, s, prov); len(got) != 1 || got[0] != "alias-2" {
		t.Errorf("the by-provider view should hold only alias-2, got %v", got)
	}
}
