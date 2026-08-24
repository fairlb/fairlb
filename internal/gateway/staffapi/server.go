// Package gwstaffapi implements the gateway's staff (operator) API.
//
// The interfaces are generated from api/gateway-staff.yaml. Routes hang off the
// staff-plane subrouter and therefore inherit that plane's audit, idempotency
// and rate-limit middleware without restating any of it here.
package gwstaffapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fairlb/fairlb/foundation/strutil"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/alert"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalogadmin"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/discovery"
	"github.com/fairlb/fairlb/internal/gateway/gwhealth"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
	"github.com/fairlb/fairlb/internal/gateway/tiers"
)

// Server implements the generated StrictServerInterface.
type Server struct {
	q *gwdb.Queries
	// pool serves writes that need a transaction. Switching the default
	// admission tier, for instance, is two UPDATEs -- clear the old one, then
	// set the new one -- because model_tiers_single_default_uk takes effect
	// within the statement, so the intermediate state of a single UPDATE
	// cannot pass it. Doing it as two separate requests instead would open a
	// window in which there is no default tier at all.
	//
	// q is derived from this pool so the two can never point at different
	// data sources.
	pool    *pgxpool.Pool
	catalog *catalog.Service
	breaker breaker.Store
	budget  *proxy.RetryBudget
	// box encrypts and decrypts provider credentials; the AAD binds each
	// ciphertext to its row id.
	box *crypto.Box
	// httpClient is used by the connectivity probe. When nil a default client
	// with a timeout is used.
	httpClient *http.Client
	// cache invalidates the data plane's key cache after a organization's tier or
	// discount changes, since the cached identity carries both values.
	// nil means the data plane reads straight from the database, so there is
	// no stale copy to clear.
	cache cache.Store
	// pricingAlerter surfaces "the price was written but invalidating the
	// cache failed" to operators. A failed invalidation must not disguise a
	// successful write as an HTTP failure: the database commit is the
	// authority and the TTL is the backstop.
	pricingAlerter Alerter
	// river enqueues the per-endpoint probe. It is optional: when nil the
	// initial rows are still written and probing is left to a manual trigger.
	// Probe results are extra information, and missing them must not fail
	// creating a route.
	river *river.Client[pgx.Tx]
	// routeProbe owns everything about "does this route answer": seeding the
	// initial rows, enqueueing the job, resetting a stale verdict, reading them
	// back. See internal/gateway/routeprobe.
	routeProbe *routeprobe.Service
	// tiers owns admission: which models an organization may reach.
	tiers *tiers.Service
	// discovery asks a provider what models it actually serves.
	discovery *discovery.Service
	// catalogAdmin is the write side of the catalog: models, routes, keys.
	catalogAdmin *catalogadmin.Service
	// traceEnabled decides whether the connectivity probe may return the full
	// exchange, which includes the credential in clear text.
	//
	// The zero value of false is the safe side: an assembly point that forgets
	// to wire this gets "the feature is unavailable" rather than "plaintext
	// credentials appear in an API response".
	traceEnabled bool
	// pricingAdmin owns the transaction boundary for pricing writes: publishing
	// a price touches several rows and an audit entry, and that has to happen in
	// one transaction. Handlers hold the concrete type rather than an interface
	// — there is one implementation, NewServer builds it unconditionally, and
	// nothing can substitute another (ADR-0157).
	pricingAdmin *pgPricingAdminService
	// health assembles the operator dashboard: the request rollup, the latency
	// histogram, the in-memory breaker state and the kill-switch counts.
	health *gwhealth.Reader
}

// ServerConfig contains every production dependency of the gateway staff API.
// The application assembly path supplies this value once, so initialization
// order cannot change behavior.
type ServerConfig struct {
	Pool           *pgxpool.Pool
	Catalog        *catalog.Service
	Breaker        breaker.Store
	Budget         *proxy.RetryBudget
	Cipher         *crypto.Box
	HTTPClient     *http.Client
	Cache          cache.Store
	PricingAlerter Alerter
	Jobs           *river.Client[pgx.Tx]
	ProbeTrace     bool
}

// Alerter reports cache-invalidation failures after a pricing write commits
// (ADR-0190: one declaration, in foundation).
type Alerter = alert.Sink

// NewServer constructs a fully wired staff server.
func NewServer(cfg ServerConfig) *Server {
	// The probe service is a local first because two of the services below take
	// it. Building it twice would give the catalog writer a second Reprobe path
	// with its own job client — and "which of the two enqueued it" is not a
	// question anybody should have to ask.
	probes := routeprobe.NewService(cfg.Pool, cfg.Jobs)
	invalidator := catalogInvalidatorOrNil(cfg.Catalog)
	return &Server{
		pool:    cfg.Pool,
		catalog: cfg.Catalog, box: cfg.Cipher,
		httpClient: cfg.HTTPClient, cache: cfg.Cache, pricingAlerter: cfg.PricingAlerter,
		river: cfg.Jobs, traceEnabled: cfg.ProbeTrace,
		routeProbe:   probes,
		tiers:        tiers.NewService(cfg.Pool, invalidator),
		discovery:    discovery.NewService(cfg.Pool, cfg.Cipher),
		catalogAdmin: catalogadmin.NewService(cfg.Pool, cfg.Jobs, probes, invalidator),
		pricingAdmin: NewPGPricingAdminService(PGPricingAdminConfig{Pool: cfg.Pool, Catalog: cfg.Catalog}),
		health:       gwhealth.NewReader(cfg.Pool, cfg.Breaker, budgetOrNil(cfg.Budget)),
	}
}

// cacheInvalidator is the one method both the tier service and the catalog
// writer need from the catalog. Declared here so the nil-guard below has one
// return type rather than one per consumer.
type cacheInvalidator interface {
	InvalidateAll(ctx context.Context)
}

// tierInvalidator keeps a nil catalog a nil interface.
//
// Assigning a nil *catalog.Service straight into the interface would make it
// non-nil while holding a nil pointer, and the domain's `!= nil` guard would
// then call a method on it. The same trap as ADR-0174's, and worth one function
// rather than one comment: it is not visible at the call site.
// catalogInvalidatorOrNil keeps a nil catalog a nil interface.
//
// The return type has to be the **interface**, not *catalog.Service. Returning
// the pointer and letting the call site convert puts the typed-nil straight
// back: a nil pointer in an interface is a non-nil interface, and every
// consumer's `!= nil` guard would then call a method on it. Interface-to-
// interface conversion is what preserves nil-ness, which is why one interface
// declared here feeds both consumers' own.
//
// Third time this trap has come up in this refactor (ADR-0174, ADR-0176), and
// the first two fixes were per-consumer. This one is the shape that cannot be
// got wrong at a call site, because the call site has no conversion to make.
func catalogInvalidatorOrNil(c *catalog.Service) cacheInvalidator {
	if c == nil {
		return nil
	}
	return c
}

var _ StrictServerInterface = (*Server)(nil)

// RequireStaff is the authorization middleware covering every staff endpoint in
// this package.
//
// It is middleware rather than a call at the top of each handler because the
// per-handler form has already failed once here: the staff plane's auth
// middleware only populates the identity, it does not reject anything, so a
// handler that forgets its own check is wide open to unauthenticated callers --
// including the ones that create credentials and edit routes.
//
// A per-handler call is a convention you have to remember, and conventions get
// forgotten. Middleware is a structure you cannot forget: a new endpoint is
// covered the moment it is registered, which is exactly the property this
// package needs.
func RequireStaff(f StrictHandlerFunc, _ string) StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if _, err := httpx.RequireUserID(ctx); err != nil {
			return nil, err
		}
		return f(ctx, w, r, request)
	}
}

// ListGatewayProviders lists providers, including health state and the
// auto-disabled flag.
func (s *Server) ListGatewayProviders(ctx context.Context, req ListGatewayProvidersRequestObject) (ListGatewayProvidersResponseObject, error) {
	page, err := httpx.ParseKeyPage(
		req.Params.Cursor, req.Params.Limit, catalogadmin.SlugCursorParts, 50, 200)
	if err != nil {
		return nil, err
	}
	providers, err := s.catalogAdmin.Providers(ctx, derefOr(req.Params.Q, ""), page)
	if err != nil {
		return nil, providerHTTPError(err)
	}
	kept, more := cursorpage.Trim(providers, int(page.Limit))
	data := make([]GatewayProvider, 0, len(kept))
	for _, p := range kept {
		data = append(data, providerWithCountsDTO(p))
	}
	resp := ListGatewayProviders200JSONResponse{Items: data}
	if more {
		nc := catalogadmin.ProviderCursor(kept[len(kept)-1])
		resp.NextCursor = &nc
	}
	return resp, nil
}

// GetGatewayProvider reads one provider.
func (s *Server) GetGatewayProvider(ctx context.Context, req GetGatewayProviderRequestObject) (GetGatewayProviderResponseObject, error) {
	p, err := s.catalogAdmin.Provider(ctx, req.ProviderId)
	if err != nil {
		return nil, providerHTTPError(err)
	}
	return GetGatewayProvider200JSONResponse(providerWithCountsDTO(p)), nil
}

// CreateGatewayProvider creates a provider.
func (s *Server) CreateGatewayProvider(ctx context.Context, req CreateGatewayProviderRequestObject) (CreateGatewayProviderResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation,
			"slug, vendor, protocols and base_url are required")
	}
	p, err := s.catalogAdmin.CreateProvider(ctx, catalogadmin.ProviderCreate{
		Slug: derefOr(in.Slug, ""), Vendor: derefOr(in.Vendor, ""),
		BaseURL: derefOr(in.BaseUrl, ""), Name: derefOr(in.Name, ""),
		Protocols: protocolsIn(in.Protocols),
		Headers:   in.Headers, Transport: in.Transport,
		CostMultiplierBps: int32Of(in.CostMultiplierBps),
		RateLimitRPM:      int32Of(in.RateLimitRpm),
		RateLimitTPM:      int32Of(in.RateLimitTpm),
		MaxConcurrency:    int32Of(in.MaxConcurrency),
	})
	if err != nil {
		return nil, providerHTTPError(err)
	}
	return CreateGatewayProvider201JSONResponse(providerDTO(p)), nil
}

// UpdateGatewayProvider partially updates a provider.
func (s *Server) UpdateGatewayProvider(ctx context.Context, req UpdateGatewayProviderRequestObject) (UpdateGatewayProviderResponseObject, error) {
	in := req.Body
	if in == nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "A request body is required")
	}
	patch := catalogadmin.ProviderPatch{
		Vendor: in.Vendor, BaseURL: in.BaseUrl, Name: in.Name, Enabled: in.Enabled,
		Headers: in.Headers, Transport: in.Transport,
		CostMultiplierBps: int32Of(in.CostMultiplierBps),
		RateLimitRPM:      int32Of(in.RateLimitRpm),
		RateLimitTPM:      int32Of(in.RateLimitTpm),
		MaxConcurrency:    int32Of(in.MaxConcurrency),
	}
	if in.Protocols != nil {
		declared := protocolsIn(in.Protocols)
		patch.Protocols = &declared
	}
	// `clear` is how a partial update asks for a ceiling to be removed: absent
	// and "set back to no limit" are different intentions, and an omitted field
	// can only express the first.
	if in.Clear != nil {
		for _, c := range *in.Clear {
			switch c {
			case RateLimitRpm:
				patch.ClearRateLimitRPM = true
			case RateLimitTpm:
				patch.ClearRateLimitTPM = true
			}
		}
	}
	p, err := s.catalogAdmin.UpdateProvider(ctx, req.ProviderId, patch)
	if err != nil {
		return nil, providerHTTPError(err)
	}
	return UpdateGatewayProvider200JSONResponse(providerDTO(p)), nil
}

// providerHTTPError maps the provider writer's refusals.
//
// It shares routeHTTPError's vocabulary and adds the two answers that are this
// surface's own: its "not found" sentence, and the 422 the cost multiplier has
// answered since it existed. An out-of-range multiplier is well-formed and
// correctly typed -- the request is not wrong on its own terms, the value is
// simply outside what the schema holds -- so it is not the same answer as a 400.
func providerHTTPError(err error) error {
	var outOfRange catalogadmin.OutOfRangeError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &outOfRange):
		return httpx.ErrCodeDetail(errcode.CommonUnprocessable, outOfRange.Message)
	case errors.Is(err, catalogadmin.ErrNotFound):
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Provider not found")
	default:
		return routeHTTPError(err)
	}
}

// providerDTO maps one provider to its wire shape. Every path shares it;
// otherwise they drift apart, and "the detail view stopped rendering one field"
// is not something any gate can see.
func providerDTO(p catalogadmin.Provider) GatewayProvider {
	out := GatewayProvider{
		Id: p.ID, Slug: p.Slug, Vendor: p.Vendor, Protocols: protocolsOut(p.Protocols),
		BaseUrl: p.BaseURL, Enabled: p.Enabled, AutoDisabled: p.AutoDisabled,
		Name: strutil.Ptr(p.Name), Headers: mapPtr(p.Headers), Transport: mapPtr(p.Transport),
		CostMultiplierBps: int(p.CostMultiplierBps),
		MaxConcurrency:    int(p.MaxConcurrency),
	}
	out.RateLimitRpm, out.RateLimitTpm = intPtrOf(p.RateLimitRPM), intPtrOf(p.RateLimitTPM)
	return out
}

// providerWithCountsDTO is the read shape: the same fields plus how many keys
// and routes hang off this provider.
//
// The write paths return the row they just wrote, which does not carry those
// counts -- they are aggregates over other tables, not columns of this one --
// so they answer without them rather than with a made-up zero.
func providerWithCountsDTO(p catalogadmin.Provider) GatewayProvider {
	out := providerDTO(p)
	out.KeyCount, out.RouteCount = intPtr(int(p.KeyCount)), intPtr(int(p.RouteCount))
	return out
}

// protocolsIn converts the contract's enum into the plain dialect names the
// domain works in. Whether each one is a dialect this build knows is the
// domain's question, not this one's.
func protocolsIn(in *[]GatewayProviderInputProtocols) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(*in))
	for _, f := range *in {
		out = append(out, string(f))
	}
	return out
}

// mapPtr renders an empty map as absent. The editor treats absent as "nothing
// configured", and an object that is present but says nothing is a third state
// nobody needs.
func mapPtr[T any](m map[string]T) *map[string]T {
	if len(m) == 0 {
		return nil
	}
	return &m
}

// int32Of and intPtrOf convert between the contract's int and the domain's
// int32, keeping "not set" as nil on both sides.
func int32Of(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func intPtrOf(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// ListGatewayModels lists the model catalog, including the union of route
// capabilities and the pricing summary.
func (s *Server) ListGatewayModels(ctx context.Context, req ListGatewayModelsRequestObject) (ListGatewayModelsResponseObject, error) {
	models, err := s.catalogAdmin.AdminModels(ctx, derefOr(req.Params.Q, ""))
	if err != nil {
		return nil, modelHTTPError(err)
	}
	data := make([]GatewayModel, 0, len(models))
	for _, m := range models {
		data = append(data, s.modelOut(ctx, m))
	}
	return ListGatewayModels200JSONResponse{Items: data}, nil
}

// GetGatewayModel reads one model.
//
// Pricing enrichment goes through the same modelOut, so the detail view's
// pricing status and margin flag come from the same place as the list's.
func (s *Server) GetGatewayModel(ctx context.Context, req GetGatewayModelRequestObject) (GetGatewayModelResponseObject, error) {
	m, err := s.catalogAdmin.AdminModel(ctx, req.ModelId)
	if err != nil {
		return nil, modelHTTPError(err)
	}
	return GetGatewayModel200JSONResponse(s.modelOut(ctx, m)), nil
}

// modelHTTPError is providerHTTPError's sibling, differing only in the sentence
// a missing row gets.
func modelHTTPError(err error) error {
	if errors.Is(err, catalogadmin.ErrNotFound) {
		return httpx.ErrCodeDetail(errcode.CommonNotFound, "Model not found")
	}
	return routeHTTPError(err)
}

// modelOut maps one model to its wire shape, pricing included. List and detail
// share it.
//
// Two domains meet here and neither owns the other: the catalog says what the
// model is, pricing says what it costs. Composing them is the transport's job
// precisely because it is the only layer that has to answer with both.
func (s *Server) modelOut(ctx context.Context, r catalogadmin.AdminModel) GatewayModel {
	m := GatewayModel{
		Id: r.ID, Slug: r.Slug, Enabled: r.Enabled,
		Visibility: GatewayModelVisibility(r.Visibility),
		Endpoints:  r.Endpoints, Protocols: r.Protocols,
		// The staff UI tells "deliberately free" from "nobody set a price"
		// using the free flag below. Without it a perfectly healthy free model
		// is labelled "unpriced, refusing traffic" -- an outage that does not
		// exist.
		DisplayName:     strutil.Ptr(r.DisplayName),
		RouteCount:      intPtr(int(r.RouteCount)),
		ContextWindow:   intPtr(int(r.ContextWindow)),
		MaxOutputTokens: intPtr(int(r.MaxOutputTokens)),
		Metadata:        mapPtr(r.Metadata),
	}
	if s.pricingAdmin != nil {
		// Pricing has no draft or pending state: a model either has a price or
		// it does not, and if it does, the price is either free or not.
		pricing, _, pricingErr := s.pricingAdmin.GetModelPricing(ctx, r.ID)
		if pricingErr == nil {
			status := GatewayModelPricingStatusUnpriced
			if pricing.Priced {
				m.PublicRates = pricing.PublicRates
				m.PriceUpdatedAt = pricing.UpdatedAt
				// Whether a person has confirmed this rate against the
				// vendor's own list. Sent as an explicit false rather than by
				// leaving the field out, because absence has to keep meaning
				// "there is no price row here": a list that read a missing
				// field as "unverified" would mark every unpriced model as
				// unverified too, which is a different sentence about a
				// different problem.
				verified := pricing.CheckedAt != nil
				m.PriceVerified = &verified
				isFree := pricing.BillingMode != nil &&
					*pricing.BillingMode == ModelPricingResourceBillingModeFree
				m.IsFree = &isFree
				status = GatewayModelPricingStatusActive
				if isFree {
					status = GatewayModelPricingStatusFree
				}
			}
			m.PricingStatus = &status
		}
	}
	return m
}

// GetGatewayHealth assembles the health dashboard.
func (s *Server) GetGatewayHealth(ctx context.Context, _ GetGatewayHealthRequestObject) (GetGatewayHealthResponseObject, error) {
	snap, err := s.health.Read(ctx)
	if err != nil {
		return nil, err
	}
	resp := GatewayHealth{Providers: make([]GatewayProviderHealth, 0, len(snap.Providers))}
	for _, p := range snap.Providers {
		h := GatewayProviderHealth{
			ProviderId:    p.ID,
			Slug:          p.Slug,
			BreakerStatus: GatewayProviderHealthBreakerStatus(p.BreakerStatus),
			CooldownUntil: p.CooldownUntil,
			Requests1h:    p.Requests1h,
			Errors1h:      p.Errors1h,
		}
		if p.Latency1h != nil {
			h.Latency1h = latencyDTO(*p.Latency1h)
		}
		resp.Providers = append(resp.Providers, h)
	}
	resp.RetryBudget.Requests, resp.RetryBudget.Retries = snap.RetryBudget.Requests, snap.RetryBudget.Retries
	if c := snap.SwitchCounts; c != nil {
		resp.SwitchCounts = &GatewayKillSwitchCounts{
			ProvidersTotal: c.ProvidersTotal, ProvidersDisabled: c.ProvidersDisabled,
			ModelsTotal: c.ModelsTotal, ModelsDisabled: c.ModelsDisabled,
		}
	}
	return GetGatewayHealth200JSONResponse(resp), nil
}

// latencyDTO renders the latency column.
//
// With no samples it reports no number at all: a 0ms reading gets read as
// "unbelievably fast", which is a different claim from "this window observed
// nothing".
func latencyDTO(l gwhealth.Latency) *GatewayProviderLatency {
	out := GatewayProviderLatency{HasSamples: l.HasSamples}
	if !l.HasSamples {
		return &out
	}
	p50, p95, mean, unbounded := l.P50Ms, l.P95Ms, l.MeanMs, l.P95Unbounded
	out.P50Ms, out.P95Ms, out.MeanMs, out.P95Unbounded = &p50, &p95, &mean, &unbounded
	return &out
}

// protocolsOut converts the stored set of dialects into the contract's enum.
func protocolsOut(in []string) []GatewayProviderProtocols {
	out := make([]GatewayProviderProtocols, 0, len(in))
	for _, f := range in {
		out = append(out, GatewayProviderProtocols(f))
	}
	return out
}

func intPtr(v int) *int { return &v }

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// budgetOrNil keeps a nil retry budget a nil interface.
//
// Same trap as catalogInvalidatorOrNil above: assigning a nil *proxy.RetryBudget
// straight into gwhealth.Budget would make the interface non-nil while holding a
// nil pointer, and the dashboard's `!= nil` guard would then call Stats on it.
// The return type has to be the interface for the conversion to preserve
// nil-ness.
func budgetOrNil(b *proxy.RetryBudget) gwhealth.Budget {
	if b == nil {
		return nil
	}
	return b
}

// transportOut renders a stored transport profile back out. An empty profile
// comes back absent rather than as an empty object: the editor treats absent as
// "nothing configured", and an object that is present but says nothing is a
// third state nobody needs.
func transportOut(raw []byte) *map[string]any {
	var m map[string]any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return mapPtr(m)
}
