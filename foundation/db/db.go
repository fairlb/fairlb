// Package db provides the pgx connection pool and runs the embedded migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// migrateLockKey is the PostgreSQL advisory lock held for the duration of a
// migration run, so that several instances starting at once cannot migrate
// concurrently.
const migrateLockKey = int64(4219020260)

// Connect builds the pool and pings to confirm the database is reachable.
// maxConns caps the pool (0 means the pgx default). The cache invalidation
// listener permanently holds one pooled connection, so capacity planning should
// read the usable size as maxConns - 1; configuration enforces the lower bound.
func Connect(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("db: DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	observePool(pool)
	return pool, nil
}

// Migrate runs, in order and under a single advisory lock, the schema
// migrations followed by the job queue's own schema. It is idempotent and safe
// to run on every start.
//
// The lock is held on a separate direct connection rather than a pooled one:
// the migrators take their connections from the pool, so a pooled lock would
// deadlock against itself when the pool is limited to a single connection.
//
// schemas carry the SQL sets to apply, in product ownership order, and the
// caller picks them: which schemas a deployment gets is decided at the assembly
// point, not by a build flag. River is migrated exactly once after every schema
// succeeds. Goose globs the root of each fsys for `*.sql` and does not recurse.
func Migrate(ctx context.Context, pool *pgxpool.Pool, schemas ...fs.FS) error {
	lockConn, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		return fmt.Errorf("db: connect for migration lock: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = lockConn.Close(closeCtx)
	}()

	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("db: acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	sqldb := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqldb.Close() }()

	// Out-of-order application is allowed on purpose. Migration numbers are
	// banded by layer, and a lower-numbered migration in one band can be written
	// after a higher-numbered one in another band has already been applied. The
	// migrator rejects such gaps by default and refuses to start, which for
	// banded numbering is the normal case rather than an anomaly. Concurrency is
	// unaffected: the whole run is serialized by the advisory lock above.
	for i, fsys := range schemas {
		provider, err := goose.NewProvider(goose.DialectPostgres, sqldb, fsys,
			goose.WithAllowOutofOrder(true))
		if err != nil {
			return fmt.Errorf("db: goose provider %d: %w", i+1, err)
		}
		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("db: goose up %d: %w", i+1, err)
		}
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("db: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{}); err != nil {
		return fmt.Errorf("db: river migrate: %w", err)
	}
	return nil
}
