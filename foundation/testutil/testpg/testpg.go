// Package testpg provides a real PostgreSQL test fixture backed by containers.
// Tests that need a container are skipped under `go test -short`.
//
// Each test gets its own database, but the whole test process starts one
// container and runs the migrations once per schema set: the migrated result
// becomes a template, and each test clones it with CREATE DATABASE ... TEMPLATE,
// which is a file-level copy and takes tens of milliseconds. One container plus a
// full migration per test costs seconds instead, multiplied by every call site
// and by every pass the verification suite makes.
//
// Isolation is unchanged: databases cannot see each other, so a test may alter
// the schema, load data, even drop constraints. The only shared objects are
// cluster-level ones such as roles, which the migrations create and which are
// meant to be globally unique anyway.
//
// One container can hold several templates, one per migration set: a build that
// ships a different set of migrations has to be tested against the schema it
// actually ships, not against a superset it will never see. Start uses the
// self-hosted set; StartWith takes any other set by name.
package testpg

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/migrations"
)

const (
	// bootDB is the database the container is created with. Templates are
	// created next to it; this one only supplies the shape of the connection
	// string and is never migrated.
	bootDB     = "fairlb_tpl"
	pgUser     = "fairlb"
	pgPassword = "fairlb"
)

var (
	bootOnce sync.Once
	shared   *base
	bootErr  error
	dbSeq    atomic.Int64

	tplMu sync.Mutex
	tpls  = map[string]*template{}
)

// base is the process-wide container and its admin connection.
type base struct {
	// admin connects to the maintenance database: both cloning and dropping
	// have to run from outside the target database.
	admin  *pgxpool.Pool
	dsnFmt func(dbName string) string
}

// template is one migrated clone source. It is built at most once per name,
// on first use — a package that never asks for a given schema never pays for it.
type template struct {
	once sync.Once
	name string
	err  error
}

// Start returns a pool for a freshly cloned, already-migrated database, cleaned
// up when the test ends.
// The schema is the self-hosted one (core + product), which is what everything
// outside the hosted-only packages runs against.
func Start(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return StartWith(t, "community", migrations.Community)
}

// StartWith is Start against a named migration set. The name identifies the
// template database, so callers passing different sets must pass different
// names; passing the same name with a different set returns the first one built.
//
// gate-honesty: a skip under -short does not show up in the default `go test`
// output — without -v, a package full of skipped tests still prints
// `ok <pkg> 0.4s`, exactly like a package where everything passed. Two things
// compensate: the fast pre-push hook uses -short deliberately and is not the
// merge criterion, and the merge criterion runs the tests without -short, where
// the container tests necessarily do run. So a skip only ever happens on the
// pass that is not the criterion.
func StartWith(t *testing.T, schema string, fsys fs.FS) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}

	bootOnce.Do(bootstrap)
	if bootErr != nil {
		t.Fatalf("testpg: fixture bootstrap: %v", bootErr)
	}

	tpl, err := ensureTemplate(schema, fsys)
	if err != nil {
		t.Fatalf("testpg: template %s: %v", schema, err)
	}

	ctx := context.Background()
	name := fmt.Sprintf("flb_t%d", dbSeq.Add(1))
	// The identifier comes from an internal counter; there is no external input.
	if _, err := shared.admin.Exec(ctx,
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tpl)); err != nil {
		t.Fatalf("testpg: clone template database: %v", err)
	}

	pool, err := db.Connect(ctx, shared.dsnFmt(name), 0)
	if err != nil {
		t.Fatalf("testpg: connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Reclaim once disconnected. A failure is logged rather than failed:
		// the container is destroyed wholesale when the process exits.
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := shared.admin.Exec(dropCtx,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s (FORCE)", name)); err != nil {
			t.Logf("testpg: could not drop %s (does not affect the result): %v", name, err)
		}
	})
	return pool
}

// bootstrap starts the container and connects to the admin database. It runs no
// migrations: templates are built on first use (see ensureTemplate), so a
// package pays only for the schemas it actually asks for.
//
// The container is not tied to any single test: it has to outlive all of them.
// Reclamation is left to the container library's reaper, which destroys
// everything when the test process exits — a lifetime that already matches
// `go test`.
func bootstrap() {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:18",
		tcpostgres.WithDatabase(bootDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPassword),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		bootErr = fmt.Errorf("start container: %w", err)
		return
	}

	bootDSN, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		bootErr = fmt.Errorf("read connection string: %w", err)
		return
	}

	dsnFmt := func(dbName string) string {
		return strings.Replace(bootDSN, "/"+bootDB+"?", "/"+dbName+"?", 1)
	}
	admin, err := db.Connect(ctx, dsnFmt("postgres"), 4)
	if err != nil {
		bootErr = fmt.Errorf("connect to the maintenance database: %w", err)
		return
	}
	shared = &base{admin: admin, dsnFmt: dsnFmt}
}

// ensureTemplate builds the template database for one migration set, once, and
// returns its name. Concurrent callers of the same name wait for the first.
func ensureTemplate(schema string, fsys fs.FS) (string, error) {
	tplMu.Lock()
	t := tpls[schema]
	if t == nil {
		t = &template{name: "flb_tpl_" + schema}
		tpls[schema] = t
	}
	tplMu.Unlock()

	t.once.Do(func() { t.err = buildTemplate(t.name, fsys) })
	return t.name, t.err
}

func buildTemplate(name string, fsys fs.FS) error {
	ctx := context.Background()
	// The identifier is built from a caller-supplied schema name; every caller
	// is in-tree and passes a constant, so there is no external input here.
	if _, err := shared.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	pool, err := db.Connect(ctx, shared.dsnFmt(name), 0)
	if err != nil {
		return fmt.Errorf("connect %s: %w", name, err)
	}
	migrateErr := db.Migrate(ctx, pool, fsys)
	pool.Close()
	if migrateErr != nil {
		return fmt.Errorf("migrate %s: %w", name, migrateErr)
	}

	// A template with a live connection cannot be cloned, and pool.Close only
	// promises that this process let go — a backend that has not finished
	// exiting yet would turn the next clone into a flake instead of a failure.
	// Cutting the rest deterministically costs one statement.
	if _, err := shared.admin.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = $1 AND pid <> pg_backend_pid()`, name); err != nil {
		return fmt.Errorf("detach from %s: %w", name, err)
	}
	return nil
}
