// Package catalogadmin is the write side of the gateway catalog: which models
// exist, which providers serve them, and with which credentials.
//
// It is a separate package from `catalog`, which is the read side and is mostly
// cache. They answer different questions and fail differently -- a stale read
// serves an old price, a bad write creates a configuration that can never be
// selected -- and the rules that belong to writing all have the same shape:
// **refuse at configuration time what would otherwise only show up at run time
// as a 404 nobody can attribute.**
package catalogadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

var (
	// ErrNotFound is "no such model, provider or route".
	ErrNotFound = errors.New("catalogadmin: not found")
	// ErrDuplicate is a route that already maps this model to that upstream
	// name on that provider.
	ErrDuplicate = errors.New("catalogadmin: already exists")
)

// InvalidError is a request the rules refuse, carrying the sentence that says
// why.
//
// The sentence is domain knowledge, not presentation: it explains which of two
// protocols does not match and what to do about it, and that explanation is the
// entire value of refusing on write instead of leaving it to run time.
type InvalidError struct{ Message string }

func (e InvalidError) Error() string { return "catalogadmin: " + e.Message }

func invalid(format string, args ...any) error {
	return InvalidError{Message: fmt.Sprintf(format, args...)}
}

// ConflictError is a request refused because the world already holds something
// incompatible — as opposed to InvalidError, which is a request that is wrong
// on its own terms.
//
// The two are separate because they answer different questions for the caller:
// a conflict says "change the world or pick another name", a validation error
// says "change what you sent". Collapsing them was a real regression this
// refactor introduced and the batch test caught: a slug collision came back as
// 400 instead of 409.
type ConflictError struct{ Message string }

func (e ConflictError) Error() string { return "catalogadmin: " + e.Message }

// Invalidator is told the catalog changed.
type Invalidator interface {
	InvalidateAll(ctx context.Context)
}

// Service performs catalog writes.
type Service struct {
	pool        *pgxpool.Pool
	q           *gwdb.Queries
	river       *river.Client[pgx.Tx]
	probes      *routeprobe.Service
	invalidator Invalidator
}

func NewService(
	pool *pgxpool.Pool, jobs *river.Client[pgx.Tx],
	probes *routeprobe.Service, invalidator Invalidator,
) *Service {
	return &Service{
		pool: pool, q: gwdb.New(pool), river: jobs,
		probes: probes, invalidator: invalidator,
	}
}

func (s *Service) invalidate(ctx context.Context) {
	if s.invalidator != nil {
		s.invalidator.InvalidateAll(ctx)
	}
}

func pgID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// Route is one upstream mapping.
type Route struct {
	ID      uuid.UUID
	ModelID uuid.UUID
	// ModelSlug and ProviderSlug travel with the route rather than being looked
	// up in the two catalogues, both of which are read a page at a time
	// (ADR-0187). A whole-set editor that resolved them by id would show a live
	// entry as if it had been deleted, purely because it sat on a later page.
	ModelSlug  string
	ProviderID uuid.UUID
	// ProviderSlug and ProviderProtocols likewise: the protocols say which
	// endpoints this route can be asked about, which is what the probe rows
	// are keyed on.
	ProviderSlug      string
	ProviderProtocols []string
	ProviderModelID   string
	Priority          int32
	Weight            int32
	Enabled           bool
	// Headers and Quirks are decoded here rather than carried as bytes: they
	// are a map in the contract and a map in the database, and the only place
	// they are ever bytes is the wire between the two.
	Headers         map[string]string
	Quirks          map[string]any
	ContextWindow   *int32
	MaxOutputTokens *int32
	// Verdicts is what is known per endpoint -- the route's only capability
	// record -- filled by the listing calls only.
	Verdicts []routeprobe.Verdict
}

// RouteCreate is a new route. A nil optional takes the column's default. There
// are no endpoints to declare: the route is probed on every endpoint of the
// protocols its provider speaks, and the verdicts are what it serves.
type RouteCreate struct {
	ModelID         uuid.UUID
	ProviderID      uuid.UUID
	ProviderModelID string
	Priority        *int32
	Weight          *int32
	Enabled         *bool
	Headers         *map[string]string
	Quirks          *map[string]any
	ContextWindow   *int32
	MaxOutputTokens *int32
}

// RoutePatch is a partial update; a nil field is left alone.
//
// The provider is absent on purpose: changing it would silently repoint a route
// at a different upstream while keeping the probe verdict that was about the
// old one.
type RoutePatch struct {
	ProviderModelID *string
	Priority        *int32
	Weight          *int32
	Enabled         *bool
	Headers         *map[string]string
	Quirks          *map[string]any
	ContextWindow   *int32
	MaxOutputTokens *int32
}

// routeParties reads the two ends of a route before it is created: it answers
// "do both exist" (zero rows means one does not) and hands back the provider's
// protocol set, which is what the route's probe rows are seeded from.
//
// There is no protocol rule to apply. A model owns no protocol, so there is
// nothing on its side that could fail to match; a provider may carry any model
// on the protocols it speaks. What used to be refused here at configuration
// time -- a route that could never be selected -- can no longer be configured,
// because the configuration no longer makes the claim that was being checked.
//
// It takes a querier rather than reaching for the pool, so a caller that
// already has a transaction open runs this read on the same connection.
// Asking the pool for a second connection while an outer transaction holds
// the first is, on a small pool, a deadlock against yourself.
func routeParties(
	ctx context.Context, q *gwdb.Queries, modelID, providerID uuid.UUID,
) (providerProtocols []string, err error) {
	row, err := q.RouteParties(ctx, gwdb.RoutePartiesParams{
		ModelID: pgID(modelID), ProviderID: pgID(providerID),
	})
	if db.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: query the route's model and provider: %w", err)
	}
	return row.ProviderProtocols, nil
}

func decodeHeaders(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// encodeMap and encodeMapOrNull are a pair: the first is for creation, where an
// omitted value means an empty object, the second for partial updates, where an
// omitted value means "leave it alone". Both columns are NOT NULL and the
// INSERT names them explicitly, so a nil on create would insert NULL and
// violate the constraint on the spot rather than fall back to the default.
func encodeMap[T any](m *map[string]T) []byte {
	if m == nil {
		return []byte(`{}`)
	}
	b, err := json.Marshal(*m)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func encodeMapOrNull[T any](m *map[string]T) []byte {
	if m == nil {
		return nil
	}
	return encodeMap(m)
}

func int32Ptr(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	n := v.Int32
	return &n
}

func int4(v *int32) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *v, Valid: true}
}

// routeFrom shapes a write's returned row. The provider's protocols are not
// on that row -- the write touches model_routes alone -- so the caller passes
// what it read; the contract marks the field required, and a missing value
// would serialise as null against a declared array.
func routeFrom(r gwdb.CreateRouteRow, providerProtocols []string) Route {
	if providerProtocols == nil {
		providerProtocols = []string{}
	}
	return Route{
		ID: uuid.UUID(r.ID.Bytes), ModelID: uuid.UUID(r.ModelID.Bytes),
		ProviderID: uuid.UUID(r.ProviderID.Bytes), ProviderProtocols: providerProtocols,
		ProviderModelID: r.ProviderModelID,
		Priority:        r.Priority, Weight: r.Weight, Enabled: r.Enabled,
		Headers: decodeHeaders(r.Headers), Quirks: decodeJSONObject(r.Quirks),
		ContextWindow: int32Ptr(r.ContextWindow), MaxOutputTokens: int32Ptr(r.MaxOutputTokens),
	}
}

func routeFromAdminRow(r gwdb.ListRoutesForAdminRow, verdicts []routeprobe.Verdict) Route {
	return Route{
		ID: uuid.UUID(r.ID.Bytes), ModelID: uuid.UUID(r.ModelID.Bytes),
		ModelSlug:  r.ModelSlug,
		ProviderID: uuid.UUID(r.ProviderID.Bytes), ProviderSlug: r.ProviderSlug,
		ProviderProtocols: r.ProviderProtocols, ProviderModelID: r.ProviderModelID,
		Priority: r.Priority, Weight: r.Weight, Enabled: r.Enabled,
		Headers: decodeHeaders(r.Headers), Quirks: decodeJSONObject(r.Quirks),
		ContextWindow: int32Ptr(r.ContextWindow), MaxOutputTokens: int32Ptr(r.MaxOutputTokens),
		Verdicts: verdicts,
	}
}

// RoutesForModel lists a model's routes, with each one's probe verdicts.
func (s *Service) RoutesForModel(ctx context.Context, modelID uuid.UUID) ([]Route, error) {
	rows, err := s.q.ListRoutesForAdmin(ctx, pgID(modelID))
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: list routes: %w", err)
	}
	if err := wholeSetWithinBound(len(rows), "routes of this model"); err != nil {
		return nil, err
	}
	verdicts := s.probes.VerdictsForModel(ctx, pgID(modelID))
	out := make([]Route, 0, len(rows))
	for _, r := range rows {
		out = append(out, routeFromAdminRow(r, verdicts[uuid.UUID(r.ID.Bytes)]))
	}
	return out, nil
}

// RoutesForProvider reads the same routes from the other direction.
//
// It deliberately includes disabled routes. That differs on purpose from
// route_count, which counts only the enabled ones: the count answers "is it
// carrying traffic", this answers "what is configured", and a disabled route is
// exactly the kind of thing that most needs to be seen and turned back on.
func (s *Service) RoutesForProvider(ctx context.Context, providerID uuid.UUID) ([]Route, error) {
	rows, err := s.q.ListRoutesForProviderAdmin(ctx, pgID(providerID))
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: list routes by provider: %w", err)
	}
	if err := wholeSetWithinBound(len(rows), "routes of this provider"); err != nil {
		return nil, err
	}
	verdicts := s.probes.VerdictsForProvider(ctx, pgID(providerID))
	out := make([]Route, 0, len(rows))
	for _, r := range rows {
		// The two queries select identical columns, so one row type converts
		// straight into the other. Not to save keystrokes: it turns "somebody
		// changed the columns on one side only" into a compile error.
		out = append(out, routeFromAdminRow(gwdb.ListRoutesForAdminRow(r), verdicts[uuid.UUID(r.ID.Bytes)]))
	}
	return out, nil
}

// CreateRoute wires a model to an upstream name on a provider.
func (s *Service) CreateRoute(ctx context.Context, in RouteCreate) (Route, error) {
	providerProtocols, err := routeParties(ctx, s.q, in.ModelID, in.ProviderID)
	if err != nil {
		return Route{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Route{}, fmt.Errorf("catalogadmin: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	row, err := qtx.CreateRoute(ctx, gwdb.CreateRouteParams{
		ModelID: pgID(in.ModelID), ProviderID: pgID(in.ProviderID),
		ProviderModelID: in.ProviderModelID,
		Priority:        derefOr(in.Priority, 100), Weight: derefOr(in.Weight, 1),
		Enabled: derefOr(in.Enabled, true),
		Headers: encodeMap(in.Headers), Quirks: encodeMap(in.Quirks),
		ContextWindow: int4(in.ContextWindow), MaxOutputTokens: int4(in.MaxOutputTokens),
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Route{}, ErrDuplicate
		}
		return Route{}, fmt.Errorf("catalogadmin: create route: %w", err)
	}
	// The probe is enqueued in the same transaction that creates the route. A
	// route written without its job enqueued sits at "unverified" forever with
	// nothing but the sweeper coming along to fix it.
	if err := routeprobe.Enqueue(ctx, qtx, s.river, tx, row.ID, providerProtocols); err != nil {
		return Route{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Route{}, fmt.Errorf("catalogadmin: commit route: %w", err)
	}
	s.invalidate(ctx)
	return routeFrom(row, providerProtocols), nil
}

// UpdateRoute applies a partial change.
func (s *Service) UpdateRoute(
	ctx context.Context, modelID, routeID uuid.UUID, in RoutePatch,
) (Route, error) {
	parties, err := s.q.RouteUnderModel(ctx, gwdb.RouteUnderModelParams{
		RouteID: pgID(routeID), ModelID: pgID(modelID),
	})
	if db.IsNoRows(err) {
		return Route{}, ErrNotFound
	}
	if err != nil {
		return Route{}, fmt.Errorf("catalogadmin: query the route: %w", err)
	}
	row, err := s.q.UpdateRoute(ctx, gwdb.UpdateRouteParams{
		ID: pgID(routeID), ModelID: pgID(modelID),
		ProviderModelID: textOrNull(in.ProviderModelID),
		Priority:        int4(in.Priority), Weight: int4(in.Weight),
		Enabled: boolOrNull(in.Enabled),
		Headers: encodeMapOrNull(in.Headers), Quirks: encodeMapOrNull(in.Quirks),
		ContextWindow: int4(in.ContextWindow), MaxOutputTokens: int4(in.MaxOutputTokens),
	})
	if err != nil {
		if db.IsNoRows(err) {
			return Route{}, ErrNotFound
		}
		return Route{}, fmt.Errorf("catalogadmin: update route: %w", err)
	}
	// Changing the upstream model name requires a fresh probe: the old verdict
	// was about the old name, and keeping it is worse than having none,
	// because the operator reads that green as still true.
	if in.ProviderModelID != nil {
		s.probes.Reprobe(ctx, pgID(routeID))
	}
	s.invalidate(ctx)
	return routeFrom(gwdb.CreateRouteRow(row), parties.ProviderProtocols), nil
}

// DeleteRoute removes a route.
func (s *Service) DeleteRoute(ctx context.Context, modelID, routeID uuid.UUID) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("catalogadmin: begin route-delete transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM model_routes WHERE id=$1 AND model_id=$2 FOR UPDATE`,
		pgID(routeID), pgID(modelID)).Scan(&locked); err != nil {
		if db.IsNoRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("catalogadmin: lock route: %w", err)
	}
	// The cost audit trail lives in the per-request snapshot on each usage log
	// row, not in any history kept on the route, so deleting a route leaves
	// past bills just as explainable as before.
	n, err := s.q.WithTx(tx).DeleteRoute(ctx, gwdb.DeleteRouteParams{
		ID: pgID(routeID), ModelID: pgID(modelID),
	})
	if err != nil {
		return fmt.Errorf("catalogadmin: delete route: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("catalogadmin: commit route delete: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

func derefOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

func textOrNull(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *v, Valid: true}
}

func boolOrNull(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

// wholeSetBound is the most rows a whole-set editor may be handed. The queries
// behind those editors fetch one more than this, so a full page is proof the
// bound bit rather than a coincidence.
const wholeSetBound = 500

// wholeSetWithinBound refuses a whole-set list that overflowed its bound. The
// editors fed by these lists compute deletions from what they were given: a
// row missing from a silently truncated list reads as "unticked" and is
// deleted on save. Refusing to open is the only safe answer; the way out is to
// narrow with search (ADR-0187) or split the editor.
func wholeSetWithinBound(n int, what string) error {
	if n <= wholeSetBound {
		return nil
	}
	return httpx.ErrCodeDetail(errcode.CommonUnprocessable,
		"More than "+strconv.Itoa(wholeSetBound)+" "+what+"; the whole-set editor cannot be opened safely. Narrow the selection first.")
}
