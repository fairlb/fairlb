package lock

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres implements the lock with a database advisory lock. The lock lives as
// long as the session, so a crashed process releases it automatically and the
// ttl parameter is ignored.
type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (l *Postgres) TryAcquire(ctx context.Context, name string, _ time.Duration) (func(context.Context) error, bool, error) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	key := lockKey(name)

	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}

	var once sync.Once
	release := func(callerCtx context.Context) error {
		var relErr error
		// Idempotent: a repeated call does nothing, so a second unlock cannot
		// disturb someone else's lock count.
		once.Do(func() {
			// Unlocking must not be affected by the caller's cancellation. If
			// it fails, destroy the session — which releases the lock with it
			// — and never return a still-holding session to the pool. A
			// returned holder would strand the lock forever, and worse, a
			// later attempt on the same name that landed on that pooled
			// session would succeed spuriously by re-entering its own lock.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil {
				_ = conn.Hijack().Close(ctx)
				relErr = fmt.Errorf("lock: release failed; the holding session was destroyed: %w", err)
				return
			}
			if !unlocked {
				// The session did not hold this lock. That should not happen, but
				// the connection is safe to return; leave a trace.
				slog.Warn("lock: pg_advisory_unlock returned false (the session did not hold it)", "key", key)
			}
			conn.Release()
		})
		return relErr
	}
	return release, true, nil
}

// lockKey hashes a lock name into the int64 key an advisory lock takes.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) //nolint:gosec // deliberate bit reinterpretation; an advisory lock key only has to be stable
}

var _ Locker = (*Postgres)(nil)
