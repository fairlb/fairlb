// Package gateway is the public integration surface of the gateway product.
//
// Deployments assemble this package and do not reach into internal/gateway.
// The module owns the HTTP handlers and the complete River worker set, which
// keeps Cloud and Community behavior identical unless a difference is expressed
// by one of the ports below.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/foundation/alert"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
	"github.com/fairlb/fairlb/internal/gateway/settle"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
	"github.com/fairlb/fairlb/settings"
	publicusage "github.com/fairlb/fairlb/usage"
)

// HoldInput and SettleInput are the deployment-neutral accounting messages.
// Aliases keep the public port and the internal request path on one exact type;
// consumers still import only this package.
type HoldInput = settle.HoldInput
type SettleInput = settle.SettleInput

// ArtifactStore is where finished video output lives.
//
// An alias rather than a second declaration, for the reason the settlement port
// gives: the request path is written against the internal type, and two
// identical interfaces would let the public port and the internal one drift
// apart while both still compile.
//
// Leaving it nil binds the no-custody store: the deployment still serves video,
// it proxies the bytes from the upstream on read for as long as the upstream
// has them (ADR-0222).
type ArtifactStore = proxy.Artifacts

// ArtifactRef and ArtifactInfo are the deployment-neutral messages that cross
// that port.
type ArtifactRef = proxy.ArtifactRef
type ArtifactInfo = proxy.ArtifactInfo

// ErrArtifactGone tells the data plane an artifact is past its retention, which
// is a normal outcome rather than a fault.
var ErrArtifactGone = proxy.ErrArtifactGone

// Settlement is the accounting port. Community records key spend; Cloud also
// owns reservations, wallet balance and ledger postings.
//
// An alias rather than a second declaration, for the reason given above: the
// gateway's request path is written against settle.Settler, and two identical
// interfaces would let the public port and the internal one drift apart while
// both still compile.
type Settlement = settle.Settler

// OrganizationAccess is the identity/organization authorization port used by the
// gateway console. It deliberately says nothing about memberships or roles.
type OrganizationAccess interface {
	ResolveOrgReadAccess(context.Context, pgtype.UUID) (finance bool, keyMetadata bool, err error)
	AuthorizeOrgAdminRead(context.Context, pgtype.UUID) error
	AuthorizeOrgWrite(context.Context, pgtype.UUID) error
}

// AlertSink receives operational faults which need human attention.
//
// Kept as a name on this façade because callers assemble against it, but it is
// the foundation port (ADR-0190) — the same shape every raiser speaks.
type AlertSink = alert.Sink

// OrgNotifier reports a organization-owned upstream credential being rejected.
// Implementations decide how notification is made durable.
type OrgNotifier interface {
	BYOKInvalid(context.Context, pgtype.UUID, pgtype.UUID, int) error
}

// AlertFunc adapts a function to AlertSink.
type AlertFunc func(context.Context, string, string)

func (f AlertFunc) Alert(ctx context.Context, subject, detail string) { f(ctx, subject, detail) }

// OrgNotifierFunc adapts a function to OrgNotifier.
type OrgNotifierFunc func(context.Context, pgtype.UUID, pgtype.UUID, int) error

func (f OrgNotifierFunc) BYOKInvalid(ctx context.Context, orgID, keyID pgtype.UUID, status int) error {
	return f(ctx, orgID, keyID, status)
}

// Dependencies is the complete construction contract. Required ports are
// validated by NewModule; optional infrastructure is documented on its field.
type Dependencies struct {
	Database           *pgxpool.Pool
	Settlement         Settlement
	OrganizationAccess OrganizationAccess
	AlertSink          AlertSink
	OrgNotifier        OrgNotifier
	Settings           *settings.Store
	Cipher             *crypto.Box
	Cache              cache.Store // nil disables caching
	RateLimit          ratelimit.Limiter
	Breaker            breaker.Store
	Jobs               *river.Client[pgx.Tx]
	Artifacts          ArtifactStore // nil takes no custody of video output
	HTTPClient         *http.Client  // nil uses the hardened shared transport
	ProbeTrace         bool          // may expose plaintext credentials; dev/staging only
}

// Planes are already-protected subrouters supplied by the host application.
// Module only adds relative routes and never changes their middleware stacks.
type Planes struct {
	DataPlane chi.Router
	// DataPlaneV1Beta carries the Gemini protocol's endpoint under the version
	// prefix its clients default to. It is its own router rather than the data
	// plane mounted twice: mounting the whole plane there would answer a Gemini
	// client's GET /v1beta/models with the OpenAI-shaped catalogue -- a 200 of
	// the wrong shape, which that SDK reads as "this deployment serves no
	// models" and reports without an error.
	DataPlaneV1Beta chi.Router
	// DataPlaneVideoNative carries the vendor compatibility surfaces, which sit
	// at the API root rather than under a version prefix: each vendor's own
	// paths carry their own version segment and no two of them agree
	// (/v1, /api/v3, /v1beta, /api/v1). A caller switching over sets their base
	// URL to <this>/video/<vendor> and their SDK appends the rest unchanged.
	DataPlaneVideoNative chi.Router
	Console              chi.Router
	Admin                chi.Router
}

// Module is an immutable, fully wired gateway product.
type Module struct {
	pool     *pgxpool.Pool
	posting  *publicusage.PostingStore
	gw       *gwdb.Queries
	settings *settings.Store
	box      *crypto.Box
	alerts   AlertSink
	settle   Settlement
	jobs     *river.Client[pgx.Tx]
	catalog  *catalog.Service
	auth     *proxy.Authenticator
	guard    *proxy.Guard
	pipeline *proxy.Pipeline
	console  *gwconsoleapi.Server
	admin    *gwstaffapi.Server
}

// NewModule validates and constructs every gateway collaborator at once.
func NewModule(deps Dependencies) (*Module, error) {
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}
	keys := apikeys.NewStore(deps.Database)
	orgs := organizations.New(deps.Database)
	gw := gwdb.New(deps.Database)
	cat := catalog.NewService(gw, deps.Cache, deps.Settings)
	auth := proxy.NewAuthenticator(keys, orgs, gw, deps.Cache)
	guard := proxy.NewGuard(keys, deps.RateLimit)
	probes := routeprobe.NewService(deps.Database, deps.Jobs)
	pipeline := proxy.NewPipeline(proxy.PipelineConfig{
		Pool: deps.Database, Gateway: gw, Catalog: cat,
		Authenticator: auth, Guard: guard, Settlement: deps.Settlement, Artifacts: deps.Artifacts,
		Cipher: deps.Cipher, HTTPClient: deps.HTTPClient,
		BreakerStore: deps.Breaker, RateLimit: deps.RateLimit,
		Alerter: deps.AlertSink,
		BYOKNotifier: func(ctx context.Context, orgID, keyID pgtype.UUID, status int) {
			if err := deps.OrgNotifier.BYOKInvalid(ctx, orgID, keyID, status); err != nil {
				deps.AlertSink.Alert(ctx, "Organization BYOK notification failed", err.Error())
			}
		},
		ProbeRequester: probes.Request,
	})
	console := gwconsoleapi.NewServer(gwconsoleapi.ServerConfig{
		Pool: deps.Database, OrganizationAccess: deps.OrganizationAccess,
		Catalog: cat, Cipher: deps.Cipher, ProbeClient: deps.HTTPClient,
		// The console reads and cancels video jobs through the pipeline's own
		// job surface rather than a second implementation of it: what a cancel
		// means, and whether it leaves the customer charged, has to be one
		// answer (ADR-0225).
		VideoJobs: pipeline.VideoJobs(),
	})
	admin := gwstaffapi.NewServer(gwstaffapi.ServerConfig{
		Pool: deps.Database, Catalog: cat, Breaker: deps.Breaker,
		Budget: pipeline.Budget(), Cipher: deps.Cipher, HTTPClient: deps.HTTPClient,
		Cache: deps.Cache, PricingAlerter: deps.AlertSink, Jobs: deps.Jobs,
		ProbeTrace: deps.ProbeTrace,
	})
	if err := pipeline.Breaker().RestoreCooldowns(context.Background()); err != nil {
		return nil, fmt.Errorf("gateway: restore circuit breaker state: %w", err)
	}
	return &Module{
		pool: deps.Database, posting: publicusage.NewPostingStore(deps.Database), gw: gw, settings: deps.Settings,
		box: deps.Cipher, alerts: deps.AlertSink, settle: deps.Settlement,
		jobs: deps.Jobs, catalog: cat, auth: auth, guard: guard,
		pipeline: pipeline, console: console, admin: admin,
	}, nil
}

func validateDependencies(d Dependencies) error {
	var missing []string
	if d.Database == nil {
		missing = append(missing, "Database")
	}
	if d.Settlement == nil {
		missing = append(missing, "Settlement")
	}
	if d.OrganizationAccess == nil {
		missing = append(missing, "OrganizationAccess")
	}
	if d.AlertSink == nil {
		missing = append(missing, "AlertSink")
	}
	if d.OrgNotifier == nil {
		missing = append(missing, "OrgNotifier")
	}
	if d.Settings == nil {
		missing = append(missing, "Settings")
	}
	if d.Cipher == nil {
		missing = append(missing, "Cipher")
	}
	if d.RateLimit == nil {
		missing = append(missing, "RateLimit")
	}
	if d.Breaker == nil {
		missing = append(missing, "Breaker")
	}
	if d.Jobs == nil {
		missing = append(missing, "Jobs")
	}
	if len(missing) != 0 {
		return fmt.Errorf("gateway: missing required dependencies: %v", missing)
	}
	return nil
}

// Mount registers all gateway HTTP surfaces on the supplied protected planes.
func (m *Module) Mount(p Planes) error {
	if p.DataPlane == nil || p.DataPlaneV1Beta == nil || p.DataPlaneVideoNative == nil ||
		p.Console == nil || p.Admin == nil {
		return errors.New("gateway: DataPlane, DataPlaneV1Beta, DataPlaneVideoNative, " +
			"Console and Admin planes are required")
	}
	p.DataPlane.Get("/public/models", m.catalog.PublicModelsHandler())
	p.DataPlane.Get("/models", proxy.ModelsHandler(m.auth, m.guard, m.catalog))
	m.pipeline.Mount(p.DataPlane)
	m.pipeline.MountGemini(p.DataPlaneV1Beta)
	// The video job plane. Idempotency is a property of the job resource rather
	// than middleware here -- see MountVideos for why the shared one could not
	// carry it (ADR-0172, ADR-0220).
	m.pipeline.MountVideos(p.DataPlane)
	// The same jobs reached at each vendor's own paths, so that a caller who
	// already wrote against one of them switches by changing a base URL. Not
	// passthrough: the shapes are theirs, the job is ours.
	m.pipeline.MountVideoNative(p.DataPlaneVideoNative)

	gwconsoleapi.HandlerFromMux(
		gwconsoleapi.NewStrictHandlerWithOptions(m.console,
			[]gwconsoleapi.StrictMiddlewareFunc{
				gwconsoleapi.RequireManagementScope,
				gwconsoleapi.RequireImpersonatedOrg,
			},
			gwconsoleapi.StrictHTTPServerOptions{
				RequestErrorHandlerFunc:  httpx.OAPIRequestError,
				ResponseErrorHandlerFunc: httpx.OAPIResponseError,
			}),
		p.Console,
	)
	gwstaffapi.HandlerFromMux(
		gwstaffapi.NewStrictHandlerWithOptions(m.admin,
			[]gwstaffapi.StrictMiddlewareFunc{gwstaffapi.RequireStaff},
			gwstaffapi.StrictHTTPServerOptions{
				RequestErrorHandlerFunc:  httpx.OAPIRequestError,
				ResponseErrorHandlerFunc: httpx.OAPIResponseError,
			}),
		p.Admin,
	)
	return nil
}

// PeriodicJobs is the single schedule definition used by both products.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		gwusage.PartitionPeriodicJob(),
		gwusage.ProbePeriodicJob(),
		gwusage.AggregatePeriodicJob(),
		gwusage.UnsettledPeriodicJob(),
		gwusage.RevenueReconPeriodicJob(),
		gwusage.AnomalyPeriodicJob(),
		gwusage.AffinityGCPeriodicJob(),
		routeprobe.SweepPeriodicJob(),
		// The reconciler for asynchronous jobs. Nothing else observes a video
		// job's end: the request that created it returned in seconds, and the
		// caller may never come back (ADR-0220).
		proxy.VideoScanPeriodicJob(),
		proxy.VideoSweepPeriodicJob(),
	}
}

// RegisterWorkers adds the complete gateway worker family to the host's River
// registry. Callers must use this together with PeriodicJobs.
func (m *Module) RegisterWorkers(workers *river.Workers) error {
	if workers == nil {
		return errors.New("gateway: worker registry is required")
	}
	river.AddWorker(workers, gwusage.NewPartitionWorker(m.pool))
	river.AddWorker(workers, gwusage.NewProbeWorker(m.pool, m.gw, m.alerts))
	river.AddWorker(workers, routeprobe.NewWorker(m.pool, m.box, m.jobs))
	river.AddWorker(workers, routeprobe.NewSweepWorker(m.pool, m.jobs))
	river.AddWorker(workers, proxy.NewVideoScanWorker(m.pipeline))
	river.AddWorker(workers, proxy.NewVideoSweepWorker(m.pipeline))
	river.AddWorker(workers, gwusage.NewUnsettledWorker(m.pool, m.settle, m.alerts))
	river.AddWorker(workers, gwusage.NewRevenueReconWorker(m.pool, m.alerts))
	river.AddWorker(workers, gwusage.NewAnomalyWorker(m.pool, m.settings, m.alerts))
	river.AddWorker(workers, gwusage.NewAffinityGCWorker(m.pool))
	agg := gwusage.NewAggregator(m.pool, m.posting, m.gw)
	river.AddWorker(workers, gwusage.NewAggregateWorker(agg))
	return nil
}

// HealthChecks returns checks owned by this module for merging into the host's
// health registry.
func (m *Module) HealthChecks() map[string]func(context.Context) error {
	return map[string]func(context.Context) error{"gateway_catalog": m.catalog.TransportHealth}
}

// NewKeyInvalidator returns the cache callback key-management services use.
func NewKeyInvalidator(store cache.Store) func(context.Context, string) error {
	return func(ctx context.Context, keyHash string) error {
		if store == nil {
			return nil
		}
		return store.Delete(ctx, proxy.KeyCacheKey(keyHash))
	}
}

// NewModelAdmission returns the model gate callback used by key management.
func NewModelAdmission(pool *pgxpool.Pool) func(context.Context, pgtype.UUID, []string) ([]string, error) {
	q := gwdb.New(pool)
	return func(ctx context.Context, orgID pgtype.UUID, slugs []string) ([]string, error) {
		if len(slugs) == 0 {
			return nil, nil
		}
		return q.ModelsNotAdmittedForOrg(ctx, gwdb.ModelsNotAdmittedForOrgParams{OrgID: orgID, Slugs: slugs})
	}
}

// CurrencyMargin is one deployment-wide reporting row.
//
// An alias, for the same reason HoldInput and SettleInput above are aliases:
// re-declaring the shape here would mean a field-by-field copy on every call,
// and a copy loop is where a new field goes missing without anything failing.
type CurrencyMargin = gwusage.CurrencyMargin

// NewMarginSource returns the deployment-wide margin reader, built from the
// pool alone -- like NewModelAdmission above, and for a reason worth stating.
//
// This used to be a *Module method. Nothing about the query needed the module
// (it reads usage rollups through gwdb.New(pool)), but reaching it through the
// module made the consumer's assembly order impossible: Cloud's billing service
// is a dependency *of* the gateway module, so a billing service that wanted
// margins could only be built after the module existed -- that is, after
// billing had already been constructed and handed over. The workaround was to
// copy the billing service post-construction and backfill the field, which put
// two services with different behaviour into circulation at once. Taking the
// pool directly removes the ordering constraint and the cycle with it.
func NewMarginSource(pool *pgxpool.Pool) func(context.Context, time.Time, time.Time) ([]CurrencyMargin, error) {
	source := gwusage.NewMarginSource(gwdb.New(pool))
	return source.MarginByCurrency
}

// FirstRequestDone exposes the gateway-owned onboarding criterion.
func (m *Module) FirstRequestDone(ctx context.Context, tx pgx.Tx, orgID pgtype.UUID) (bool, error) {
	return gwusage.FirstRequestDone(ctx, m.gw, tx, orgID)
}

// SettingSpecs is every settings key this module's layer owns.
//
// 装配点拿它去建 `settings.Registry`。这一层的键此前由两个包各自的 `init()` 塞进
// 一个包级 map——那让**链接集决定设置页渲染什么**，而少一行在页面上看不出来
// （ADR-0194）。
func SettingSpecs() []settings.Spec {
	specs := catalog.Specs()
	return append(specs, gwusage.Specs()...)
}
