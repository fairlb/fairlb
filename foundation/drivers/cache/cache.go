// Package cache is the cache driver for keys, orgs and the model catalog.
//
// All shared state on the data plane is reached through the driver interfaces,
// so switching to a shared store requires no changes in the business code.
package cache

import (
	"context"
	"time"
)

// Store is the cache driver interface; implementations must be safe for
// concurrent use.
type Store interface {
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)
	// Set stores a value with a TTL; a ttl of zero or less means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes this instance's copy and broadcasts the invalidation to
	// the others.
	Delete(ctx context.Context, key string) error
}
