// Package catalog owns the gateway's model catalog, its routing candidates and
// its pricing. This package is the read path; the operator CRUD that writes
// providers and models lives with the staff API.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/settings"
)

// catalogTTL is the backstop lifetime of the catalog cache. A write from the
// operator page invalidates it actively; the TTL only bounds how stale things
// can get when that notification is lost.
const catalogTTL = 60 * time.Second

// visibilityPublic is the only visibility that can be called. Anything else is
// not callable: the difference between those states exists in the operator UI,
// not on the data plane.
const visibilityPublic = "public"

// Surface is the public API surface, which decides the protocol and the endpoint
// filter applied to candidates. Requests pass through within a protocol without
// protocol translation.
type Surface string

const (
	SurfaceChat       Surface = "chat_completions"
	SurfaceMessages   Surface = "messages"
	SurfaceEmbeddings Surface = "embeddings"
	SurfaceImages     Surface = "images"
	// SurfaceResponses is a fourth surface in the openai protocol. It shares
	// that protocol's wire format and credentials and differs only in the shape
	// of the request, the response and the usage report. "The protocol is the
	// dialect, the surface is the endpoint" already allows several surfaces per
	// protocol.
	SurfaceResponses Surface = "responses"
	// Utility and resource operations are separate route capabilities. They
	// deliberately do not inherit the parent surface: an upstream that serves
	// Responses or Messages is not necessarily able to compact, count tokens,
	// or persist resources.
	SurfaceResponsesCompact     Surface = "responses_compact"
	SurfaceResponsesResources   Surface = "responses_resources"
	SurfaceResponsesInputTokens Surface = "responses_input_tokens"
	SurfaceMessagesCountTokens  Surface = "messages_count_tokens"
	// SurfaceGenerateContent is the Gemini protocol's own surface. It differs
	// from the other two protocols in where the request is addressed rather than
	// only in what it contains: the model is a path segment and streaming is a
	// different method name, not a body flag. Passing through it is the same
	// same-protocol pass-through as the others -- a Gemini request reaches a
	// Gemini upstream, with nothing translated on the way.
	SurfaceGenerateContent          Surface = "generate_content"
	SurfaceGeminiCountTokens        Surface = "gemini_count_tokens"
	SurfaceGeminiEmbedContent       Surface = "gemini_embed_content"
	SurfaceGeminiBatchEmbedContents Surface = "gemini_batch_embed_contents"
	SurfaceGeminiInteractions       Surface = "gemini_interactions"
)

// The surface alone determines both protocol and endpoint. endpoint is the key
// of model_route_probes rows; protocol matches the provider's declared dialects.
func (s Surface) protocol() (string, bool) {
	switch s {
	case SurfaceChat, SurfaceResponses, SurfaceResponsesCompact, SurfaceResponsesResources,
		SurfaceResponsesInputTokens, SurfaceEmbeddings, SurfaceImages:
		return ProtocolOpenAI, true
	case SurfaceGenerateContent, SurfaceGeminiCountTokens, SurfaceGeminiEmbedContent,
		SurfaceGeminiBatchEmbedContents, SurfaceGeminiInteractions:
		return ProtocolGemini, true
	case SurfaceMessages, SurfaceMessagesCountTokens:
		return ProtocolAnthropic, true
	default:
		return "", false
	}
}

func (s Surface) endpoint() (string, bool) {
	switch s {
	case SurfaceChat:
		return "chat", true
	case SurfaceMessages:
		return "messages", true
	case SurfaceEmbeddings:
		return "embeddings", true
	case SurfaceImages:
		return "images", true
	case SurfaceResponses:
		return "responses", true
	case SurfaceResponsesCompact:
		return "responses_compact", true
	case SurfaceResponsesResources:
		return "responses_resources", true
	case SurfaceResponsesInputTokens:
		return "responses_input_tokens", true
	case SurfaceGenerateContent:
		return "generate_content", true
	case SurfaceMessagesCountTokens:
		return "messages_count_tokens", true
	case SurfaceGeminiCountTokens:
		return "gemini_count_tokens", true
	case SurfaceGeminiEmbedContent:
		return "gemini_embed_content", true
	case SurfaceGeminiBatchEmbedContents:
		return "gemini_batch_embed_contents", true
	case SurfaceGeminiInteractions:
		return "gemini_interactions", true
	default:
		return "", false
	}
}

// Endpoint is the stored name of this surface's endpoint -- the key of its
// probe rows -- exported for the data plane, which has a surface in hand when
// it needs to name the endpoint a route failed on.
func (s Surface) Endpoint() (string, bool) { return s.endpoint() }

// ProbeMode says how a route's capability on a surface is established. It is
// an attribute of the surface table, and everything that treats one endpoint
// differently from another -- the seeder, the worker, the sweeper, the
// candidate query, the catalog -- reads it from here rather than spelling the
// endpoint's name.
type ProbeMode string

const (
	// ProbeAuto: probed automatically when a route is created and re-checked
	// by the sweeper. A route is a candidate unless a probe found the endpoint
	// unsupported, and the catalog publishes it once a probe found it working.
	ProbeAuto ProbeMode = "auto"
	// ProbeManual: never probed on its own, because one probe costs real money
	// (an image generation). A route is a candidate only once an operator's
	// probe or override says `ok` -- an endpoint the gateway refuses to observe
	// on its own is opt-in, not tried on live traffic.
	ProbeManual ProbeMode = "manual"
	// ProbeDerived: cannot be probed (nothing to retrieve until a request has
	// stored something) and only ever reached pinned to the route that created
	// the resource. No probe row exists; it is published with the surface that
	// creates the resource.
	ProbeDerived ProbeMode = "derived"
)

// probeMode is the one table. A surface absent here is ProbeAuto.
func (s Surface) probeMode() ProbeMode {
	switch s {
	case SurfaceImages:
		return ProbeManual
	case SurfaceResponsesResources:
		return ProbeDerived
	default:
		return ProbeAuto
	}
}

// ProbeModeForEndpoint is the probe mode of an endpoint by its stored name.
func ProbeModeForEndpoint(endpoint string) (ProbeMode, bool) {
	s, ok := SurfaceForEndpoint(endpoint)
	if !ok {
		return "", false
	}
	return s.probeMode(), true
}

// allSurfaces is the complete set, walked by the two reverse lookups below.
// Adding a surface means adding an entry here plus a case in each of the
// protocol() and endpoint() switches.
var allSurfaces = []Surface{
	SurfaceChat, SurfaceMessages, SurfaceMessagesCountTokens,
	SurfaceResponses, SurfaceResponsesCompact, SurfaceResponsesResources, SurfaceResponsesInputTokens,
	SurfaceEmbeddings, SurfaceImages, SurfaceGenerateContent, SurfaceGeminiCountTokens,
	SurfaceGeminiEmbedContent, SurfaceGeminiBatchEmbedContents, SurfaceGeminiInteractions,
}

// ProtocolForEndpoint returns the protocol that serves an endpoint. It is
// what configuration-time validation checks against.
//
// It derives from Surface.protocol() rather than being written out a second time:
// configuration time and run time must decide "who can serve whom" the same
// way, or the gap between them produces configurations that save fine and never
// run.
func ProtocolForEndpoint(endpoint string) (string, bool) {
	for _, s := range allSurfaces {
		if ep, ok := s.endpoint(); ok && ep == endpoint {
			return s.protocol()
		}
	}
	return "", false
}

// SurfaceForEndpoint returns the surface an endpoint name denotes.
//
// The same table again, read for the third purpose: the admin-side probe needs
// a surface so it can put its request through exactly the rewrite the data
// plane applies. Without that, a provider whose profile re-cuts the body is
// probed with a body the data plane would never send, and the probe reports its
// own mistake as the provider's.
func SurfaceForEndpoint(endpoint string) (Surface, bool) {
	for _, s := range allSurfaces {
		if ep, ok := s.endpoint(); ok && ep == endpoint {
			return s, true
		}
	}
	return "", false
}

// ProtocolEndpoints returns every endpoint in a dialect, with a second return
// value reporting whether that dialect exists at all -- which is what validates
// the protocols a provider declares.
//
// It is the same table as ProtocolForEndpoint, read the other way. This direction
// is needed because "which endpoints can this provider be probed on" is a union
// over its protocols, and that union is derived from protocol to endpoint.
func ProtocolEndpoints(protocol string) ([]string, bool) {
	var out []string
	for _, s := range allSurfaces {
		fam, ok := s.protocol()
		if !ok || fam != protocol {
			continue
		}
		if ep, ok := s.endpoint(); ok {
			out = append(out, ep)
		}
	}
	return out, len(out) > 0
}

// EndpointsForProtocols is the union of ProtocolEndpoints over a provider's
// protocol set, in the table's stable order: every endpoint a route on that
// provider may be asked to serve, and therefore every endpoint it is probed on.
// Unknown protocols contribute nothing.
func EndpointsForProtocols(protocols []string) []string {
	var out []string
	for _, s := range allSurfaces {
		fam, ok := s.protocol()
		if !ok || !slices.Contains(protocols, fam) {
			continue
		}
		if ep, ok := s.endpoint(); ok {
			out = append(out, ep)
		}
	}
	return out
}

// PublishedEndpoints turns the endpoints a probe has verified into the set the
// catalog publishes. The only derivation is the ProbeDerived surface: the
// stored-resource operations ride on the surface that creates the resource.
// Anything else is published only when it was observed.
func PublishedEndpoints(verified []string) []string {
	out := make([]string, 0, len(verified)+1)
	out = append(out, verified...)
	responses, _ := SurfaceResponses.endpoint()
	resources, _ := SurfaceResponsesResources.endpoint()
	if slices.Contains(out, responses) && !slices.Contains(out, resources) {
		out = append(out, resources)
		slices.Sort(out)
	}
	return out
}

// KnownProtocols is every dialect the gateway speaks, in a stable order.
//
// Derived from the same surface table as the two lookups above rather than
// written out again: a dialect that exists in one list and not the other is a
// protocol that can be declared and never served, or served and never
// declarable, and both fail quietly.
func KnownProtocols() []string {
	var out []string
	for _, s := range allSurfaces {
		fam, ok := s.protocol()
		if !ok || slices.Contains(out, fam) {
			continue
		}
		out = append(out, fam)
	}
	return out
}

// Service is the entry point for catalog reads.
type Service struct {
	q         *gwdb.Queries
	cache     cache.Store // nil reads straight from the database
	settings  *Settings
	transport *transportRuntime
}

type transportRuntime struct {
	mu        sync.RWMutex
	knownGood map[pgtype.UUID]Transport
}

func NewService(q *gwdb.Queries, c cache.Store, store *settings.Store) *Service {
	return &Service{
		q: q, cache: c, settings: NewSettings(store),
		transport: &transportRuntime{knownGood: make(map[pgtype.UUID]Transport)},
	}
}

// WithRequestSnapshot binds catalog reads to a caller-owned database snapshot.
// Mutable catalog/current-version caches are deliberately bypassed: a cached
// model price combined with plan or procurement rows from the transaction would
// reintroduce the cross-version mix this view exists to prevent.
func (s *Service) WithRequestSnapshot(q *gwdb.Queries) *Service {
	return &Service{q: q, settings: s.settings, transport: s.transport}
}

// Settings exposes the gateway settings reader, which the proxy layer consults
// for the kill switch and for pricing.
func (s *Service) Settings() *Settings { return s.settings }

// ErrModelUnavailable means the model does not exist, is disabled, or has
// nothing serving it on this surface. All three are one 404 to the client: a
// model outside your catalog must not leak "it exists but you may not use it".
var ErrModelUnavailable = fmt.Errorf("catalog: model is not available on this surface")

// Resolution is the outcome of resolving one model, consumed by the proxy.
type Resolution struct {
	Model        gwdb.Model
	ModelPricing ModelPricingSnapshot
	Routes       []Route
}

// ModelPricingSnapshot is the price as read at the start of a request.
//
// The price is pinned by the pipeline's snapshot transaction, not by a version
// id: reading it once inside the transaction is itself the lock, so a price
// change during a streaming request is not applied retroactively -- this
// request already read its copy.
type ModelPricingSnapshot struct {
	Priced        bool
	BillingMode   string
	Upstream      Price
	MultiplierBps int64
	UpdatedAt     pgtype.Timestamptz
}

// modelPricingCachePayload is cached per model_id. Its wire format is decoupled
// from the driver's own JSON encoding, so that upgrading a dependency cannot
// make already-cached prices unreadable.
type modelPricingCachePayload struct {
	BillingMode   string    `json:"billing_mode"`
	Upstream      Price     `json:"upstream"`
	MultiplierBps int64     `json:"multiplier_bps"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// Route is one usable candidate, with the provider details already joined in so
// the request builder can use it directly.
type Route struct {
	ID           pgtype.UUID
	ProviderID   pgtype.UUID
	ProviderSlug string
	// ProviderVendor is which platform this candidate belongs to. It decides
	// whether a organization-supplied credential applies to this hop: the organization has
	// an account at a platform, not at a protocol.
	ProviderVendor  string
	Protocol        string
	BaseURL         string
	ProviderModelID string
	Priority        int32
	Weight          int32
	// Procurement is the cost basis pinned when this candidate was resolved.
	// It affects cost and margin only and never enters the customer's price.
	Procurement ProcurementPricingSnapshot
	// ContextWindow and MaxOutputTokens override the model's limits for this
	// upstream; 0 means "use the model's own value". The same model can carry
	// different limits on different relays.
	ContextWindow   int32
	MaxOutputTokens int32
	// IgnoresMaxOutputTokens marks an upstream that does not honour the output
	// limit in the request -- one relay was measured returning 94 tokens for a
	// requested 16. The pre-authorization estimate therefore refuses to treat
	// the requested cap as a cost ceiling for it.
	IgnoresMaxOutputTokens bool
	// ProviderHeaders and RouteHeaders are the two levels of header mapping.
	// On merge, a route-level key overrides the provider-level one.
	ProviderHeaders map[string]string
	RouteHeaders    map[string]string
	// Transport is the provider's addressing profile. It has no route level:
	// how an upstream is reached is a property of the machine, and two routes
	// on one machine disagreeing about which header carries the credential
	// would not describe anything real.
	Transport Transport
	// Capacity is what this provider's upstream account will take. It belongs
	// to the provider rather than to the route for the same reason Transport
	// does: a quota is granted to an account, not to one of the models that
	// account serves.
	Capacity ProviderCapacity
}

// ProviderCapacity is the declared throughput of one upstream account.
//
// Zero means undeclared for the two rate ceilings, so nothing is measured
// against them. MaxConcurrency always has a value, because "how many calls at
// once" has no meaningful "unlimited": past some number the upstream stops
// answering, and a gateway with no opinion queues until something times out.
type ProviderCapacity struct {
	RateLimitRPM   int
	RateLimitTPM   int
	MaxConcurrency int
}

// ProcurementPricingSnapshot is the cost basis at request time. It is a single
// scalar: cost is the official rate times MultiplierBps. There are no absolute
// per-route, per-dimension or per-tool overrides, because a relay prices as
// "official rate times a discount" and modelling schemes nobody uses is false
// precision.
type ProcurementPricingSnapshot struct {
	MultiplierBps int64
}

// Resolve resolves a model and returns the candidates usable on this surface,
// ordered by ascending priority. No candidates means ErrModelUnavailable: a
// model configured only for chat lands here when an images request arrives,
// because a model's advertised capabilities are the union of its enabled
// routes.
//
// tierID is the caller's effective access tier. The zero value skips tier
// filtering, for paths with no caller context such as the public catalog.
func (s *Service) Resolve(
	ctx context.Context, slug string, surface Surface, tierID pgtype.UUID,
) (Resolution, error) {
	return s.ResolveFor(ctx, slug, surface, tierID, nil)
}

// ResolveFor is Resolve for a caller that brings its own credential for some
// vendors. An `unsupported` verdict was reached with the platform's shared
// credential, and an upstream's 404 also means "your project has no access";
// the organization's own key may have it, so for those vendors the verdict
// does not exclude the route.
func (s *Service) ResolveFor(
	ctx context.Context, slug string, surface Surface, tierID pgtype.UUID, byokVendors []string,
) (Resolution, error) {
	protocol, ok := surface.protocol()
	if !ok {
		return Resolution{}, ErrModelUnavailable
	}
	endpoint, _ := surface.endpoint()

	model, err := s.modelBySlug(ctx, slug)
	if err != nil {
		return Resolution{}, err
	}
	// The per-model kill switch is the enabled column, already filtered by the
	// query. There is no protocol on the model to compare against: a model is
	// reachable on every protocol its routes' providers speak, and the
	// candidate query filters by that membership. Nothing translates between
	// protocols -- a request that arrives on a protocol no provider of this
	// model speaks simply finds no candidate.
	// Visibility is the outermost admission check. A hot path that tests only
	// enabled makes every other visibility mean "not listed" rather than "not
	// callable" -- the two become identical in effect, and a model the
	// operator believes is hidden can still be called directly.
	if model.Visibility != visibilityPublic {
		return Resolution{}, ErrModelUnavailable
	}
	modelPricing, err := s.currentModelPricing(ctx, model)
	if err != nil {
		return Resolution{}, err
	}
	// The access tier, the middle layer: it either admits every model or admits
	// exactly what it lists. The check itself is in SQL.
	if tierID.Valid {
		allowed, aErr := s.q.TierAllowsModel(ctx, gwdb.TierAllowsModelParams{
			TierID: tierID, ModelID: model.ID,
		})
		if aErr != nil {
			if db.IsNoRows(aErr) {
				// The tier was deleted while a cached identity still carried
				// it. Every model is unreachable for this caller until the
				// identity is reloaded, which is what "unavailable" says; a
				// permissive default here would serve a request under an
				// admission policy that no longer exists.
				slog.WarnContext(ctx, "catalog: the caller's access tier no longer exists; refusing",
					"model", slug)
				return Resolution{}, ErrModelUnavailable
			}
			return Resolution{}, fmt.Errorf("catalog: query access tier: %w", aErr)
		}
		if !allowed {
			return Resolution{}, ErrModelUnavailable
		}
	}
	// An unpriced model refuses service. This comes before the candidate
	// query: an incomplete configuration must not consume upstream resources,
	// and the caller must not be told "there is no route" when the truth is
	// "nobody has set a price yet".
	if err := checkPriced(modelPricing); err != nil {
		return Resolution{}, err
	}

	rows, err := s.q.ListRoutesForModel(ctx, gwdb.ListRoutesForModelParams{
		ModelID: model.ID, Protocol: protocol, Endpoint: endpoint, ByokVendors: byokVendors,
		// An endpoint the gateway never probes on its own is opt-in: a
		// candidate only where a verdict says ok.
		RequiresVerdict: surface.probeMode() == ProbeManual,
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("catalog: query route candidates: %w", err)
	}
	if len(rows) == 0 {
		return Resolution{}, ErrModelUnavailable
	}

	routes := make([]Route, 0, len(rows))
	for _, r := range rows {
		transport, tErr := s.transportFor(r.ProviderID, r.ProviderSlug, r.ProviderTransport)
		if tErr != nil {
			return Resolution{}, tErr
		}
		route := Route{
			ID:                     r.ID,
			ProviderID:             r.ProviderID,
			ProviderSlug:           r.ProviderSlug,
			ProviderVendor:         r.ProviderVendor,
			Protocol:               r.Protocol,
			BaseURL:                r.BaseUrl,
			ProviderModelID:        r.ProviderModelID,
			Priority:               r.Priority,
			Weight:                 r.Weight,
			ProviderHeaders:        decodeHeaders(ctx, r.ProviderHeaders),
			RouteHeaders:           decodeHeaders(ctx, r.Headers),
			Transport:              transport,
			ContextWindow:          r.ContextWindow.Int32,
			MaxOutputTokens:        r.MaxOutputTokens.Int32,
			IgnoresMaxOutputTokens: quirkBool(ctx, r.Quirks, quirkIgnoresMaxOutput),
			Procurement:            ProcurementPricingSnapshot{MultiplierBps: noDiscountBps},
			Capacity: ProviderCapacity{
				RateLimitRPM:   int(r.ProviderRateLimitRpm.Int32),
				RateLimitTPM:   int(r.ProviderRateLimitTpm.Int32),
				MaxConcurrency: int(r.ProviderMaxConcurrency),
			},
		}
		// The cost basis is one scalar: cost is the official rate times the
		// provider's multiplier. It is read inside the snapshot transaction
		// and frozen into the usage record per request, so changing the
		// multiplier never moves a historical margin.
		mult, pErr := s.q.GetRouteCostMultiplier(ctx, r.ID)
		if pErr != nil {
			return Resolution{}, fmt.Errorf("catalog: read route cost multiplier: %w", pErr)
		}
		route.Procurement = ProcurementPricingSnapshot{MultiplierBps: int64(mult)}
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		return Resolution{}, ErrModelUnavailable
	}
	return Resolution{Model: model, ModelPricing: modelPricing, Routes: routes}, nil
}

// TransportHealth validates every stored provider profile. It is a readiness
// check, not a liveness check: an invalid initial catalog must not receive
// traffic, while a running instance can keep serving with the last profile it
// validated and expose the bad hot update here.
func (s *Service) TransportHealth(ctx context.Context) error {
	rows, err := s.q.ListProvidersForProbe(ctx)
	if err != nil {
		return fmt.Errorf("catalog: list provider transports: %w", err)
	}
	var faults []error
	for _, row := range rows {
		transport, validateErr := ValidateTransport(row.Transport)
		if validateErr != nil {
			faults = append(faults, fmt.Errorf("provider %q: %w", row.Slug, validateErr))
			continue
		}
		s.rememberTransport(row.ID, transport)
	}
	return errors.Join(faults...)
}

func (s *Service) transportFor(id pgtype.UUID, slug string, raw []byte) (Transport, error) {
	transport, err := ValidateTransport(raw)
	if err == nil {
		s.rememberTransport(id, transport)
		return transport, nil
	}
	if previous, ok := s.lastTransport(id); ok {
		slog.Error("provider transport hot load failed; retaining last-known-good profile",
			"provider", slug, "error", err)
		return previous, nil
	}
	return Transport{}, fmt.Errorf("catalog: provider %q has invalid transport: %w", slug, err)
}

func (s *Service) rememberTransport(id pgtype.UUID, transport Transport) {
	if s.transport == nil || !id.Valid {
		return
	}
	s.transport.mu.Lock()
	s.transport.knownGood[id] = transport
	s.transport.mu.Unlock()
}

func (s *Service) lastTransport(id pgtype.UUID) (Transport, bool) {
	if s.transport == nil || !id.Valid {
		return Transport{}, false
	}
	s.transport.mu.RLock()
	transport, ok := s.transport.knownGood[id]
	s.transport.mu.RUnlock()
	return transport, ok
}

// currentModelPricing reads this model's current price row.
//
// The cache is a single level. With no version ids, a "current pointer plus
// versioned payload" indirection would be complexity and nothing else, and the
// invalidation trigger is simply "this model's price was written".
func (s *Service) currentModelPricing(ctx context.Context, model gwdb.Model) (ModelPricingSnapshot, error) {
	if snapshot, ok := s.cachedModelPricing(ctx, model.ID); ok {
		return snapshot, nil
	}
	row, err := s.q.GetModelPricing(ctx, model.ID)
	if err != nil {
		if db.IsNoRows(err) {
			// A model without its single current price row is unpriced.
			return ModelPricingSnapshot{}, nil
		}
		return ModelPricingSnapshot{}, fmt.Errorf("catalog: query model price: %w", err)
	}
	snapshot, snapErr := modelPricingSnapshot(row)
	if snapErr != nil {
		return ModelPricingSnapshot{}, snapErr
	}
	return s.publishModelPricingCache(ctx, model.ID, snapshot)
}

func modelPricingSnapshot(v gwdb.ModelPricing) (ModelPricingSnapshot, error) {
	// Free stops charging the customer; it does not make the upstream cost
	// disappear. Every price row must therefore state all four rates
	// explicitly: NULL means unknown and fails closed, while an explicit 0 is
	// what says that component really is free.
	if !v.UpstreamInNanoPerMtok.Valid || !v.UpstreamOutNanoPerMtok.Valid ||
		!v.UpstreamCacheReadNanoPerMtok.Valid || !v.UpstreamCacheWriteNanoPerMtok.Valid {
		return ModelPricingSnapshot{}, ErrModelUnpriced
	}
	return ModelPricingSnapshot{
		Priced:      true,
		BillingMode: v.BillingMode,
		Upstream: Price{
			InNanoPerMTok:         v.UpstreamInNanoPerMtok.Int64,
			OutNanoPerMTok:        v.UpstreamOutNanoPerMtok.Int64,
			CacheReadNanoPerMTok:  v.UpstreamCacheReadNanoPerMtok.Int64,
			CacheWriteNanoPerMTok: v.UpstreamCacheWriteNanoPerMtok.Int64,
		},
		MultiplierBps: int64(v.MultiplierBps), UpdatedAt: v.UpdatedAt,
	}, nil
}

func modelPricingCacheKey(modelID pgtype.UUID) string {
	return "gw:pricing:model:" + uuid.UUID(modelID.Bytes).String()
}

func (s *Service) cachedModelPricing(ctx context.Context, modelID pgtype.UUID) (ModelPricingSnapshot, bool) {
	if s.cache == nil || !modelID.Valid {
		return ModelPricingSnapshot{}, false
	}
	raw, ok, err := s.cache.Get(ctx, modelPricingCacheKey(modelID))
	if err != nil || !ok {
		if err != nil {
			slog.WarnContext(ctx, "model price cache read failed, falling back to the database", "model_id", modelID, "error", err)
		}
		return ModelPricingSnapshot{}, false
	}
	var payload modelPricingCachePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		_ = s.cache.Delete(ctx, modelPricingCacheKey(modelID))
		return ModelPricingSnapshot{}, false
	}
	return payload.snapshot(), true
}

func cachePayload(snapshot ModelPricingSnapshot) modelPricingCachePayload {
	payload := modelPricingCachePayload{
		BillingMode: snapshot.BillingMode, Upstream: snapshot.Upstream,
		MultiplierBps: snapshot.MultiplierBps,
	}
	if snapshot.UpdatedAt.Valid {
		payload.UpdatedAt = snapshot.UpdatedAt.Time.UTC()
	}
	return payload
}

func (p modelPricingCachePayload) snapshot() ModelPricingSnapshot {
	snapshot := ModelPricingSnapshot{
		Priced: true, BillingMode: p.BillingMode, Upstream: p.Upstream,
		MultiplierBps: p.MultiplierBps,
	}
	if !p.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = pgtype.Timestamptz{Time: p.UpdatedAt.UTC(), Valid: true}
	}
	return snapshot
}

// publishModelPricingCache writes a single cache level.
//
// A two-level scheme -- a mutable current pointer plus payloads stored under
// immutable version ids, with a re-read after writing -- exists to stop a
// publication that lands between two reads from being assembled into a mixed
// price. With no versions there is one price row, and the invalidation trigger
// is simply "this model's price was written", so the write endpoint deletes
// this key. That is cheaper than the re-read and easier to follow.
func (s *Service) publishModelPricingCache(
	ctx context.Context, modelID pgtype.UUID, snapshot ModelPricingSnapshot,
) (ModelPricingSnapshot, error) {
	if s.cache == nil || !snapshot.Priced {
		return snapshot, nil
	}
	raw, err := json.Marshal(cachePayload(snapshot))
	if err != nil {
		return snapshot, nil
	}
	if err := s.cache.Set(ctx, modelPricingCacheKey(modelID), raw, catalogTTL); err != nil {
		slog.WarnContext(ctx, "model price cache write failed (this request is unaffected)", "model_id", modelID, "error", err)
	}
	return snapshot, nil
}

// modelBySlug reads a model, through the cache.
func (s *Service) modelBySlug(ctx context.Context, slug string) (gwdb.Model, error) {
	key := s.modelCacheKey(ctx, slug)
	if s.cache != nil {
		if raw, ok, err := s.cache.Get(ctx, key); err == nil && ok {
			var m gwdb.Model
			if json.Unmarshal(raw, &m) == nil {
				return m, nil
			}
		}
	}
	m, err := s.q.GetModelBySlug(ctx, slug)
	if err != nil {
		if db.IsNoRows(err) {
			return gwdb.Model{}, ErrModelUnavailable
		}
		return gwdb.Model{}, fmt.Errorf("catalog: query model: %w", err)
	}
	if s.cache != nil {
		if raw, mErr := json.Marshal(m); mErr == nil {
			if err := s.cache.Set(ctx, key, raw, catalogTTL); err != nil {
				slog.WarnContext(ctx, "catalog cache write failed (this request is unaffected)", "error", err)
			}
		}
	}
	return m, nil
}

// InvalidateAll clears the whole catalog cache. Editing a provider affects
// every model hanging off it, and invalidating those one by one means first
// querying which they are. Invalidating everything is simpler and safer: the
// catalog is small, and rebuilding it costs less than a routing error caused by
// a missed invalidation.
func (s *Service) InvalidateAll(ctx context.Context) {
	if s.cache == nil {
		return
	}
	// Every model key carries the generation, so writing a new generation
	// makes the old keys unreachable and they expire on their own. Deleting a
	// key that never took part in forming a model key -- an easy mistake here
	// -- invalidates nothing at all.
	next := strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := s.cache.Set(ctx, catalogGenerationKey, []byte(next), 0); err != nil {
		slog.ErrorContext(ctx, "catalog-wide invalidation failed, hot updates are delayed until the TTL expires", "error", err)
	}
}

// catalogGenerationKey holds the catalog cache's generation. Every model cache
// key must incorporate its current value.
const catalogGenerationKey = "gw:catalog:gen"

func (s *Service) catalogGeneration(ctx context.Context) string {
	if s.cache == nil {
		return "0"
	}
	if raw, ok, err := s.cache.Get(ctx, catalogGenerationKey); err == nil && ok && len(raw) > 0 {
		return string(raw)
	}
	return "0"
}

func (s *Service) modelCacheKey(ctx context.Context, slug string) string {
	return "gw:model:" + s.catalogGeneration(ctx) + ":" + slug
}

// InvalidateModel clears one model's catalog cache. It is called after an
// operator write, and the eviction is broadcast by the cache driver.
func (s *Service) InvalidateModel(ctx context.Context, slug string) {
	if s.cache == nil {
		return
	}
	if err := s.cache.Delete(ctx, s.modelCacheKey(ctx, slug)); err != nil {
		slog.ErrorContext(ctx,
			"catalog cache invalidation broadcast failed, hot updates are delayed "+
				"until the TTL expires",
			"slug", slug, "error", err)
	}
}

// InvalidateModelPricing moves only the mutable current pointer for one model and
// evicts that model's catalog row. Immutable version payloads are intentionally
// retained: in-flight requests may still hold/reference the old version, and its
// bytes can never become incorrect after publication.
func (s *Service) InvalidateModelPricing(ctx context.Context, modelID pgtype.UUID, slug string) error {
	if s.cache == nil {
		return nil
	}
	var errs []error
	if err := s.cache.Delete(ctx, modelPricingCacheKey(modelID)); err != nil {
		errs = append(errs, fmt.Errorf("delete model pricing current pointer: %w", err))
	}
	if slug != "" {
		if err := s.cache.Delete(ctx, s.modelCacheKey(ctx, slug)); err != nil {
			errs = append(errs, fmt.Errorf("delete model catalog row %s: %w", slug, err))
		}
	}
	return errors.Join(errs...)
}

// PublicModel is one row of GET /v1/models.
type PublicModel struct {
	Slug        string
	DisplayName string
	// Protocols is the set of protocols the verified endpoints belong to. It
	// is derived from Endpoints, never from the providers' declared protocol
	// sets: a declaration is a claim, and the catalog publishes observations.
	Protocols       []string
	ContextWindow   int32
	MaxOutputTokens int32
	// Endpoints is what this model has been seen to serve: every endpoint a
	// probe found working on an enabled route of an enabled provider, plus
	// the stored-resource operations when the surface that creates them is
	// verified. An endpoint nobody has verified is callable -- the data plane
	// lets anything through that is not known to be unsupported -- but is
	// not listed, because a listing the caller cannot act on is the failure
	// the catalog exists to prevent.
	Endpoints       []string
	Capabilities    json.RawMessage
	PriceIn         int64
	PriceOut        int64
	PriceCacheRead  int64
	PriceCacheWrite int64
	Currency        string
	IsFree          bool
	// ModelMultiplierBps is the effective multiplier from the upstream's
	// official rate to the published rate.
	ModelMultiplierBps int64
	// PlanMultiplierBps is the plan in force: this organization's plan for an
	// authenticated catalog, the default plan for the anonymous one. A
	// per-model override has already replaced the plan's default in the
	// query, so nothing multiplies again at render time.
	PlanMultiplierBps int64
	// PlanDefaultMultiplierBps is the resolved plan's own default, before any
	// per-model override replaced it. One query resolves one plan, so every row
	// carries the same value; it exists because the deployment-level service fee
	// has to be quoted at the same plan as the rates beside it, and a per-model
	// override is not the plan.
	PlanDefaultMultiplierBps int64
	// PricingPlanID is set only for an authenticated catalog: which pricing
	// plan this organization actually resolved to. The anonymous catalog
	// resolves the default plan to price with, but has no organization whose
	// plan this would be, so it clears the field rather than publishing an
	// internal id that answers nobody's question.
	PricingPlanID pgtype.UUID
	// PriceUpdatedAt is when this price row was last written. Since saving is
	// publishing, that is also when it took effect.
	PriceUpdatedAt pgtype.Timestamptz
	// SourceName and SourceURL record where the upstream rate came from, and
	// VerifiedAt when a person confirmed it against the vendor's own list. An
	// invalid VerifiedAt means nobody has: the rate came from the bundled
	// reference dataset and carries only that dataset's authority. See
	// docs/design/reference-prices.md.
	SourceName string
	SourceURL  string
	VerifiedAt pgtype.Timestamptz
}

// PublicModels returns the public catalog: public visibility, enabled, and at
// least one usable route. It applies no tier filter, because the public catalog
// is a price list seen from the default tier, not any particular caller's
// available set.
func (s *Service) PublicModels(ctx context.Context) ([]PublicModel, error) {
	models, err := s.modelsFor(ctx, pgtype.UUID{}, pgtype.UUID{})
	if err != nil {
		return nil, err
	}
	// The default plan priced these rows -- that is what makes this list the
	// price a reader signing up right now would be charged -- but there is no
	// organization here whose plan it is. Publishing the id would put an
	// internal identifier in an anonymous response that answers nobody's
	// question.
	for i := range models {
		models[i].PricingPlanID = pgtype.UUID{}
	}
	return models, nil
}

// ModelsForOrg returns this organization's final per-unit prices. The plan multiplier
// is resolved per model, because a per-model override replaces the plan default
// rather than stacking on it -- one number shared by the whole catalog would be
// wrong.
func (s *Service) ModelsForOrg(
	ctx context.Context, tierID, orgID pgtype.UUID,
) ([]PublicModel, error) {
	return s.modelsFor(ctx, tierID, orgID)
}

func (s *Service) modelsFor(ctx context.Context, tierID, orgID pgtype.UUID) ([]PublicModel, error) {
	rows, err := s.q.ListPublicModels(ctx, gwdb.ListPublicModelsParams{TierID: tierID, OrgID: orgID})
	if err != nil {
		return nil, fmt.Errorf("catalog: query public catalog: %w", err)
	}
	out := make([]PublicModel, 0, len(rows))
	for _, r := range rows {
		// The contract requires a name, while display_name may be empty.
		// Falling back to the slug rather than emitting an empty string or
		// omitting the field: a catalog entry with no name is useless to a
		// client, and the slug is at least displayable and copyable.
		name := r.DisplayName
		if name == "" {
			name = r.Slug
		}
		m := PublicModel{
			Slug:            r.Slug,
			DisplayName:     name,
			Protocols:       r.Protocols,
			ContextWindow:   r.ContextWindow,
			MaxOutputTokens: r.MaxOutputTokens,
			Endpoints:       PublishedEndpoints(r.Endpoints),
			Capabilities:    r.Capabilities,
			Currency:        r.PriceCurrency,
			// The rates and the free flag are deliberately left unset: no
			// price row means no price, not a price of zero. Zeroing them
			// would make "unpriced" and "deliberately free" identical
			// downstream. A model without a price row is skipped below, so it
			// never leaves here carrying a set of zeros. The multiplier
			// defaults to 1x and is overridden below for priced models.
			ModelMultiplierBps:       noDiscountBps,
			PlanMultiplierBps:        noDiscountBps,
			PlanDefaultMultiplierBps: noDiscountBps,
		}
		// The price has exactly one home: the model_pricing row.
		if r.PricedModelID.Valid {
			m.PriceIn = r.CurrentUpstreamInNanoPerMtok.Int64
			m.PriceOut = r.CurrentUpstreamOutNanoPerMtok.Int64
			m.PriceCacheRead = r.CurrentUpstreamCacheReadNanoPerMtok.Int64
			m.PriceCacheWrite = r.CurrentUpstreamCacheWriteNanoPerMtok.Int64
			m.IsFree = r.CurrentBillingMode.String == "free"
			m.ModelMultiplierBps = int64(r.CurrentModelMultiplierBps.Int32)
			m.PriceUpdatedAt = r.CurrentModelPriceEffectiveAt
			m.SourceName = r.CurrentPriceSourceName.String
			m.SourceURL = r.CurrentPriceSourceUrl.String
			m.VerifiedAt = r.CurrentPriceVerifiedAt
			if r.CurrentPricingPlanID.Valid {
				m.PricingPlanID = r.CurrentPricingPlanID
				m.PlanDefaultMultiplierBps = int64(r.CurrentPlanDefaultMultiplierBps.Int32)
				m.PlanMultiplierBps = int64(r.CurrentPlanDefaultMultiplierBps.Int32)
				if r.CurrentPlanOverrideMultiplierBps.Valid {
					m.PlanMultiplierBps = int64(r.CurrentPlanOverrideMultiplierBps.Int32)
				}
			}
		}
		// No price row means unpriced, and an unpriced model is not listed --
		// the same rule resolution applies. A model whose calls would be
		// refused as unpriced must not appear in the catalog with four zero
		// rates, looking free and available. A deliberately free model does
		// have a row and stays listed.
		if !r.PricedModelID.Valid {
			continue
		}
		// Everything that reaches here is priced, so failing to resolve a plan
		// is a configuration fault: there is always exactly one active default
		// plan, and failing to resolve one means it was disabled or deleted.
		// Falling back to 1x would quietly publish -- and charge -- list price
		// while the operator believes a discount is in force. This applies to
		// the anonymous catalog too: it is priced by the default plan, so it
		// has the same way of being wrong.
		if !m.PricingPlanID.Valid {
			return nil, fmt.Errorf(
				"catalog: model %s resolves to no active pricing plan (was the default plan disabled or deleted?)",
				m.Slug)
		}
		out = append(out, m)
	}
	return out, nil
}

// ErrModelUnpriced means the price row is missing one of the four upstream
// rates, or a paid model has no non-zero rate at all. A free model still has to
// carry the full upstream rates, because that is how cost is computed.
//
// Keeping it separate from ErrModelUnavailable matters: that one means "nothing
// serves this", which the operator cannot act on directly, while this one means
// "the configuration is incomplete", which they fix by entering a price.
// Merging them makes the most fixable problem the hardest to locate.
var ErrModelUnpriced = errors.New("catalog: model has no pricing configured")

// checkPriced decides whether a model can be billed.
//
// The rule is "all four buckets are zero", not "the input rate is zero":
// embeddings have no output, an image model's cost sits on the output side, and
// a cache-oriented entry may carry only cache rates. Judging on any single
// bucket would reject those perfectly normal configurations. Only all four at
// zero really means nothing was set.
func checkPriced(p ModelPricingSnapshot) error {
	if !p.Priced {
		return ErrModelUnpriced // there is no price row at all
	}
	if p.BillingMode == "free" {
		return nil // explicitly declared free
	}
	if p.Upstream.InNanoPerMTok == 0 && p.Upstream.OutNanoPerMTok == 0 &&
		p.Upstream.CacheReadNanoPerMTok == 0 && p.Upstream.CacheWriteNanoPerMTok == 0 {
		return ErrModelUnpriced
	}
	return nil
}

// PriceOf returns a model's four upstream rates.
//
// It takes the price row, not the model row. Reading rates off the model row
// only works while something patches the real values into that in-memory struct
// during resolution, and that patch is exactly where a defect can hide: swap
// which columns hold the price, leave the model type untouched, and the type
// system sees nothing. Reading the price directly removes an intermediate state
// that is able to lie.
func PriceOf(p ModelPricingSnapshot) Price {
	return Price{
		InNanoPerMTok:         p.Upstream.InNanoPerMTok,
		OutNanoPerMTok:        p.Upstream.OutNanoPerMTok,
		CacheReadNanoPerMTok:  p.Upstream.CacheReadNanoPerMTok,
		CacheWriteNanoPerMTok: p.Upstream.CacheWriteNanoPerMTok,
	}
}

// IsFree is the single rule for "this model is free": the billing mode on the
// price row.
//
// It is a method rather than a string comparison spelled out at each call site,
// because there are five of them -- non-streaming, streaming, images, snapshot
// and effective rate -- and five copies of a string comparison are five chances
// to misspell it.
func (p ModelPricingSnapshot) IsFree() bool { return p.BillingMode == "free" }

// BillablePriceOf returns the base rate on the customer-billing side. Free is
// an explicit billing mode rather than a catalog label: the upstream rates stay
// on the row, and everything charged to the customer is nonetheless zero. The
// cost side keeps using PriceOf, so the real cost of a free model remains
// visible.
func BillablePriceOf(p ModelPricingSnapshot) Price {
	if p.IsFree() {
		return Price{}
	}
	return PriceOf(p)
}

// decodeHeaders decodes the header map. Its shape is guaranteed by a database
// CHECK; a decode failure is logged and treated as an empty map, because
// sending a request with a header missing is better than failing the whole
// chain.
func decodeHeaders(ctx context.Context, raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		slog.ErrorContext(ctx, "header mapping is not valid json, treating it as empty", "error", err)
		return nil
	}
	return m
}

// quirkIgnoresMaxOutput is the only upstream behaviour flag recognized so
// far.
const quirkIgnoresMaxOutput = "ignores_max_output_tokens"

// quirkBool reads one boolean flag. Malformed jsonb is treated as unset rather
// than failing the route: this is supplementary configuration, and one bad row
// must not render a model unavailable.
func quirkBool(ctx context.Context, raw []byte, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		slog.WarnContext(ctx, "route quirks is not valid jsonb, treating it as unset", "error", err)
		return false
	}
	v, _ := m[key].(bool)
	return v
}

// HoldCap gives the output limit the pre-authorization estimate should use, and
// whether the request's own cap can be trusted as an upper bound.
//
// The pre-authorization happens before a candidate is chosen, so which upstream
// will serve the request is not yet known and the most conservative answer wins:
// the limit is the maximum across candidates, and if any one candidate ignores
// the requested cap, none of them are trusted with it. The point of holding
// funds is to reserve what might be spent, and holding too much is corrected at
// settlement while holding too little defeats the budget guard entirely -- a
// request asking for 16 output tokens could burn thousands.
func HoldCap(m gwdb.Model, routes []Route) (cap int64, ignoreRequestCap bool) {
	cap = int64(m.MaxOutputTokens)
	for _, r := range routes {
		if int64(r.MaxOutputTokens) > cap {
			cap = int64(r.MaxOutputTokens)
		}
		if r.IgnoresMaxOutputTokens {
			ignoreRequestCap = true
		}
	}
	return cap, ignoreRequestCap
}
