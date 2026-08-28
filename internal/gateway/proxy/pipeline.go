package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/alert"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/settle"
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
)

// The request pipeline.
//
// Billing has two hard boundaries:
//   - a failure *before* the first byte reaches the client voids the hold and
//     charges nothing;
//   - a failure after it settles against whatever usage was produced.
//
// On the non-streaming path the "first byte" is the entire response, so every
// failure here voids.

// nonStreamTimeout bounds a whole non-streaming request.
const nonStreamTimeout = 120 * time.Second

// withConnectTimeout puts this provider's own connect bound on the request, for
// the shared dialer to read. The mechanism lives in httpx because the admin API
// and the liveness probe dial the same upstreams and want the same bound; this
// is only the adapter that knows where a provider keeps it.
func withConnectTimeout(ctx context.Context, t Target) context.Context {
	return httpx.WithConnectTimeout(ctx, t.Transport.ConnectTimeout())
}

// maxUpstreamBody caps how much of an upstream response body is read. The bound
// comes from base64 image responses; text responses are far smaller.
const maxUpstreamBody = 64 << 20 // 64 MiB

// Pipeline orchestrates one dataplane request.
type Pipeline struct {
	pool    *pgxpool.Pool
	gw      *gwdb.Queries
	catalog *catalog.Service
	auth    *Authenticator
	guard   *Guard
	billing settle.Settler
	// artifacts is where finished video output lives. Never nil: the assembly
	// point binds NoCustody when the deployment has no object store, so the
	// call sites do not each have to remember to check.
	artifacts Artifacts
	box       *crypto.Box
	client    *http.Client
	// streamHTTP is the streaming-only client (see newStreamClient). It is
	// built once and held because its Transport carries a connection pool.
	streamHTTP *http.Client

	// Resilience: candidate strategy, two-level circuit breaking, a retry
	// budget and per-provider backpressure. See
	// docs/design/failover-and-cooldowns.md.
	strategy               *PriorityWeighted
	breaker                *Breaker
	budget                 *RetryBudget
	semaphores             sync.Map // providerID -> *Semaphore
	keyCursors             sync.Map // providerID -> *atomic.Uint64, the credential round-robin
	perProviderConcurrency int
	// limiter measures each upstream account's declared capacity. It is the
	// same driver the Guard uses for customer limits: one shared counter per
	// deployment, so with several replicas the allowance stays the configured
	// one rather than being multiplied by the replica count.
	limiter ratelimit.Limiter

	// alerter reports configuration faults such as an unpriced model. Not
	// injected means log only.
	alerter Alerter

	// notifyBYOK tells a organization that their own upstream credential was
	// rejected. It is a function rather than an interface because there is
	// exactly one call shape and an interface would only add a layer. Not
	// injected means log only -- but then the organization first learns they were
	// switched back to full price when the bill arrives, so the assembly point
	// really should supply it.
	notifyBYOK func(ctx context.Context, orgID, keyID pgtype.UUID, upstreamStatus int)
	// requestProbe asks the probe worker to look at one endpoint of one route
	// after an upstream said there was nothing there. See
	// PipelineConfig.ProbeRequester.
	requestProbe func(ctx context.Context, routeID pgtype.UUID, endpoint string)
	// unpricedSeen suppresses repeat alerts: a model missing prices keeps
	// being requested, and alerting on every one of those drowns the channel
	// and gets the alerts ignored.
	unpricedSeen sync.Map // modelSlug -> time.Time

	// The request state machine is split by responsibility. Pipeline remains
	// the short public orchestrator; collaborators own admission/pricing,
	// execution/failover and durable settlement recording respectively.
	admission          *Admission
	pricingSnapshot    *PricingSnapshot
	executor           *Executor
	settlementRecorder *SettlementRecorder
}

// Alerter is where operational alerts go. One declaration, in foundation,
// because `gateway` imports this package — a port declared there could not
// reach the two packages that raise the most alerts (ADR-0190).
type Alerter = alert.Sink

// PipelineConfig is the complete, immutable construction input for a data
// plane pipeline. A usable Pipeline is created in one operation.
type PipelineConfig struct {
	Pool          *pgxpool.Pool
	Gateway       *gwdb.Queries
	Catalog       *catalog.Service
	Authenticator *Authenticator
	Guard         *Guard
	Settlement    settle.Settler
	// Artifacts may be nil, which binds NoCustody: a deployment without an
	// object store still serves video, it just proxies the bytes on read.
	Artifacts           Artifacts
	Cipher              *crypto.Box
	HTTPClient          *http.Client
	BreakerStore        breaker.Store
	RateLimit           ratelimit.Limiter
	ProviderConcurrency int
	Alerter             Alerter
	BYOKNotifier        func(context.Context, pgtype.UUID, pgtype.UUID, int)
	// ProbeRequester is told when an upstream answered a live request with
	// "nothing here for this model" (404/405) on a route that was not pinned
	// and not using the organization's own credential. It asks the probe
	// worker to look; the data plane itself never writes a verdict, because
	// one live 404 cannot tell "unsupported" from "being rolled" or "wrong
	// upstream name". Nil means nobody is listening.
	ProbeRequester func(ctx context.Context, routeID pgtype.UUID, endpoint string)
}

// unpricedAlertInterval is how long alerts for one model are suppressed.
const unpricedAlertInterval = time.Hour

// alertUnpriced reports an unpriced model, at most once an hour per model.
func (p *Pipeline) alertUnpriced(ctx context.Context, slug string) {
	if p.alerter == nil {
		return
	}
	now := time.Now()
	if last, ok := p.unpricedSeen.Load(slug); ok {
		if t, _ := last.(time.Time); now.Sub(t) < unpricedAlertInterval {
			return
		}
	}
	p.unpricedSeen.Store(slug, now)
	p.alerter.Alert(ctx, "Model has no pricing configured; requests are being refused",
		"All four billing buckets of model "+slug+" are priced at zero and it is not marked as a free model. "+
			"Until prices are filled in or the model is explicitly marked free, every request for it answers 503.")
}

// NewPipeline constructs a complete immutable request pipeline.
func NewPipeline(cfg PipelineConfig) *Pipeline {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: nonStreamTimeout, Transport: httpx.UpstreamTransport()}
	}
	brk := cfg.BreakerStore
	if brk == nil {
		brk = noopBreakerStore{}
	}
	concurrency := cfg.ProviderConcurrency
	if concurrency <= 0 {
		concurrency = defaultProviderConcurrency
	}
	p := &Pipeline{
		pool: cfg.Pool, gw: cfg.Gateway, catalog: cfg.Catalog, auth: cfg.Authenticator,
		guard: cfg.Guard, billing: cfg.Settlement, box: cfg.Cipher, client: client,
		artifacts:              orNoCustody(cfg.Artifacts),
		streamHTTP:             newStreamClient(client.Transport),
		strategy:               NewPriorityWeighted(),
		breaker:                NewBreaker(brk, cfg.Gateway),
		budget:                 NewRetryBudget(),
		perProviderConcurrency: concurrency,
		limiter:                cfg.RateLimit,
		alerter:                cfg.Alerter,
		notifyBYOK:             cfg.BYOKNotifier,
		requestProbe:           cfg.ProbeRequester,
	}
	p.pricingSnapshot = &PricingSnapshot{pipeline: p}
	p.admission = &Admission{pipeline: p, pricing: p.pricingSnapshot}
	p.executor = &Executor{pipeline: p}
	p.settlementRecorder = &SettlementRecorder{pipeline: p}
	return p
}

// Budget exposes the retry budget so the health dashboard can read its usage.
func (p *Pipeline) Budget() *RetryBudget { return p.budget }

// Breaker exposes the circuit breaker so cooldown state can be restored at
// start-up.
func (p *Pipeline) Breaker() *Breaker { return p.breaker }

// defaultProviderConcurrency is the cap used for a candidate that carries no
// configured one. The column is NOT NULL with this same default, so in a real
// deployment it applies only to routes built by hand in tests.
const defaultProviderConcurrency = 64

// semaphoreFor returns this provider's concurrency semaphore, creating it
// lazily and replacing it when the configured cap has changed.
//
// Replacement rather than resizing, because a buffered channel cannot be
// resized. The slots held by requests in flight against the old semaphore are
// released into it and then dropped with it, so a change is briefly permissive
// -- up to the old cap plus the new one -- and never restrictive. That is the
// right direction for a backpressure valve: a configuration change must not
// refuse traffic that was already accepted.
func (p *Pipeline) semaphoreFor(providerID pgtype.UUID, limit int) *Semaphore {
	if limit <= 0 {
		limit = p.perProviderConcurrency
	}
	key := uuidStr(providerID)
	if v, ok := p.semaphores.Load(key); ok {
		if sem, _ := v.(*Semaphore); sem.Capacity() == limit {
			return sem
		}
	}
	sem := NewSemaphore(limit)
	p.semaphores.Store(key, sem)
	return sem
}

// providerCapacityBucket names the rate-limiter buckets that hold an upstream
// account's declared allowance. They are keyed by provider, not by route: the
// quota belongs to the account, and every model served on it draws from the
// same one.
func providerCapacityBucket(kind string, providerID pgtype.UUID) string {
	return "gw:cap:" + kind + ":" + uuidStr(providerID)
}

// capacityAllows asks whether this provider still has allowance this minute,
// and consumes it when it has.
//
// It is a filter, exactly like a circuit breaker: a provider with nothing left
// is skipped and the next candidate is tried. It deliberately does not become a
// weight -- an upstream is either usable right now or it is not, and a share of
// traffic that quietly erodes is far harder to reason about than a candidate
// that is plainly out.
//
// A broken limiter driver lets the request through, the same direction every
// other capacity gate fails in: refusing traffic because the limiter is
// unreachable turns a limiter outage into an outage.
func (p *Pipeline) capacityAllows(ctx context.Context, route catalog.Route, estimatedTokens int64) (bool, time.Duration) {
	if p.limiter == nil {
		return true, 0
	}
	if rpm := route.Capacity.RateLimitRPM; rpm > 0 {
		res, err := p.limiter.Allow(ctx, providerCapacityBucket("rpm", route.ProviderID), rpm, rateWindow)
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: provider request-capacity check failed; letting the attempt through",
				"provider", route.ProviderSlug, "error", err)
		} else if !res.Allowed {
			return false, res.RetryAfter
		}
	}
	if tpm := route.Capacity.RateLimitTPM; tpm > 0 && estimatedTokens > 0 {
		n := int(min(estimatedTokens, int64(tpm)+1)) // an over-limit request can never pass, and this cannot overflow
		res, err := p.limiter.AllowN(ctx, providerCapacityBucket("tpm", route.ProviderID), n, tpm, rateWindow)
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: provider token-capacity check failed; letting the attempt through",
				"provider", route.ProviderSlug, "error", err)
		} else if !res.Allowed {
			return false, res.RetryAfter
		}
	}
	return true, 0
}

// noopBreakerStore is the fallback when no store is injected: everything is
// available and nothing is recorded. It lets a Pipeline run in tests and in
// degraded assemblies without a breaker store being mandatory.
type noopBreakerStore struct{}

func (noopBreakerStore) Get(context.Context, string) (breaker.State, bool, error) {
	return breaker.State{}, false, nil
}
func (noopBreakerStore) Set(context.Context, string, breaker.State, time.Duration) error { return nil }
func (noopBreakerStore) Delete(context.Context, string) error                            { return nil }

// Request is the input to one dataplane call.
type Request struct {
	Surface      catalog.Surface
	Protocol     Protocol
	UpstreamPath string // upstream path, e.g. /v1/chat/completions
	Method       string // empty means POST
	Resource     string // upstream stateful resource id for path substitution
	Body         []byte
	// Credential is the plaintext virtual key, extracted by the handler via
	// CredentialOf. Deliberately not the raw header: header names belong to
	// the HTTP boundary, which the pipeline does not know about.
	Credential string
	EndUserID  string // X-End-User-Id header, wins over the body's attribution field
	// RequestID is generated by the handler, which also writes it into the
	// response header, so that X-Request-Id and the usage log's request id are
	// genuinely the same value. Left empty, this layer generates one -- tests
	// that build a Request directly need not care.
	RequestID string
	// Model overrides the body's model field, for the protocol that puts it in
	// the address instead. Empty means "read it from the body", which is every
	// other surface.
	Model string
	// UpstreamQuery is appended to the outbound URL. It carries the one
	// parameter this gateway adds on its own -- the Gemini stream selector,
	// which is how that API asks for SSE rather than a JSON array.
	UpstreamQuery map[string]string
	// Stream records whether the caller asked for a streamed response. It is
	// read off the body once, at the boundary that also decides which entry
	// point to call, so the dispatch and the recorded fact cannot disagree.
	//
	// It lives here rather than being passed down as a parameter because
	// everything below needs it for a different reason -- the usage row's
	// stream column, the failure path's metric label -- and a parameter
	// threaded through those layers is one each new caller has to get right,
	// with a silent mislabel rather than a compile error when they do not.
	// RunStream and the buffered entry points normalise it on the way in, so a
	// caller that builds a Request by hand (every test does) still gets a value
	// that matches the path it actually took.
	Stream bool
	// A stateful follow-up is pinned to the exact credential that created the
	// resource. These are populated only after an org-scoped affinity lookup.
	PinnedProviderKeyID    pgtype.UUID
	PinnedOrgProviderKeyID pgtype.UUID
	// Utility operations are authenticated, admitted, rate-limited and audited
	// but skip monetary pricing, budget and hold/settlement.
	Utility bool
}

// Result is what the pipeline produces; the handler renders the response from
// it.
type Result struct {
	Status int
	Body   []byte
}

// prepared is the first half, shared by the streaming and non-streaming paths:
// authentication, gates, parsing, pricing.
type prepared struct {
	id        Identity
	modelSlug string
	affinity  *gwdb.GetResourceAffinityRow
	// byok is every usable credential the organization brings, resolved once
	// per request before candidates are chosen: which vendors it covers is an
	// input to candidate selection, not only to the credential pick.
	byok        byokChoices
	res         catalog.Resolution
	pricing     pricingInputs
	inputTokens int64
	estNano     int64
	// priceTable hangs off the model price version locked at the start of the
	// request. It holds the four official prices, the advanced dimensions and
	// the tool prices. The customer side uses it as is; the cost side only
	// swaps in the route's four base buckets.
	priceTable catalog.PriceTable
	// unitPriceTable is the per-unit rate card, locked in the same transaction,
	// and populated when the *price row* says this model is billed by unit --
	// not when the surface says so (ADR-0227). The images surface carries both
	// families at once, so keying this off the surface loaded the card for
	// every image request or for none of them, and neither is right.
	//
	// Stored unswitched, exactly as priceTable is. The customer-side view is
	// derived at the point of billing via ForBilling; keeping only that view
	// here would hand the same zeroed table to the cost side and make every
	// free job look like it cost nothing to serve.
	unitPriceTable catalog.UnitPriceTable
	// units is the billable quantity vector of a unit-priced *synchronous*
	// request, computed during admission and settled unchanged.
	//
	// The job plane leaves it empty and computes its own after validating
	// against the capability envelope, because a job outlives the request that
	// started it. A synchronous one has no such gap: the charge is a pure
	// function of parameters already in hand, so the hold below is the exact
	// amount rather than an estimate, and settlement reuses this vector rather
	// than deriving a second one that could disagree with what was reserved.
	units catalog.Units
}

// unitBilled reports whether this request is charged from the per-unit card.
// The answer is on the price row, never on the surface.
func (p prepared) unitBilled() bool {
	return p.res.ModelPricing.Family == catalog.FamilyUnits
}

// billingUnitPrices returns the pair of tables the unit-priced charge is
// computed from: what the customer is charged against, and what it cost us.
//
// They are the same rate card viewed two ways, and the split is the whole
// point -- `free` stops the charge, never the cost, or margin reporting
// silently loses every free request.
func (p prepared) billingUnitPrices() (list, cost catalog.UnitPriceTable) {
	return p.unitPriceTable.ForBilling(p.res.ModelPricing.IsFree()), p.unitPriceTable
}

// prepare runs the first half of the pipeline: authentication, kill switch,
// model resolution, admission gates, pricing and estimation. Both paths
// (streaming and not) are identical here and diverge only at forwarding.
func (a *Admission) Prepare(ctx context.Context, in Request) (prepared, *Error) {
	p := a.pipeline
	var out prepared

	// 1. Authentication and scope.
	id, gerr := p.auth.Authenticate(ctx, in.Credential)
	if gerr != nil {
		return out, gerr
	}
	// Store the identity the moment authentication succeeds: the caller needs
	// it to record every later kind of failure in the usage log. When this
	// assignment sat after RequireScope, a scope failure could not even carry
	// the org, so that failure left no trace in the database at all.
	out.id = id
	if gerr := RequireScope(id, "inference"); gerr != nil {
		return out, gerr
	}

	// 2. The global kill switch, the broadest of the three availability gates.
	if p.catalog.Settings().KillSwitch(ctx) {
		return out, NewError(errcode.GatewayModelDisabled, "Service is temporarily disabled")
	}

	// 3. Parse the request body and resolve the model.
	modelSlug, affinity, gerr := p.modelForAdmission(ctx, id, in)
	if gerr != nil {
		return out, gerr
	}
	out.modelSlug = modelSlug
	out.affinity = affinity
	// An entirely unusable admission tier -- disabled, or no default tier at
	// all -- fails closed. This runs before CheckModel because when the tier
	// is unusable every model is refused, and the organization deserves that one
	// clear reason instead of a string of "model not found" to guess from.
	if gerr := p.guard.CheckTier(id); gerr != nil {
		return out, gerr
	}
	if gerr := p.guard.CheckModel(id, modelSlug); gerr != nil {
		return out, gerr
	}

	// BYOK credentials are not part of the versioned catalog snapshot and read
	// through the pool directly, so resolve them before holding a transaction
	// connection. Everything after BeginTx must read through pricingQ (including
	// settings cache misses) or a saturated pool can deadlock when every request
	// holds one connection and waits for another.
	out.byok = p.resolveBYOK(ctx, id.OrgID)

	// The model price, the plan/organization binding and the procurement version of
	// every candidate route must come from one MVCC instant. Under READ
	// COMMITTED a sequence of individually correct SELECTs can still compose
	// "old model price plus new plan or new procurement price" while a release
	// is being activated. This read-only transaction is held only until every
	// pricing object has been copied into memory; the hold, route selection,
	// retries and streaming settlement never look up the current version again.
	pricingTx, err := p.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: opening the request pricing snapshot failed", "error", err)
		return out, NewError(errcode.GatewayInternal, "Model resolution failed")
	}
	defer func() { _ = pricingTx.Rollback(ctx) }()
	pricingQ := p.gw.WithTx(pricingTx)
	requestCatalog := p.catalog.WithRequestSnapshot(pricingQ, pricingTx)

	res, err := requestCatalog.ResolveFor(ctx, modelSlug, in.Surface, id.ModelTierID, out.byok.vendors())
	if err != nil {
		if errors.Is(err, catalog.ErrModelUnpriced) {
			// Incomplete configuration is not the caller's fault, so this is a
			// 503 rather than a 404 -- and it has to alert: a missing price is
			// an operator problem, and an operator who does not go looking at
			// the logs will never find out.
			slog.ErrorContext(ctx, "dataplane: model has no pricing configured; refusing to serve", "model", modelSlug)
			p.alertUnpriced(ctx, modelSlug)
			return out, NewError(errcode.GatewayModelUnpriced, "Model is temporarily unavailable")
		}
		if errors.Is(err, catalog.ErrModelUnavailable) {
			if affinityErr := p.affinityResolutionError(ctx, id, in, out.affinity, pricingQ); affinityErr != nil {
				return out, affinityErr
			}
			return out, NewError(errcode.GatewayModelNotFound, "Model not found or unavailable")
		}
		slog.ErrorContext(ctx, "dataplane: model resolution failed", "error", err)
		return out, NewError(errcode.GatewayInternal, "Model resolution failed")
	}
	out.res = res
	if in.Utility {
		if err := pricingTx.Commit(ctx); err != nil {
			return out, NewError(errcode.GatewayInternal, "Model resolution failed")
		}
		out.inputTokens = EstimateRequestTokens(in.Body)
		if gerr := p.guard.CheckRate(ctx, id, out.inputTokens); gerr != nil {
			return out, gerr
		}
		return out, nil
	}

	// 4. Lock the whole pricing chain.
	pricing, gerr := a.pricing.LockInputs(ctx, id, res, pricingQ, requestCatalog.Settings())
	if gerr != nil {
		return out, gerr
	}
	out.pricing = pricing
	priceTable, tErr := requestCatalog.LockedPriceTable(ctx, res.Model.ID, res.ModelPricing)
	if tErr != nil {
		// A failed database read is not the same as "no override row": the
		// latter inherits the four buckets normally, the former cannot prove
		// the price list is complete and must fail closed before any upstream
		// call.
		slog.ErrorContext(ctx, "dataplane: locking the advanced price table failed", "error", tErr, "model", modelSlug)
		return out, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
	out.priceTable = priceTable
	if out.unitBilled() {
		unitTable, uErr := requestCatalog.LockedUnitPriceTable(ctx, res.Model.ID)
		if uErr != nil {
			slog.ErrorContext(ctx, "dataplane: locking the unit price table failed", "error", uErr, "model", modelSlug)
			return out, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
		}
		if unitTable.Empty() {
			// A model on the unit plane with no unit rate is unpriced, and it
			// reaches here only because model_pricing said `units` while the
			// rate rows are missing -- an invariant no CHECK can hold, so this
			// is where it is caught. Same treatment as an unpriced token model:
			// a 503 the operator gets told about, never a request served free.
			slog.ErrorContext(ctx, "dataplane: unit-priced model has no unit rates; refusing to serve", "model", modelSlug)
			p.alertUnpriced(ctx, modelSlug)
			return out, NewError(errcode.GatewayModelUnpriced, "Model is temporarily unavailable")
		}
		out.unitPriceTable = unitTable
	}
	if err := pricingTx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "dataplane: committing the request pricing snapshot failed", "error", err, "model", modelSlug)
		return out, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}

	// 5. Budget and rate limiting. Rate limiting has side effects, so it goes
	// after the other gates. These reads may hit a remote rate limiter, which
	// is why they run after the pricing transaction commits -- there is no
	// reason to keep a PostgreSQL MVCC snapshot alive across a network call.
	if gerr := p.guard.CheckBudget(ctx, id); gerr != nil {
		return out, gerr
	}
	// A unit-priced request has no tokens to estimate, and inventing a token
	// equivalence for its TPM cost would let one organization's video traffic
	// evict another's text traffic in a ratio nobody chose. It is bounded by
	// RPM instead, and that limitation is documented rather than papered over.
	if out.unitBilled() {
		if gerr := p.guard.CheckRate(ctx, id, 0); gerr != nil {
			return out, gerr
		}
		// The job plane stops here with estNano at zero: a job outlives the
		// request that started it, so the exact charge is computed by the
		// caller once the parameters have been validated against the model's
		// capability envelope, and the hold is taken there (ADR-0220).
		if in.Surface == catalog.SurfaceVideo {
			return out, nil
		}
		// A per-image model cannot be streamed, and this is the only place that
		// can say so before money moves.
		//
		// The charge on this family is the number of images produced, and a
		// stream has no place that number can be read from. In a buffered
		// response `data` is one array and every vendor on this surface fills
		// it the same way. In a stream they do not agree at all: this vendor
		// emits one `image_generation.partial_succeeded` per finished image and
		// a terminal `completed`, that one emits `completed` per image and up
		// to three `partial_image` renders before each -- and both spell the
		// payload `b64_json`, so the frames cannot be told apart by shape
		// either. Counting would overcharge on one vendor and undercharge on
		// the other.
		//
		// So it is refused, in the caller's dialect, before a hold is taken.
		// Not serving it unmetered and not guessing: the same answer the Gemini
		// array-streaming form gets, and for the same reason -- this gateway
		// does not serve a shape it cannot meter. Dropping `stream` returns the
		// identical images.
		if in.Stream && isImageSurface(in.Surface) {
			return out, NewError(errcode.GatewayInvalidRequest,
				"This model is billed per image, and the number of images produced "+
					"cannot be counted from a stream. Retry without \"stream\".")
		}
		// A synchronous unit-priced request has no such gap. The rate row is a
		// pure function of parameters already in hand and the hold is taken
		// against the most images this model can return in one response, rather
		// than an estimate -- which is why it does not go through
		// catalog.Estimate below. Settlement then replaces the count with what
		// the response actually carried.
		units, gerr := p.syncUnits(ctx, out, in)
		if gerr != nil {
			return out, gerr
		}
		list, cost := out.billingUnitPrices()
		quote, gerr := p.quoteOrRefuse(ctx, out, func() (catalog.Quote, error) {
			return catalog.ComputeUnits(list, cost, units, out.pricing.rates)
		})
		if gerr != nil {
			return out, gerr
		}
		out.units = units
		out.estNano = quote.ChargedNano
		return out, nil
	}
	out.inputTokens = EstimateRequestTokens(in.Body)
	if gerr := p.guard.CheckRate(ctx, id, out.inputTokens); gerr != nil {
		return out, gerr
	}

	// The hold is taken *before* route selection, so its cap is the most
	// conservative of the candidates (see catalog.HoldCap).
	holdCap, ignoreCap := catalog.HoldCap(res.Model, res.Routes)
	holdPrice, holdCeiling, holdRates := pricing.holdInputs(res.ModelPricing, priceTable)
	estNano, err := catalog.Estimate(catalog.EstimateInput{
		Price:            holdPrice,
		LongContext:      holdCeiling,
		InputTokens:      out.inputTokens,
		MaxOutput:        MaxTokensOf(in.Body),
		DefaultMaxCap:    holdCap,
		IgnoreRequestCap: ignoreCap,
		Rates:            holdRates,
	})
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: estimating the hold failed", "error", err)
		return out, NewError(errcode.GatewayInternal, "Billing estimation failed")
	}
	out.estNano = estNano

	return out, nil
}

// orNoCustody binds the no-custody store when the deployment has none, so no
// call site has to remember that the field can be empty.
func orNoCustody(a Artifacts) Artifacts {
	if a == nil {
		return NoCustody{}
	}
	return a
}

// recordRejection records a request refused before it ever reached an upstream.
// Both paths share it.
//
// Such failures used to be entirely silent: nothing in the usage log, nothing
// in the application log, so "the customer reports an error and the server
// knows nothing about it" was the normal outcome. Worse, the same logical error
// -- an unavailable model, say -- was recorded when it surfaced during route
// resolution but not when the admission tier caught it first, and recording
// half the cases is harder to work with than recording all or none.
//
// The split: failures that have an org go to the usage log, where they can be
// found per organization and read the same way as the ones logFailure writes.
// Authentication failures have no org (the column is NOT NULL) and only reach
// the metrics.
func (r *SettlementRecorder) RecordRejection(
	ctx context.Context, prep prepared, in Request, requestID string, gerr *Error, started time.Time,
) {
	p := r.pipeline
	recordRejected(ctx, string(in.Surface), gerr.Code)
	if !prep.id.OrgID.Valid {
		// No org means no usage row, so logFailure is never reached and the
		// outcome has to be recorded here instead. Without this, authentication
		// failures would be missing from the request counter entirely -- and
		// they are the single most frequent refusal there is.
		recordOutcome(ctx, string(in.Surface), failureStatus(gerr.Code), in.Stream, time.Since(started))
		return // authentication failed: we do not even know who this was
	}
	// The route stays zero and no upstream was attempted: this request was
	// refused by a gate before any candidate was chosen, and provider_id is
	// nullable. Zero attempts is the honest number here, and it is what
	// distinguishes "refused by us" from "every upstream failed".
	p.logFailure(ctx, prep.id, in, prep.modelSlug, catalog.Route{}, rotationResult{}, requestID, gerr, started)
}

// RecordHoldRejection records a request refused when the billing hold was
// placed: insufficient credit, a suspended organisation, or the hold write
// itself failing.
//
// These are terminal outcomes like any other, and until this existed all three
// entry points simply returned -- so the request counter, whose whole purpose
// is an operational error rate, was silently missing an entire class. The one
// that matters most is the last: mapHoldError turns a failed billing write into
// a `gateway.internal` 500, and that is precisely the moment an error-rate
// graph has to move.
//
// It deliberately writes no usage row, unlike the admission refusals that go
// through logFailure. Whether a refused-for-credit request belongs in the usage
// log is a real question with a real cost -- it is per-organization, per-request,
// partitioned data -- and it is not one a metrics fix should answer by
// accident.
func (r *SettlementRecorder) RecordHoldRejection(
	ctx context.Context, in Request, gerr *Error, started time.Time,
) {
	recordRejected(ctx, string(in.Surface), gerr.Code)
	recordOutcome(ctx, string(in.Surface), failureStatus(gerr.Code), in.Stream, time.Since(started))
}

// billingHoldInput builds the hold parameters. Both paths share it.
func billingHoldInput(id Identity, requestID string, amountNano int64) settle.HoldInput {
	return settle.HoldInput{OrgID: id.OrgID, RequestID: requestID, AmountNano: amountNano}
}

// Run executes the non-streaming pipeline. The handler renders the returned
// *Error per surface.
func (p *Pipeline) Run(ctx context.Context, in Request) (Result, *Error) {
	in.Stream = false // the buffered path, whatever the caller passed
	started := time.Now()
	requestID := in.requestID()

	prep, gerr := p.admission.Prepare(ctx, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return Result{}, gerr
	}
	prep, in, gerr = p.pinAffinity(ctx, prep, in)
	if gerr != nil {
		p.settlementRecorder.RecordRejection(ctx, prep, in, requestID, gerr, started)
		return Result{}, gerr
	}
	holdID, gerr := p.settlementRecorder.ReserveFor(ctx, prep.id, requestID, prep.estNano, holdTTLFor(in.Surface))
	if gerr != nil {
		p.settlementRecorder.RecordHoldRejection(ctx, in, gerr, started)
		return Result{}, gerr
	}

	upstream, rotation := p.executor.Execute(ctx, prep.res.Routes, in, prep.res.Model, prep.byok, prep.id.OrgID, prep.inputTokens)
	if rotation.err != nil {
		p.settlementRecorder.RecordExecutionFailure(ctx, prep, in, requestID, rotation, started)
		return Result{}, rotation.err
	}
	p.rememberAffinity(ctx, prep, in, upstream, rotation)
	return p.settlementRecorder.CompleteBuffered(ctx, bufferedCompletion{
		prep: prep, in: in, requestID: requestID, upstream: upstream,
		rotation: rotation, holdID: holdID, started: started,
	})
}

// pricingInputs holds this request's pricing parameters.
type pricingInputs struct {
	rates             catalog.Rates
	byokFeeBps        int64
	pricingPlanID     pgtype.UUID
	planModelOverride bool
	fxVersion         string
}

func (p pricingInputs) ratesForRoute(route catalog.Route) catalog.Rates {
	rates := p.rates
	rates.ProcurementMultiplierBps = route.Procurement.MultiplierBps
	return rates
}

// holdInputs gives the price and multipliers the hold is computed from. There
// is exactly one price list, so the hold and the settlement read the same one
// -- which is the relationship they should always have had.
//
// The ceiling comes from the customer-side view of the table, so a free model
// yields a zero ceiling and holds what it always held.
func (p pricingInputs) holdInputs(
	pricing catalog.ModelPricingSnapshot, table catalog.PriceTable,
) (catalog.Price, catalog.Price, catalog.Rates) {
	ceiling := table.ForBilling(pricing.IsFree()).LongContextCeiling()
	return catalog.BillablePriceOf(pricing), ceiling, p.rates
}

// LockInputs resolves the pricing chain and the exchange rate.
//
// Markup and discount are two orthogonal factors and are deliberately not
// squeezed into one priority chain: markup answers "how much margin does this
// model carry" (model override beats global), discount answers "what rate does
// this customer get" (from the org's model settings, loaded with the Identity).
func (s *PricingSnapshot) LockInputs(
	ctx context.Context, id Identity, res catalog.Resolution, pricingQ *gwdb.Queries,
	set *catalog.Settings,
) (pricingInputs, *Error) {
	fx := set.FXRate(ctx, id.WalletCurrency)
	if fx == "" {
		// With no configured rate, refuse to bill rather than assume 1 -- that
		// would issue a bill in the wallet currency using the dollar amount.
		slog.ErrorContext(ctx, "dataplane: no exchange rate configured; refusing to serve", "currency", id.WalletCurrency)
		return pricingInputs{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
	out := pricingInputs{
		byokFeeBps: set.BYOKFeeBps(ctx),
		rates: catalog.Rates{
			ModelMultiplierBps:       res.ModelPricing.MultiplierBps,
			PlanMultiplierBps:        10000,
			ProcurementMultiplierBps: 10000,
			FXRate:                   fx,
		},
		fxVersion: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(fx))),
	}

	row, err := pricingQ.ResolveUsagePricing(ctx, gwdb.ResolveUsagePricingParams{
		ModelSlug: res.Model.Slug, OrgID: id.OrgID,
	})
	if db.IsNoRows(err) {
		// A priced model with no resolvable plan usually means the default plan
		// was disabled by mistake. This *must* fail closed: falling back to a
		// multiplier of 1 silently charges list price while the operator
		// believes a discount is configured.
		slog.ErrorContext(ctx, "dataplane: priced model has no usable customer plan", "model", res.Model.Slug, "org", id.OrgID)
		return pricingInputs{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: locking the customer pricing plan failed", "error", err)
		return pricingInputs{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
	// The model identity is still checked. There is no version to check
	// against: both reads happen inside the same pricing snapshot transaction.
	// Accepting the wrong plan merely because the slug matched, when the query
	// itself is broken, is a different failure and worth guarding.
	if row.ModelID != res.Model.ID || row.ModelSlug != res.Model.Slug ||
		!row.PricingPlanID.Valid {
		slog.WarnContext(ctx, "dataplane: request pricing snapshot references are inconsistent", "model", res.Model.Slug)
		return pricingInputs{}, NewError(errcode.GatewayInternal, "Billing configuration is incomplete")
	}
	out.rates.ModelMultiplierBps = int64(row.ModelMultiplierBps)
	out.rates.PlanMultiplierBps = int64(row.PlanMultiplierBps)
	out.pricingPlanID = row.PricingPlanID
	out.planModelOverride = row.PlanModelOverride
	return out, nil
}

type upstreamResult struct {
	status   int
	body     []byte
	keyID    pgtype.UUID
	attempts int
	// byok records whether the hop that finally *succeeded* used a
	// organization-supplied credential, and byokKeyID which one. The choice made at
	// the start of the request cannot answer either: with fallback allowed, an
	// earlier hop can fail on the organization's credential and a later one succeed on
	// a shared one, and that request is billed at full price. With credentials
	// keyed by vendor there is not even a single "the organization's credential" for a
	// request whose candidates span platforms.
	byok      bool
	byokKeyID pgtype.UUID
}

// voidHold releases the hold, explicitly: this request produced nothing.
func (r *SettlementRecorder) VoidHold(ctx context.Context, orgID pgtype.UUID, requestID string) {
	p := r.pipeline
	if err := p.billing.Void(ctx, orgID, requestID); err != nil {
		slog.ErrorContext(ctx, "dataplane: voiding the hold failed (the sweeper will pick it up)", "error", err, "request_id", requestID)
	}
}

// mapHoldError translates a hold failure into a dataplane error, swapping the
// generic code for its gateway equivalent so the surface can render it. The
// management API's problem+json must never appear on the dataplane.
func mapHoldError(ctx context.Context, err error) *Error {
	var coded *httpx.CodeError
	if errors.As(err, &coded) {
		switch coded.Code {
		case errcode.CommonInsufficientCredits:
			return NewError(errcode.GatewayInsufficientCredits, "Insufficient credits")
		case errcode.CommonOrgSuspended:
			return NewError(errcode.GatewayOrgSuspended, "Organization is suspended")
		case errcode.CommonNotFound:
			// Organization missing or pending deletion: to the dataplane that
			// is the same as suspended.
			return NewError(errcode.GatewayOrgSuspended, "Organization is unavailable")
		}
	}
	slog.ErrorContext(ctx, "dataplane: placing the hold failed", "error", err)
	return NewError(errcode.GatewayInternal, "Billing failed")
}

// requestID returns the identifier the handler passed in, generating one when
// it is absent.
func (r Request) requestID() string {
	if r.RequestID != "" {
		return r.RequestID
	}
	return httpx.NewRequestID()
}

// settleArgs gathers every snapshot settlement and logging need.
type settleArgs struct {
	id        Identity
	requestID string
	quote     catalog.Quote
	usage     Usage
	// units is the billable quantity vector of a unit-priced synchronous
	// request, empty for a token-billed one. It is what makes "how many images
	// did this organization generate" a column that can be summed rather than a
	// document that has to be parsed -- the same reason the job plane records
	// it, arriving on this path for the first time.
	units     catalog.Units
	estimated bool
	model     gwdb.Model
	// modelPricing is the price row locked for this request. It is the *only*
	// thing that can answer whether the model is free.
	modelPricing catalog.ModelPricingSnapshot
	route        catalog.Route
	in           Request
	pricing      pricingInputs
	priceTable   catalog.PriceTable
	pricingIssue string
	// pricingFallback means the upstream did not report enough usage and the
	// hold amount was used instead.
	pricingFallback bool
	httpStatus      int
	durationMs      int32
	// Streaming only: the stream flag, the closing status (ok, canceled or
	// upstream_error) and the time to first byte.
	status string
	ttfbMs int32
	// attempts is how many routes were actually tried -- the observable
	// evidence of failover.
	attempts int32
	// byok and orgProviderKeyID record whether a organization-supplied credential was
	// used and which one. Reconciliation needs them to tell a service fee apart
	// from a full-price charge.
	byok             bool
	orgProviderKeyID pgtype.UUID
	// routeID and trail carry which route served the request and which hops
	// failed before it. The trail holds only the failures: the winning hop is
	// the row's own provider/route/credential columns.
	routeID pgtype.UUID
	trail   []byte
	// providerKeyID is *our* credential, the one from the provider's pool that
	// served this request. It is empty when a organization credential was used.
	// Without it a provider with several credentials cannot be told apart when
	// one of them starts being rejected: the rows all name the provider and
	// none of them name the key.
	providerKeyID pgtype.UUID
	// holdID is the reservation this request was settled against. It is what
	// lets this row and the accounting entries that funded it be looked up from
	// each other; zero where the deployment has no reservation concept.
	holdID  pgtype.UUID
	utility bool
}

// unsettledRecordTimeout bounds writing the fallback record. It is short
// because it runs on the response teardown path, where the client may already
// be gone -- it must not pin a goroutine.
const unsettledRecordTimeout = 5 * time.Second

// recordUnsettled records a failed settlement for a worker to replay.
//
// Two deliberate choices:
//   - use the *pool*, not the transaction that just failed -- that transaction
//     is no longer usable;
//   - detach from the request context (WithoutCancel) -- on the streaming path
//     the client is usually already gone, and the original context would cancel
//     this write too, which amounts to doing nothing.
//
// If this write also fails, only the log is left. But that means the database
// is broadly unavailable, at which point the dataplane itself should be
// breaking the circuit: an availability problem, not a billing one.
func (r *SettlementRecorder) RecordUnsettled(
	ctx context.Context, requestID string, id Identity,
	quote catalog.Quote, params gwdb.InsertUsageLogParams, cause error,
) {
	p := r.pipeline
	slog.ErrorContext(ctx, "dataplane: settlement failed; recording it for replay", "error", cause, "request_id", requestID)

	payload, mErr := gwusage.EncodeUsageReplayPayload(params)
	if mErr != nil {
		slog.ErrorContext(ctx, "dataplane: encoding the replay payload failed", "error", mErr, "request_id", requestID)
		return
	}
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unsettledRecordTimeout)
	defer cancel()

	if err := p.gw.RecordUnsettled(recCtx, gwdb.RecordUnsettledParams{
		RequestID: requestID, OrgID: id.OrgID,
		ChargedNano: quote.ChargedNano, Currency: id.WalletCurrency,
		Reason: cause.Error(), Payload: payload,
	}); err != nil {
		slog.ErrorContext(recCtx, "dataplane: writing the replay record failed -- this usage will go unsettled",
			"error", err, "request_id", requestID, "charged_nano", quote.ChargedNano)
	}
}

// recordPricingMissing preserves a request that was served but whose locked
// price version lacked a unit price for a dimension the upstream reported. It
// deliberately calls neither Settle nor Void: the hold stays held and the
// record goes into a physically separate queue, where no generic replay worker
// can mistakenly charge it at the estimated amount. These need an operator to
// add the missing price and then resolve them by hand against the snapshot.
func (r *SettlementRecorder) RecordPricingMissing(
	ctx context.Context, requestID string, id Identity, reservedNano int64,
	params gwdb.InsertUsageLogParams, cause error,
) {
	p := r.pipeline
	payload, err := gwusage.EncodeUsageReplayPayload(params)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: encoding the missing-price payload failed", "error", err, "request_id", requestID)
		return
	}
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unsettledRecordTimeout)
	defer cancel()

	// Stop the sweeper from expiring this hold first, then queue the record --
	// the order cannot be reversed. Queue first and crash in between and you
	// are left with a "waiting for a manual price fix" record whose hold has
	// already been swept away. If protecting fails, or the hold is no longer
	// there, nothing is queued: that money has either been released already or
	// been dealt with already, and one more to-do would only send an operator
	// chasing an entry that does not exist.
	protected, err := p.billing.ProtectHold(recCtx, id.OrgID, requestID)
	if err != nil {
		slog.ErrorContext(recCtx, "dataplane: protecting the hold failed; not queueing for manual pricing",
			"error", err, "request_id", requestID, "reserved_nano", reservedNano)
		return
	}
	if !protected {
		slog.WarnContext(recCtx, "dataplane: hold is no longer held; skipping the manual-pricing queue",
			"request_id", requestID, "reserved_nano", reservedNano)
		return
	}
	if err := p.gw.RecordUnsettledPricing(recCtx, gwdb.RecordUnsettledPricingParams{
		RequestID: requestID, OrgID: id.OrgID, ReservedNano: reservedNano,
		Currency: id.WalletCurrency, Reason: cause.Error(), Payload: payload,
	}); err != nil {
		slog.ErrorContext(recCtx, "dataplane: recording the missing-price entry failed -- the hold is kept but is not queued",
			"error", err, "request_id", requestID, "reserved_nano", reservedNano)
	}
}

// settleAndLog settles and writes the usage row in one transaction.
func (r *SettlementRecorder) SettleAndLog(ctx context.Context, a settleArgs) error {
	p := r.pipeline
	return db.WithSystemTx(ctx, p.pool, func(tx pgx.Tx) error {
		if err := p.billing.SettleTx(ctx, tx, settle.SettleInput{
			OrgID: a.id.OrgID, RequestID: a.requestID,
			ActualNano: a.quote.ChargedNano, APIKeyID: a.id.KeyID,
		}); err != nil {
			return err
		}
		return p.gw.WithTx(tx).InsertUsageLog(ctx, usageLogParams(a))
	})
}

// usagePricingSchemaVersion is the one snapshot shape the unsettled-replay
// decoder accepts. Every writer of a price snapshot has to stamp it, or that
// row can never be replayed.
const usagePricingSchemaVersion = 1

// usageLogParams turns the snapshot of one successful call into insert
// parameters.
func usageLogParams(a settleArgs) gwdb.InsertUsageLogParams {
	return gwdb.InsertUsageLogParams{
		OrgID:               a.id.OrgID,
		ApiKeyID:            a.id.KeyID,
		RequestID:           a.requestID,
		Surface:             string(a.in.Surface),
		ModelSlug:           a.model.Slug,
		ProviderID:          a.route.ProviderID,
		ProviderKeyID:       a.providerKeyID,
		HoldID:              a.holdID,
		RouteID:             a.routeID,
		Attempts:            attemptsJSON(a.trail),
		Byok:                a.byok,
		OrgProviderKeyID:    a.orgProviderKeyID,
		RouteAttempts:       attemptsOrOne(a.attempts),
		Stream:              a.in.Stream,
		Status:              statusOrOK(a.status),
		HttpStatus:          int32(a.httpStatus),
		TokensIn:            clampInt32(a.usage.In),
		TokensOut:           clampInt32(a.usage.Out),
		TokensCachedRead:    clampInt32(a.usage.CachedRead),
		TokensCacheWrite:    clampInt32(a.usage.CacheWrite),
		TokensReasoning:     clampInt32(a.usage.Reasoning),
		UsageEstimated:      a.estimated,
		UpstreamCostUsdNano: a.quote.UpstreamUSDNano,
		ChargedNano:         a.quote.ChargedNano,
		ChargedCurrency:     a.id.WalletCurrency,
		FxRate:              numericFromString(a.quote.FXRate),
		EndUserID:           endUserOf(a.in),
		TtftMs:              a.ttfbMs,
		DurationMs:          a.durationMs,
		// Tool usage and service tier are snapshotted alongside: both feed into
		// the charged amount, and without them this row cannot be recomputed on
		// its own.
		ToolCalls:          encodeToolCalls(a.usage.ToolCalls),
		ServiceTier:        pgtype.Text{String: a.usage.ServiceTier, Valid: true},
		TokensAudioIn:      snapshotInt4(a.usage.AudioIn),
		TokensAudioOut:     snapshotInt4(a.usage.AudioOut),
		TokensImageIn:      snapshotInt4(a.usage.ImageIn),
		TokensImageOut:     snapshotInt4(a.usage.ImageOut),
		TokensCacheWrite5m: snapshotInt4(a.usage.CacheWrite5m),
		TokensCacheWrite1h: snapshotInt4(a.usage.CacheWrite1h),
		BilledUnits:        billedUnitsOfVector(a.units),
		BilledUnit:         billedUnitOfVector(a.units),
		PricingSnapshot:    encodePricingSnapshot(a),
	}
}

// billedUnitsOfVector and billedUnitOfVector render a synchronous request's
// quantity vector into the two columns.
//
// Absent rather than zero for a token-billed request: NULL in this column means
// "not billed by unit", and a zero would claim a unit-billed request that
// produced nothing. The job plane says the same thing about its own vector, in
// billedUnitsOf.
func billedUnitsOfVector(u catalog.Units) pgtype.Int4 {
	if len(u.Quantities) == 0 {
		return pgtype.Int4{}
	}
	var total int64
	for _, q := range u.Quantities {
		total += q
	}
	return pgtype.Int4{Int32: clampInt32(total), Valid: true}
}

func billedUnitOfVector(u catalog.Units) string {
	for k := range u.Quantities {
		// One request names one unit: the vector is built from a single rate
		// key, and a mixed one would have no single answer to put in this
		// column.
		return string(k.Unit)
	}
	return ""
}

type usagePricingSnapshot struct {
	// schema_version identifies the only snapshot shape accepted by this
	// fresh-install baseline.
	SchemaVersion         int                        `json:"schema_version"`
	BillingMode           string                     `json:"billing_mode"`
	BYOK                  bool                       `json:"byok"`
	BYOKFeeBps            int64                      `json:"byok_fee_bps,omitempty"`
	PricingPlanID         string                     `json:"pricing_plan_id,omitempty"`
	PlanModelOverride     bool                       `json:"plan_model_override"`
	OfficialPriceTable    catalog.PriceTableSnapshot `json:"official_price_table"`
	ProcurementPriceTable catalog.PriceTableSnapshot `json:"procurement_price_table"`
	// ContextTokens is the prompt length the context-length price bands were
	// selected against. Recording the selector rather than "the band that was
	// hit" is deliberate: different buckets can resolve to different bands, and
	// one scalar cannot say that. Together with the min_input_tokens carried on
	// every row of the tables above, this replays the resolution exactly.
	ContextTokens            int64                  `json:"context_tokens"`
	OfficialRatesUSDPerM     usageTokenRatesUSDPerM `json:"official_rates_usd_per_m"`
	PublicRatesUSDPerM       usageTokenRatesUSDPerM `json:"public_rates_usd_per_m"`
	CustomerRatesUSDPerM     usageTokenRatesUSDPerM `json:"customer_rates_usd_per_m"`
	ProcurementRatesUSDPerM  usageTokenRatesUSDPerM `json:"procurement_rates_usd_per_m"`
	ModelMultiplierBps       int64                  `json:"model_multiplier_bps"`
	PlanMultiplierBps        int64                  `json:"plan_multiplier_bps"`
	ProcurementMultiplierBps int64                  `json:"procurement_multiplier_bps"`
	FXRate                   string                 `json:"fx_rate"`
	FXVersion                string                 `json:"fx_version"`
	UpstreamCostUSDNano      int64                  `json:"upstream_cost_usd_nano"`
	ChargedNano              int64                  `json:"charged_nano"`
	ChargedCurrency          string                 `json:"charged_currency"`
	SettlementStatus         string                 `json:"settlement_status"`
	Utility                  bool                   `json:"utility,omitempty"`
	PricingIssue             string                 `json:"pricing_issue,omitempty"`
}

func encodePricingSnapshot(a settleArgs) []byte {
	procurement := a.priceTable
	officialSnapshot := a.priceTable.Snapshot()
	procurementSnapshot := procurement.Snapshot()
	billingMode := map[bool]string{true: "free", false: "paid"}[a.modelPricing.IsFree()]
	effectiveRates := versionedEffectiveRates(a)
	snapshot := usagePricingSnapshot{
		SchemaVersion:            usagePricingSchemaVersion,
		BillingMode:              billingMode,
		BYOK:                     a.byok,
		BYOKFeeBps:               map[bool]int64{true: a.pricing.byokFeeBps}[a.byok],
		PricingPlanID:            uuidStr(a.pricing.pricingPlanID),
		PlanModelOverride:        a.pricing.planModelOverride,
		OfficialPriceTable:       officialSnapshot,
		ProcurementPriceTable:    procurementSnapshot,
		ContextTokens:            a.usage.BillingTokens().ContextTokens(),
		OfficialRatesUSDPerM:     effectiveRates.Official,
		PublicRatesUSDPerM:       effectiveRates.Public,
		CustomerRatesUSDPerM:     effectiveRates.Customer,
		ProcurementRatesUSDPerM:  effectiveRates.Procurement,
		ModelMultiplierBps:       a.quote.ModelMultiplierBps,
		PlanMultiplierBps:        a.quote.PlanMultiplierBps,
		ProcurementMultiplierBps: a.quote.ProcurementMultiplierBps,
		FXRate:                   a.quote.FXRate, FXVersion: a.pricing.fxVersion,
		UpstreamCostUSDNano: a.quote.UpstreamUSDNano,
		ChargedNano:         a.quote.ChargedNano, ChargedCurrency: a.id.WalletCurrency,
		SettlementStatus: map[bool]string{true: "pricing_missing", false: "priced"}[a.pricingIssue != ""],
		PricingIssue:     a.pricingIssue,
		Utility:          a.utility,
	}
	if a.utility {
		snapshot.BillingMode = "utility"
		snapshot.SettlementStatus = "not_charged"
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	return b
}

func snapshotInt4(v int64) pgtype.Int4 {
	return pgtype.Int4{Int32: clampInt32(v), Valid: true}
}

// encodeToolCalls serialises tool usage into jsonb. An empty map returns nil
// and SQL's coalesce stores `{}` -- one fallback shared by all three insertion
// points.
func encodeToolCalls(m map[string]int64) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func attemptsJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte(`[]`)
	}
	return raw
}

// attemptsOrOne defaults to 1: if nothing was recorded, at least one attempt
// happened.
func attemptsOrOne(n int32) int32 {
	if n < 1 {
		return 1
	}
	return n
}

// statusOrOK defaults to ok, which the non-streaming path never sets.
func statusOrOK(s string) string {
	if s == "" {
		return "ok"
	}
	return s
}

// logFailure writes a failure row with zero billing columns, because nothing is
// charged for a failure before the first byte.
//
// attempt carries how many candidates were tried and which credential the last
// one used. Both used to be dropped -- the row said one attempt and named no
// provider however many upstreams had actually been burned through, which made
// the row least informative in exactly the case it was written for.
func (p *Pipeline) logFailure(
	ctx context.Context, id Identity, in Request, modelSlug string,
	route catalog.Route, rot rotationResult, requestID string, gerr *Error, started time.Time,
) {
	// Measured once and shared by the metric and the row. Two calls to
	// time.Since would report the same request as two slightly different
	// durations in two places, which is the kind of gap nobody notices until
	// somebody reconciles the two sources and cannot.
	elapsed := time.Since(started)
	// Recorded before the write, not after: the metric describes what happened
	// to the request, and that is equally true when persisting the row fails.
	// Recording it after the insert would make the error rate silently depend
	// on database availability -- the one moment it most needs to be readable.
	recordOutcome(ctx, string(in.Surface), failureStatus(gerr.Code), in.Stream, elapsed)
	err := p.gw.InsertUsageLog(ctx, gwdb.InsertUsageLogParams{
		OrgID:         id.OrgID,
		ApiKeyID:      id.KeyID,
		RequestID:     requestID,
		Surface:       string(in.Surface),
		ModelSlug:     modelSlug,
		ProviderID:    route.ProviderID,
		ProviderKeyID: rot.keyID,
		RouteID:       rot.routeID(),
		Attempts:      attemptsJSON(rot.trailJSON()),
		// Not clamped to a minimum of one, unlike the success path: zero is a
		// real and useful value here. It says the request was refused by a gate
		// before any upstream was contacted, which is a different event from an
		// upstream that was tried and failed.
		RouteAttempts:   int32(rot.attempts),
		Status:          failureStatus(gerr.Code),
		ErrorCode:       gerr.Code,
		HttpStatus:      int32(statusOf(gerr.Code)),
		ChargedCurrency: id.WalletCurrency,
		ToolCalls:       []byte(`{}`),
		ServiceTier:     pgtype.Text{String: "", Valid: true},
		// A rejected request has no price to snapshot. The explicit empty object
		// describes that current state; replay payloads never use it.
		PricingSnapshot: []byte(`{}`),
		// This path never billed, so the markup is left NULL (not recorded) and
		// the discount records "no discount". The columns carry
		// CHECK(1..10000), and a zero would make the *entire* failure row fail
		// to insert -- exactly the row an investigation needs most.
		EndUserID:  endUserOf(in),
		DurationMs: int32(elapsed.Milliseconds()),
	})
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: writing the failure usage row failed", "error", err, "request_id", requestID)
	}
}

// failureStatus maps an error code onto the four states of the usage row's
// status column.
func failureStatus(code string) string {
	switch code {
	case errcode.GatewayUpstreamTimeout, errcode.GatewayAllProvidersFailed, errcode.GatewayInternal:
		return "upstream_error"
	default:
		return "client_error"
	}
}

// endUserOf resolves end-user attribution: the HTTP header wins over the body
// field.
func endUserOf(in Request) string {
	if in.EndUserID != "" {
		return in.EndUserID
	}
	return EndUserOf(in.Protocol, in.Body)
}

// clampInt32 fits a token count into int32, which is the column width. The
// upper bound itself is guarded by catalog.ValidateTokens.
func clampInt32(v int64) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}

// numericFromString turns the decimal exchange-rate string into a
// pgtype.Numeric, snapshotted on every charge.
func numericFromString(s string) pgtype.Numeric {
	var n pgtype.Numeric
	if s == "" {
		return n
	}
	if err := n.Scan(s); err != nil {
		return pgtype.Numeric{}
	}
	return n
}

// forwardWithFailover tries candidates in strategy order, classifies failures
// and advances the circuit breakers. See docs/design/failover-and-cooldowns.md.
//
// Three hard constraints:
//
//   - A client-class failure passes straight through and does not rotate: a
//     malformed parameter is malformed for every candidate, and retrying only
//     burns upstream quota.
//   - One request tries at most maxRouteAttempts routes, and is additionally
//     bounded by the global retry budget, which is what keeps a retry storm
//     from forming.
//   - When everything fails, return the *last* classified error rather than a
//     blanket all_providers_failed: the caller needs to know whether it was
//     quota, credentials or an upstream outage.
func (e *Executor) Execute(
	ctx context.Context, candidates []catalog.Route, in Request, model gwdb.Model,
	byok byokChoices, orgID pgtype.UUID, estimatedTokens int64,
) (upstreamResult, rotationResult) {
	p := e.pipeline
	var got upstreamResult
	rot := e.Rotate(ctx, candidates, in.Surface, estimatedTokens, func(ctx context.Context, route catalog.Route, _ int) attemptOutcome {
		res, cls, gerr := p.attemptOnce(ctx, route, in, model, byok, orgID)
		if gerr == nil {
			got = res
			return attemptOutcome{keyID: res.keyID, byok: res.byok}
		}
		// Whether the organization's own credential was used travels with the
		// failure too: the breaker's route-class reaction asks nothing on a
		// BYOK hit, and that decision cannot be made without it.
		return attemptOutcome{cls: cls, err: gerr, keyID: cls.keyID, byok: res.byok}
	})
	got.attempts = rot.attempts
	if rot.err == nil {
		return got, rot
	}
	return upstreamResult{attempts: rot.attempts, keyID: rot.keyID}, rot
}

// Rotate is the single failover state machine used by buffered and streaming
// delivery. The callback changes how one attempt is delivered, not which
// candidates are eligible or how retries, breakers and budgets advance.
//
// estimatedTokens is what this request is expected to consume upstream; it is
// what a provider's token allowance is charged. It is the same estimate the
// customer's own token limit was measured against, computed once in Prepare --
// two estimates of one request would be two different numbers.
func (e *Executor) Rotate(
	ctx context.Context, candidates []catalog.Route, surface catalog.Surface, estimatedTokens int64,
	attempt func(context.Context, catalog.Route, int) attemptOutcome,
) rotationResult {
	return e.pipeline.rotate(ctx, candidates, surface, estimatedTokens, attempt)
}

// availableRoutes drops providers whose circuit is open. Key-level breaking is
// decided when a credential is picked.
func (p *Pipeline) availableRoutes(ctx context.Context, candidates []catalog.Route) []catalog.Route {
	out := make([]catalog.Route, 0, len(candidates))
	for _, r := range candidates {
		if p.breaker.ProviderAvailable(ctx, r.ProviderID) {
			out = append(out, r)
		}
	}
	return out
}

// attemptOnce forwards once to a single candidate and returns the
// classification that drives the breakers.
func (p *Pipeline) attemptOnce(
	ctx context.Context, route catalog.Route, in Request, model gwdb.Model,
	byok byokChoices, orgID pgtype.UUID,
) (upstreamResult, Classification, *Error) {
	// A organization credential only applies to candidates at the same vendor: what
	// the organization configured is "my account at this platform". Rotating to
	// another company's endpoint -- even one speaking the same dialect -- falls
	// back to a shared credential, because the organization has no account there.
	choice, useBYOK := byok.forVendor(route.ProviderVendor)
	var (
		keyID   pgtype.UUID
		apiKey  string
		gerr    *Error
		baseURL = route.BaseURL
	)
	if in.PinnedProviderKeyID.Valid || in.PinnedOrgProviderKeyID.Valid {
		var pinnedBYOKKeyID pgtype.UUID
		keyID, apiKey, baseURL, pinnedBYOKKeyID, gerr = p.pinnedCredentialFor(ctx, route, orgID, in)
		useBYOK = pinnedBYOKKeyID.Valid
		choice = byokChoice{KeyID: pinnedBYOKKeyID, Secret: apiKey}
	} else if useBYOK {
		apiKey = choice.Secret
		if choice.BaseURL != "" {
			baseURL = choice.BaseURL // organization's own gateway; empty keeps the provider's endpoint
		}
	} else {
		// Do not look up a shared credential when using the organization's: it is a
		// wasted database round trip, and with no shared credentials
		// configured it manufactures a spurious "no usable key" error on every
		// request.
		keyID, apiKey, gerr = p.pickKey(ctx, route)
	}
	if gerr != nil {
		if gerr.Code == errcode.GatewayStateRouteUnavailable {
			return upstreamResult{byok: useBYOK}, Classification{Class: ClassTerminal}, gerr
		}
		return upstreamResult{byok: useBYOK}, Classification{Class: ClassProvider, CountsTowardHealth: true}, gerr
	}

	body := in.Body
	if len(body) > 0 {
		var err error
		body, err = RewriteRequest(in.Surface, body, route.ProviderModelID, false, route.Transport)
		if err != nil {
			return upstreamResult{byok: useBYOK}, Classification{Class: ClassClient},
				NewError(errcode.GatewayInvalidRequest, err.Error())
		}
	}
	req, err := BuildRequest(ctx, Target{
		Protocol: in.Protocol, BaseURL: baseURL, APIKey: apiKey,
		Path: in.UpstreamPath, Headers: MergeHeaders(route.ProviderHeaders, route.RouteHeaders),
		Transport: route.Transport, UpstreamModel: route.ProviderModelID,
		ExtraQuery: in.UpstreamQuery, Method: in.Method, Resource: in.Resource,
	}, body)
	if err != nil {
		return upstreamResult{byok: useBYOK}, Classification{Class: ClassProvider, CountsTowardHealth: true},
			NewError(errcode.GatewayInternal, err.Error())
	}

	resp, err := p.clientFor(in.Surface).Do(req)
	if err != nil {
		cls := ClassifyTransportError(ctx, err)
		cls.keyID = keyID
		if cls.Err == nil { // the client hung up
			return upstreamResult{byok: useBYOK}, cls, NewError(errcode.GatewayInternal, "Request was cancelled")
		}
		return upstreamResult{byok: useBYOK}, cls, cls.Err
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded rather than unbounded reads: a broken or compromised upstream can
	// exhaust memory with a huge response body, and the per-provider
	// concurrency cap is 64. Exceeding the bound *errors* rather than silently
	// truncating -- truncated JSON yields no usage and would quietly degrade
	// into estimated billing instead of failing where it can be seen.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		cls := ClassifyTransportError(ctx, err)
		cls.keyID = keyID
		return upstreamResult{byok: useBYOK}, cls, NewError(errcode.GatewayUpstreamTimeout, "Reading the upstream response failed")
	}
	if int64(len(respBody)) > maxUpstreamBody {
		slog.ErrorContext(ctx, "dataplane: upstream response body exceeded the limit", "provider", route.ProviderSlug, "limit", maxUpstreamBody)
		return upstreamResult{byok: useBYOK}, Classification{Class: ClassProvider, CountsTowardHealth: true},
			NewError(errcode.GatewayAllProvidersFailed, "Upstream response exceeded the size limit")
	}
	if resp.StatusCode >= 400 {
		cls := classifyUpstreamStatus(in, resp.StatusCode, respBody, resp.Header.Get("Retry-After"))
		cls.keyID = keyID
		if useBYOK {
			// On a organization credential, *no* upstream failure is a health signal
			// for this deployment. That hop used the organization's credential, their
			// quota, possibly even their own base URL -- failures on that path
			// say nothing about this deployment's own link to the provider.
			//
			// Clearing this only for 401/403, as it once did, meant a organization's
			// self-hosted gateway returning 500 would open the circuit between
			// this deployment and the provider: one organization could take that
			// provider away from everyone.
			cls.CountsTowardHealth = false
			// keyID is cleared for the same reason. It is already the zero
			// value, and leaving it would make a 429 (key class) record the
			// failure against a credential that does not exist -- one row
			// shared by every organization using their own key.
			cls.keyID = pgtype.UUID{}
		}
		if useBYOK && byokRejected(resp.StatusCode) {
			// Upstream rejected the organization's credential: mark it invalid so
			// later requests stop choosing it, and tell the organization.
			p.markBYOKInvalid(ctx, orgID, choice.KeyID, resp.StatusCode)
			if choice.Fallback {
				// Drop this vendor's credential from the request's set so a
				// later candidate at the same vendor retries on a shared one.
				// *Only this vendor's*: the org's credentials at other
				// platforms are separate accounts, and one rejection says
				// nothing about them.
				delete(byok, route.ProviderVendor)
				slog.WarnContext(ctx, "dataplane: organization credential rejected and fallback allowed; retrying on a shared credential at full price",
					"org_id", orgID, "vendor", route.ProviderVendor)
			}
		}
		return upstreamResult{byok: useBYOK}, cls, cls.Err
	}

	p.breaker.RecordSuccess(ctx, route.ProviderID, keyID)
	return upstreamResult{
		status: resp.StatusCode, body: respBody, keyID: keyID,
		byok: useBYOK, byokKeyID: byokKeyIDIfUsed(useBYOK, choice),
	}, Classification{}, nil
}

// applyBreaker advances the breaker at the level the classification names.
//
// surface is the one the request arrived on: a route-class failure is about
// one endpoint of one route, and that is what the probe worker is asked about.
func (p *Pipeline) applyBreaker(ctx context.Context, route catalog.Route, surface catalog.Surface, cls Classification, byok bool) {
	switch cls.Class {
	case ClassKey:
		authFailure := cls.CooldownHint == 0 && cls.Err != nil &&
			cls.Err.Code == errcode.GatewayAllProvidersFailed
		p.breaker.RecordKeyFailure(ctx, cls.keyID, cls.CooldownHint, authFailure)
	case ClassProvider:
		if cls.CountsTowardHealth {
			p.breaker.RecordProviderFailure(ctx, route.ProviderID, "upstream failure")
		}
	case ClassRoute:
		// The upstream says there is nothing at this endpoint for this
		// model. Do not cool down the whole provider, and do not decide
		// anything here either: ask the probe worker to look, and let its
		// verdict -- not this request -- take the route out of rotation for
		// the endpoint if that is what it finds. A request on the
		// organization's own credential says nothing about the shared route
		// and asks for nothing.
		endpoint, _ := surface.Endpoint()
		slog.WarnContext(ctx, "dataplane: upstream reports nothing at this endpoint for this route; asking for a probe",
			"provider", route.ProviderSlug, "upstream_model", route.ProviderModelID, "endpoint", endpoint)
		if p.requestProbe != nil && !byok && endpoint != "" {
			p.requestProbe(ctx, route.ID, endpoint)
		}
	}
}

// pickKey chooses a provider credential that is not cooling down, and decrypts
// it.
//
// The credentials of one provider are a pool, and requests are spread over it
// in round-robin: a counter per provider advances once per selection and
// decides where the scan starts. Taking the first usable one every time instead
// would put the whole load on the oldest credential and leave the others as
// standbys -- which is wrong for the case they mostly exist for, several keys
// on one account added precisely to share that account's quota. Spreading also
// keeps every credential warm, so a key that has been revoked upstream is found
// out by traffic rather than at the moment the first one finally trips.
//
// It is not a fair scheduler and does not try to be: keys are skipped when they
// are cooling down, so the sequence is only approximately even, and nothing
// here measures how much any one of them has actually consumed. Even coverage
// is the goal; exact balance is not.
func (p *Pipeline) pickKey(ctx context.Context, route catalog.Route) (pgtype.UUID, string, *Error) {
	keys, err := p.gw.GetProviderKeysForProvider(ctx, route.ProviderID)
	if err != nil || len(keys) == 0 {
		slog.ErrorContext(ctx, "dataplane: provider has no usable credential", "provider", route.ProviderSlug, "error", err)
		return pgtype.UUID{}, "", NewError(errcode.GatewayAllProvidersFailed, "Provider is unavailable")
	}
	start := p.nextKeyOffset(route.ProviderID, len(keys))
	for i := range keys {
		k := keys[(start+i)%len(keys)]
		if !p.breaker.KeyAvailable(ctx, k.ID) {
			continue // cooling down; try the next one
		}
		plain, err := p.box.Open(k.SecretEnc, k.ID.Bytes[:])
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: decrypting a provider credential failed", "provider", route.ProviderSlug, "error", err)
			continue
		}
		return k.ID, string(plain), nil
	}
	return pgtype.UUID{}, "", NewError(errcode.GatewayAllProvidersFailed, "Provider has no usable credential: all are cooling down or failed to decrypt")
}

// nextKeyOffset advances this provider's round-robin cursor and returns where
// the next scan should start.
//
// The cursor is per process and is not persisted: it decides nothing but which
// equally valid credential goes first, so a restart losing it costs nothing.
// With several replicas each keeps its own, which spreads the load just as
// well -- the one thing that must not happen is every replica always choosing
// the same key, and an independent cursor per replica does not do that.
func (p *Pipeline) nextKeyOffset(providerID pgtype.UUID, n int) int {
	if n <= 1 {
		return 0
	}
	key := uuidStr(providerID)
	v, _ := p.keyCursors.LoadOrStore(key, new(atomic.Uint64))
	cursor, _ := v.(*atomic.Uint64)
	// Add returns the post-increment value; the first caller therefore starts
	// at index 1 rather than 0. Which index a provider starts from is
	// arbitrary, so that costs nothing and avoids reasoning about wraparound.
	return int(cursor.Add(1) % uint64(n)) //nolint:gosec // n > 1 and the modulus keeps this inside int
}

// modelForRequest is the model this request is for, from the address when the
// protocol puts it there and from the body otherwise.
//
// One function rather than a branch at each reader, because "which model" is
// asked by routing, by pricing and by the usage row, and three answers that can
// disagree is how a request gets routed as one model and billed as another.
func modelForRequest(in Request) (string, error) {
	if in.Model != "" {
		return in.Model, nil
	}
	return ModelOf(in.Body)
}
