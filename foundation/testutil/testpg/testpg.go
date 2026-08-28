// Package testpg provides a real PostgreSQL test fixture backed by one shared
// server. Tests that need it are skipped under `go test -short`.
//
// Each test gets its own database, cloned with CREATE DATABASE ... TEMPLATE
// from a migrated template — a file-level copy that takes tens of milliseconds.
// The server and the templates are shared across test *processes*, not just
// within one: `go test ./...` runs every package in its own process, and a
// per-process server meant one container boot plus one full migration run per
// package — identical work repeated ~40 times per full verify (ADR-0211).
//
// The server is a machine-level container (fairlb-testpg-18) started on first
// use and left running: later packages, later runs and other checkouts reuse
// it, so they pay nothing for startup. Its data lives on tmpfs and is tuned
// for tests (fsync off); nothing durable lives there. Reset it at any time:
//
//	docker rm -f fairlb-testpg-18
//
// FAIRLB_TESTPG_PORT moves the published port; FAIRLB_TESTPG_DSN (a postgres://
// URL to a maintenance database) points the fixture at any existing server and
// skips container management entirely.
//
// Templates are addressed by content: flb_tpl_<schema>_<hash of the migration
// set>. A build that ships different migrations hashes differently and builds
// its own template, so a shared server can never hand out a stale schema.
// Cross-process coordination is a PostgreSQL advisory lock per template;
// cross-process clone names carry the pid. Isolation is unchanged: databases
// cannot see each other, so a test may alter the schema, load data, even drop
// constraints. The only shared objects are cluster-level ones such as roles,
// which the migrations create and which are meant to be globally unique anyway.
package testpg

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/migrations"
)

const (
	pgImage = "postgres:18"
	// The container name carries the major version: bumping the image starts a
	// fresh container instead of reusing one with an older server inside.
	containerName = "fairlb-testpg-18"
	defaultPort   = "54329"
	pgUser        = "fairlb"
	pgPassword    = "fairlb"
	// maintenanceDB is where cloning and dropping run from: both have to happen
	// from outside the target database. It is never migrated.
	maintenanceDB = "postgres"
)

var (
	bootOnce sync.Once
	shared   *base
	bootErr  error
	dbSeq    atomic.Int64

	tplMu    sync.Mutex
	tplBuilt = map[string]error{}
)

// base is the shared server's admin connection.
type base struct {
	admin  *pgxpool.Pool
	dsnFmt func(dbName string) string
}

// Start returns a pool for a freshly cloned, already-migrated database, cleaned
// up when the test ends.
// The schema is the self-hosted one (core + product), which is what everything
// outside the hosted-only packages runs against.
func Start(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return StartWith(t, "community", migrations.Community)
}

// StartWith is Start against a named migration set. The name labels the
// template database for humans; identity comes from hashing the set's content,
// so two callers passing the same name with different content get different
// templates rather than whichever was built first.
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

	tpl, err := templateName(schema, fsys)
	if err != nil {
		t.Fatalf("testpg: hash migration set %s: %v", schema, err)
	}
	if err := ensureTemplate(schema, tpl, fsys, false); err != nil {
		t.Fatalf("testpg: template %s: %v", tpl, err)
	}

	ctx := context.Background()
	// The pid keeps concurrent test processes (one per package) out of each
	// other's namespace; the sequence separates tests within a process.
	name := fmt.Sprintf("flb_t%d_%d", os.Getpid(), dbSeq.Add(1))
	// The identifier comes from the pid and an internal counter; there is no
	// external input.
	_, err = shared.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tpl))
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		// Another checkout on different migration content garbage-collected
		// this template between our existence check and the clone. Rebuild it
		// once; losing a template is recoverable by construction.
		if err = ensureTemplate(schema, tpl, fsys, true); err != nil {
			t.Fatalf("testpg: rebuild template %s: %v", tpl, err)
		}
		_, err = shared.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tpl))
	}
	if err != nil {
		t.Fatalf("testpg: clone template database: %v", err)
	}

	pool, err := db.Connect(ctx, shared.dsnFmt(name), 0)
	if err != nil {
		t.Fatalf("testpg: connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Reclaim once disconnected. A failure is logged rather than failed:
		// a leaked clone is swept by the next process's orphan pass.
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := shared.admin.Exec(dropCtx,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s (FORCE)", name)); err != nil {
			t.Logf("testpg: could not drop %s (does not affect the result): %v", name, err)
		}
	})
	return pool
}

// bootstrap connects to the shared server, starting its container if nothing
// answers. It runs no migrations: templates are built on first use, so a
// package pays only for the schemas it actually asks for.
func bootstrap() {
	adminDSN, dsnFmt, managed, err := serverDSN()
	if err != nil {
		bootErr = err
		return
	}

	ctx := context.Background()
	admin, err := connectAdmin(ctx, adminDSN, 2*time.Second)
	if err != nil {
		if !managed {
			bootErr = fmt.Errorf("FAIRLB_TESTPG_DSN is set but unreachable: %w", err)
			return
		}
		admin, err = startServer(ctx, adminDSN)
		if err != nil {
			bootErr = err
			return
		}
	}
	shared = &base{admin: admin, dsnFmt: dsnFmt}
	sweepOrphans(ctx, admin)
}

// serverDSN resolves where the server is: FAIRLB_TESTPG_DSN wins and disables
// container management; otherwise the managed container's fixed local port.
func serverDSN() (adminDSN string, dsnFmt func(string) string, managed bool, err error) {
	if raw := os.Getenv("FAIRLB_TESTPG_DSN"); raw != "" {
		u, perr := url.Parse(raw)
		if perr != nil || u.Scheme == "" {
			return "", nil, false, fmt.Errorf("FAIRLB_TESTPG_DSN must be a postgres:// URL: %q", raw)
		}
		return raw, func(dbName string) string {
			swapped := *u
			swapped.Path = "/" + dbName
			return swapped.String()
		}, false, nil
	}
	port := os.Getenv("FAIRLB_TESTPG_PORT")
	if port == "" {
		port = defaultPort
	}
	fmtDSN := func(dbName string) string {
		return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable",
			pgUser, pgPassword, port, dbName)
	}
	return fmtDSN(maintenanceDB), fmtDSN, true, nil
}

// connectAdmin polls the maintenance database until it answers or the budget
// runs out. The pool is small: it only clones, drops and coordinates.
func connectAdmin(ctx context.Context, dsn string, patience time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(patience)
	for {
		admin, err := db.Connect(ctx, dsn, 4)
		if err == nil {
			return admin, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// startServer brings the shared container up, serialized across processes with
// a file lock so exactly one of the packages racing at cold start creates it.
func startServer(ctx context.Context, adminDSN string) (*pgxpool.Pool, error) {
	lock, err := os.OpenFile(filepath.Join(os.TempDir(), "fairlb-testpg.flock"), os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open start lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("acquire start lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	// Whoever held the lock before us may have finished the job.
	if admin, err := connectAdmin(ctx, adminDSN, 2*time.Second); err == nil {
		return admin, nil
	}

	// A stopped container restarts; an absent or broken one is recreated.
	if exec.Command("docker", "start", containerName).Run() == nil {
		if admin, err := connectAdmin(ctx, adminDSN, 15*time.Second); err == nil {
			return admin, nil
		}
	}
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	port := os.Getenv("FAIRLB_TESTPG_PORT")
	if port == "" {
		port = defaultPort
	}
	// tmpfs holds PGDATA: nothing here is worth keeping, and file-level clones
	// out of memory are what makes per-test databases cheap. The server flags
	// trade crash durability — meaningless on tmpfs — for write speed.
	run := exec.Command("docker", "run", "-d",
		"--name", containerName,
		"-p", "127.0.0.1:"+port+":5432",
		"--tmpfs", "/var/lib/postgresql",
		"-e", "POSTGRES_USER="+pgUser,
		"-e", "POSTGRES_PASSWORD="+pgPassword,
		pgImage,
		"-c", "fsync=off",
		"-c", "synchronous_commit=off",
		"-c", "full_page_writes=off",
		"-c", "max_connections=500",
	)
	if out, err := run.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker run %s: %v\n%s", containerName, err, out)
	}
	admin, err := connectAdmin(ctx, adminDSN, 90*time.Second)
	if err != nil {
		return nil, fmt.Errorf("server did not answer after docker run: %w", err)
	}
	return admin, nil
}

// templateName derives the template's identity from the migration set's
// content, so schema changes roll templates automatically and two checkouts on
// different commits coexist on one server.
func templateName(schema string, fsys fs.FS) (string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, entry := range entries { // ReadDir returns entries sorted by name
		if entry.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return "", err
		}
		h.Write([]byte(entry.Name()))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return fmt.Sprintf("flb_tpl_%s_%x", schema, h.Sum(nil)[:6]), nil
}

// ensureTemplate builds one template, at most once per process; force clears
// that memo after a template was garbage-collected out from under us.
// Cross-process the build is serialized by an advisory lock keyed on the
// template name, so concurrent packages wait for the first builder instead of
// each migrating their own copy.
func ensureTemplate(schema, name string, fsys fs.FS, force bool) error {
	tplMu.Lock()
	defer tplMu.Unlock()
	if err, done := tplBuilt[name]; done && !force {
		return err
	}
	err := buildTemplate(schema, name, fsys)
	tplBuilt[name] = err
	return err
}

func buildTemplate(schema, name string, fsys fs.FS) error {
	ctx := context.Background()
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin connection: %w", err)
	}
	defer conn.Release()

	// Advisory locks are session-scoped, hence the pinned connection. The lock
	// space is per-database and this session is on the maintenance database, so
	// it cannot collide with advisory locks tests take inside their clones.
	key := int64(binary.BigEndian.Uint64(sha256sum(name)[:8]))
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("lock template build: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key) }()

	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check for template: %w", err)
	}
	if exists {
		return nil
	}

	// Build under a staging name and rename at the end: a process killed mid-
	// migration must not leave a half-built database under the final name,
	// where every later process would trust it. The staging name carries the
	// pid so the orphan sweep reclaims it like any other leaked database.
	stage := fmt.Sprintf("flb_t%d_stage%d", os.Getpid(), dbSeq.Add(1))
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", stage)); err != nil {
		return fmt.Errorf("create %s: %w", stage, err)
	}
	pool, err := db.Connect(ctx, shared.dsnFmt(stage), 0)
	if err != nil {
		return fmt.Errorf("connect %s: %w", stage, err)
	}
	migrateErr := db.Migrate(ctx, pool, fsys)
	pool.Close()
	if migrateErr != nil {
		_, _ = conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s (FORCE)", stage))
		return fmt.Errorf("migrate %s: %w", stage, migrateErr)
	}
	// A database with a live connection can be neither renamed nor cloned, and
	// pool.Close only promises that this process let go — a backend that has
	// not finished exiting yet would turn the rename into a flake instead of a
	// failure. Cutting the rest deterministically costs one statement.
	if _, err := conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = $1 AND pid <> pg_backend_pid()`, stage); err != nil {
		return fmt.Errorf("detach from %s: %w", stage, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", stage, name)); err != nil {
		return fmt.Errorf("publish template %s: %w", name, err)
	}

	// Retire this schema's templates for other migration content. No FORCE: a
	// template another checkout is cloning from right now refuses the drop and
	// simply survives until it really is idle.
	rows, err := conn.Query(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE $1 AND datname <> $2",
		"flb_tpl_"+schema+"_%", name)
	if err != nil {
		return nil
	}
	var stale []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			stale = append(stale, d)
		}
	}
	rows.Close()
	for _, d := range stale {
		_, _ = conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", d))
	}
	return nil
}

// sweepOrphans reclaims databases left by processes that died without their
// cleanups running (SIGKILL, panic in a harness). Ownership is readable from
// the name — flb_t<pid>_... — and the server is local, so a dead pid on this
// machine means a dead owner. Everything here is best-effort: a database in
// use refuses a plain DROP and survives.
func sweepOrphans(ctx context.Context, admin *pgxpool.Pool) {
	rows, err := admin.Query(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE 'flb_t%' AND datname NOT LIKE 'flb_tpl_%'")
	if err != nil {
		return
	}
	owned := regexp.MustCompile(`^flb_t(\d+)_`)
	var orphans []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) != nil {
			continue
		}
		m := owned.FindStringSubmatch(d)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid == os.Getpid() {
			continue
		}
		// Signal 0 probes liveness; EPERM means alive under another user.
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			orphans = append(orphans, d)
		}
	}
	rows.Close()
	for _, d := range orphans {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", d))
	}
}

func sha256sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
