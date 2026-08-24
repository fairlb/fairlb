// Package lock is the distributed lock driver: mutual exclusion for scheduled
// jobs and for creating partitions ahead of time.
package lock

import (
	"context"
	"time"
)

// Locker acquires locks without blocking.
//
// ttl is the crash backstop, i.e. how long the lock survives an owner that never
// releases it. The database implementation ignores it, because the lock is tied
// to the session and is released when the connection drops; the shared-store
// implementation depends on it.
type Locker interface {
	// TryAcquire attempts to take the lock; when acquired is true the caller
	// must call release.
	TryAcquire(ctx context.Context, name string, ttl time.Duration) (release func(context.Context) error, acquired bool, err error)
}
