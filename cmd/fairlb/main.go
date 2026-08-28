// Command fairlb runs the community edition: the gateway, its admin UI, and
// a single-tenant identity model.
//
// Integrators use the exported gateway module and supply their own settlement,
// identity and organisation-access adapters. This command supplies Community's
// single-tenant adapters and public migrations.
//
// # Starting up
//
// `serve` is meant to be the only command anyone needs. It migrates, generates
// and stores a master key if none was supplied, creates the first administrator
// if the environment describes one, and otherwise points at the setup wizard.
// Nothing here requires a second command in a second terminal, because the step
// that requires one is the step people skip.
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/fairlb/fairlb/foundation/brand"
	"github.com/fairlb/fairlb/web"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/audit"
	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/foundation/o11y"
	"github.com/fairlb/fairlb/gateway"
	communitybootstrap "github.com/fairlb/fairlb/internal/community/bootstrap"
	communityconfig "github.com/fairlb/fairlb/internal/community/config"
	communityorgauthz "github.com/fairlb/fairlb/internal/community/orgauthz"
	communitysettle "github.com/fairlb/fairlb/internal/community/settle"
	communitystaffapi "github.com/fairlb/fairlb/internal/community/staffapi"
	communitystaffauth "github.com/fairlb/fairlb/internal/community/staffauth"
	"github.com/fairlb/fairlb/migrations"
	"github.com/fairlb/fairlb/settings"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stderr)) }

func run(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve()
	case "migrate":
		err = migrate()
	case "admin":
		err = adminCmd(args[1:], stdin)
	case "pricing":
		err = pricingCmd(args[1:], stdin, os.Stdout)
	case "healthcheck":
		err = healthcheck()
	case "config":
		err = configCmd(args[1:])
	case "version":
		fmt.Printf("fairlb %s (%s)\n", version, runtime.Version())
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "fairlb:", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: fairlb <command>

  serve                   run the gateway and the admin UI
  migrate                 apply database migrations and exit
  admin create            create an administrator
  admin reset-password    change an administrator's password
  pricing import          fill in model prices from the bundled reference dataset
  healthcheck             probe a running instance (used by the container health check)
  config check            validate and print the effective configuration (secrets redacted)
  version                 print the version

Configuration is read from the environment; see the README.
`)
}

func configCmd(args []string) error {
	if len(args) != 1 || args[0] != "check" {
		return errors.New("usage: fairlb config check")
	}
	src := config.OSSource()
	cfg, err := communityconfig.LoadRuntime(src)
	if err != nil {
		return err
	}
	return communityconfig.WriteCheck(os.Stdout, src, cfg)
}

// connectRuntime is the opening move for the running service and commands that
// need shared drivers in addition to the database.
//
// Migrations run on start rather than as a separate command. A two-step start
// has a first step somebody eventually forgets, and the symptom then is a
// running service whose tables do not exist.
func connectRuntime(ctx context.Context) (communityconfig.Config, *pgxpool.Pool, error) {
	cfg, err := communityconfig.LoadRuntime(config.OSSource())
	if err != nil {
		return communityconfig.Config{}, nil, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return communityconfig.Config{}, nil, err
	}
	if err := db.Migrate(ctx, pool, migrations.Community); err != nil {
		pool.Close()
		return communityconfig.Config{}, nil, err
	}
	return cfg, pool, nil
}

func connectDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := communityconfig.LoadDatabase(config.OSSource())
	if err != nil {
		return nil, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool, migrations.Community); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func migrate() error {
	ctx := context.Background()
	pool, err := connectDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	fmt.Println("migrations applied")
	return nil
}

// adminPassword takes the password from the environment or from stdin, never
// from the command line.
//
// An argument would land in the shell history and in the output of `ps` for
// every user on the machine, for as long as the command runs. Both are the sort
// of leak nobody thinks to clean up, because nothing about typing the command
// suggests it happened.
func adminPassword(stdin io.Reader) (string, error) {
	p, err := config.Secret(config.OSSource(), "FAIRLB_ADMIN_PASSWORD")
	if err != nil {
		return "", err
	}
	if p != "" {
		return p, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return "", fmt.Errorf("reading the password from stdin: %w", err)
	}
	p = strings.TrimRight(string(b), "\r\n")
	if p == "" {
		return "", errors.New(
			"no password given. Set FAIRLB_ADMIN_PASSWORD or FAIRLB_ADMIN_PASSWORD_FILE, or pipe it in:\n" +
				"      printf %s 'your password' | fairlb admin create you@example.com")
	}
	return p, nil
}

func adminCmd(args []string, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: fairlb admin create|reset-password <email> [name]")
	}
	action := args[0]
	if action != "create" && action != "reset-password" {
		return fmt.Errorf("unknown admin command %q (want create or reset-password)", action)
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: fairlb admin %s <email> [name]", action)
	}
	// Anyone following an older README will pass the password here. Say what to
	// do instead rather than treating it as a name.
	if action == "create" && len(args) > 3 {
		return errors.New("too many arguments. The password is not passed on the " +
			"command line (it would end up in your shell history and in `ps`); " +
			"set FAIRLB_ADMIN_PASSWORD or pipe it to stdin")
	}
	email := args[1]
	name := communitybootstrap.DefaultAdminName
	if action == "create" && len(args) == 3 {
		name = args[2]
	}

	password, err := adminPassword(stdin)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := connectDatabase(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	if action == "reset-password" {
		if err := communitybootstrap.SetPassword(ctx, pool, email, password); err != nil {
			return err
		}
		fmt.Printf("password updated for %s\n", email)
		return nil
	}
	if err := communitybootstrap.CreateAdmin(ctx, pool, email, password, name); err != nil {
		return err
	}
	fmt.Printf("administrator created: %s\n", email)
	return nil
}

// healthcheck exists because the container image has no shell and no curl —
// a distroless base is the right default for a service holding provider
// credentials, and the cost of that is that HEALTHCHECK has to be a subcommand.
func healthcheck() error {
	cfg := communityconfig.LoadProbe(config.OSSource())
	addr := cfg.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz") //nolint:noctx // short-lived CLI probe
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", resp.StatusCode)
	}
	return nil
}

func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, pool, err := connectRuntime(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	slog.SetDefault(o11y.NewLoggerFormat(cfg.LogFormat, os.Stderr))

	// The master key encrypts provider credentials at rest. An unset SECRET_KEY
	// means "keep one for me" rather than "run without encryption"; see
	// bootstrap.LoadOrCreateSecretKey for why generating one per start would be
	// worse than either.
	secret := cfg.SecretKey
	if len(secret) == 0 {
		if secret, err = communitybootstrap.LoadOrCreateSecretKey(cfg.DataDir); err != nil {
			return err
		}
		slog.Info("using the master key stored in the data directory",
			"path", cfg.DataDir+"/"+communitybootstrap.SecretKeyFile)
	}
	box, err := crypto.NewBox(secret)
	if err != nil {
		return err
	}

	// Every product table carries org_id and the row-level policies key off it,
	// so the single-tenant shape is one fixed organisation rather than no
	// organisation at all. The mechanism keeps working; the UI never says "org".
	orgID, err := communitybootstrap.EnsureDefaultOrg(ctx, pool)
	if err != nil {
		return err
	}
	if err := firstAdmin(ctx, cfg, pool); err != nil {
		return err
	}

	drv, err := drivers.New(ctx, cfg.Config, pool)
	if err != nil {
		return err
	}

	// "self-hosted" rather than a tier name: the resource attribute wants an
	// environment, and this build does not have dev/staging/production to
	// report. Anyone shipping telemetry somewhere can tell instances apart by
	// the usual OTEL_RESOURCE_ATTRIBUTES.
	tel, err := o11y.Setup(ctx, "self-hosted", version)
	if err != nil {
		return err
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(shutCtx)
	}()

	// 注册表在这里建：这个二进制装的是 gateway 那一层，于是设置页恰好呈现那一层的键。
	// 此前它由各包的 init() 塞进一个包级 map，页面上有什么由链接集决定（ADR-0194）。
	set := settings.New(pool, drv.Cache, settings.NewRegistry(gateway.SettingSpecs()), box)

	// Settlement has a community stand-in: Hold and Void do nothing and
	// SettleTx only accumulates per-key spend, which keeps the spend-limit
	// enforcement in the data plane working while wallets and the ledger stay
	// out of this build entirely.
	settler := communitysettle.New(pool)

	// Key management is the shared implementation with one thing swapped: who
	// may administer keys. Here everyone who is signed in may, because there is
	// only one identity to be.
	invalidateKey := gateway.NewKeyInvalidator(drv.Cache)
	modelAdmission := gateway.NewModelAdmission(pool)
	keys := apikeys.NewService(apikeys.ServiceConfig{
		Database: pool,
		Admin:    communitystaffapi.AllowKeyAdmin,
		Invalidator: func(ctx context.Context, keyHash string) {
			if err := invalidateKey(ctx, keyHash); err != nil {
				slog.ErrorContext(ctx, "could not invalidate the data plane key cache; "+
					"revocation will take effect when the entry expires", "error", err)
			}
		},
		// A key may not be pointed at a model its team cannot reach. Without
		// this the extra entries are simply dead: they resolve to 404 at call
		// time, and the key reads as configured for something it can never do.
		ModelAdmission: modelAdmission,
	})
	workers := jobs.NewWorkers()
	// The gateway module's jobs plus the maintenance this build owns directly.
	//
	// The second group is not optional and not a hosted-only concern: this
	// process writes audit rows and idempotency keys through its own
	// middleware, and both of those tables need a job to stay usable. Taking
	// the whole periodic set from gateway.PeriodicJobs() alone is what left
	// them unattended -- the audit table then has only the two months the
	// migration pre-creates, after which every row lands in the catch-all
	// partition that the schema's own comment describes as the sign this job
	// has stopped.
	river.AddWorker(workers, audit.NewPartitionWorker(pool))
	river.AddWorker(workers, httpx.NewIdempotencyReapWorker(pool))
	// Built explicitly rather than appended onto gateway.PeriodicJobs(). That
	// append is only safe while the callee happens to return a fresh slice with
	// no spare capacity; the day it returns a package-level slice, or one built
	// with room to grow, this would write into the gateway package's own array
	// and the corruption would be silent -- jobs registered, just not these.
	periodic := make([]*river.PeriodicJob, 0, len(gateway.PeriodicJobs())+2)
	periodic = append(periodic, gateway.PeriodicJobs()...)
	periodic = append(periodic,
		audit.PartitionPeriodicJob(),
		httpx.IdempotencyReapPeriodicJob(),
	)
	riverClient, err := jobs.NewWorkerClient(pool, jobs.WorkerConfig{
		Workers: workers, PeriodicJobs: periodic,
	})
	if err != nil {
		return err
	}
	gatewayModule, err := gateway.NewModule(gateway.Dependencies{
		Database: pool, Settlement: settler,
		OrganizationAccess: communityorgauthz.New(pool),
		AlertSink: gateway.AlertFunc(func(ctx context.Context, subject, detail string) {
			slog.ErrorContext(ctx, subject, "detail", detail)
		}),
		OrgNotifier: gateway.OrgNotifierFunc(func(context.Context, pgtype.UUID, pgtype.UUID, int) error {
			return nil // the single operator already owns and observes this credential
		}),
		Settings: set, Cipher: box, Cache: drv.Cache, RateLimit: drv.RateLimit,
		Breaker: drv.Breaker, Jobs: riverClient, ProbeTrace: cfg.ProbeTrace,
	})
	if err != nil {
		return err
	}
	if err := gatewayModule.RegisterWorkers(workers); err != nil {
		return err
	}
	if err := riverClient.Start(ctx); err != nil {
		return fmt.Errorf("river start: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := riverClient.Stop(stopCtx); err != nil {
			slog.Error("river stop", "error", err)
		}
	}()

	staffSvc := communitystaffauth.New(pool)
	healthChecks := gatewayModule.HealthChecks()
	healthChecks["db"] = pool.Ping
	health := httpx.NewHealth(healthChecks)

	// The brand is resolved once, here, before anything is served. A named
	// bundle that will not load stops startup: the page falls back to the
	// default profile when it carries none, so a half-loaded brand would be
	// served as FairLB with nothing said (ADR-0214).
	adminUI, err := brand.Serve(web.StaffDist(), cfg.BrandProfileDir, brand.SurfaceCommunityAdmin)
	if err != nil {
		return err
	}
	if adminUI.Name != "" {
		brand.Name = adminUI.Name
	}
	r := buildRouter(cfg, pool, gatewayModule, staffSvc, drv, orgID, keys, health, set, adminUI.FS)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		// Wrapped the same way the hosted build wraps its router. Server spans
		// were the one signal the two products did not share, which meant a bug
		// reproduced here could not be followed the way the same bug is
		// followed there. With no OTLP endpoint configured -- the normal state
		// for a self-hosted deployment -- sampling is off, so this costs a
		// non-recording span and nothing more.
		//
		// Its metrics are suppressed deliberately. Spans are what this wrapper
		// was added for; the HTTP duration histogram it also records is not
		// governed by the sampler, so it would be a second per-request
		// recording on the data plane measuring the same requests as
		// gateway_request_duration_seconds with narrower default buckets --
		// two series that disagree about p95 on long completions, for traffic
		// the gateway histogram already covers. The hosted build keeps its
		// HTTP metrics because there they are the only signal the control
		// planes have; here the control plane is one operator's admin UI.
		Handler:           otelhttp.NewHandler(r, "server", otelhttp.WithMeterProvider(metricnoop.NewMeterProvider())),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Operational endpoints listen separately so that /metrics is not reachable
	// from wherever the gateway itself is reachable. Nothing publishes this
	// port by default.
	opsSrv := &http.Server{
		Addr:              cfg.InternalAddr,
		Handler:           opsRouter(tel, health),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("fairlb started",
		"addr", cfg.HTTPAddr, "internal", cfg.InternalAddr,
		"public_url", cfg.PublicURL.String(), "version", version, "org", orgIDString(orgID))

	errCh := make(chan error, 2)
	go func() { errCh <- opsSrv.ListenAndServe() }()
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	return httpx.GracefulShutdown(httpx.ShutdownConfig{
		Health: health, DrainGrace: cfg.DrainGrace, Timeout: cfg.ShutdownTimeout,
		HTTPServers: []*http.Server{srv, opsSrv},
	})
}

// opsRouter serves liveness, readiness and metrics on the internal address.
//
// /healthz answers "is this process alive", /readyz answers "can it serve" —
// they differ exactly when the database is unreachable, which is the case an
// orchestrator most needs to tell apart from a crash.
func opsRouter(tel *o11y.Telemetry, health *httpx.Health) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover)
	r.Get("/healthz", health.Up)
	r.Get("/readyz", health.Healthz)
	r.Method(http.MethodGet, "/metrics", tel.MetricsHandler())
	return r
}

// firstAdmin creates the initial administrator from the environment, or says
// where to create one.
//
// Two paths on purpose. Automated installs describe the account in the compose
// file and never touch a browser; everyone else gets a link. What there is not
// is a path where the service starts, looks healthy, and cannot be signed in to
// without a command the operator has not been told about.
func firstAdmin(ctx context.Context, cfg communityconfig.Config, pool *pgxpool.Pool) error {
	exists, err := communitybootstrap.AdminExists(ctx, pool)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if cfg.AdminEmail != "" && cfg.AdminPassword != "" {
		err := communitybootstrap.CreateFirstAdmin(ctx, pool, cfg.AdminEmail, cfg.AdminPassword, "")
		switch {
		case errors.Is(err, communitybootstrap.ErrAlreadyConfigured):
			// Another replica won the race; that is the outcome we wanted.
			return nil
		case err != nil:
			return err
		}
		slog.Info("created the first administrator from the environment", "email", cfg.AdminEmail)
		return nil
	}
	slog.Info("no administrator yet — finish setup in your browser",
		"url", cfg.PublicURL.String()+"/setup")
	return nil
}

func orgIDString(id pgtype.UUID) string {
	b, err := id.MarshalJSON()
	if err != nil || len(b) < 2 {
		return ""
	}
	return string(b[1 : len(b)-1])
}
