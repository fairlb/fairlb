package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the shared-store implementation; the contract suite runs against a
// real redis. A shared store is consistent across instances by construction, so
// no invalidation broadcast is needed. Adding a local near-cache in front of it
// would require one.
type Redis struct {
	rdb *redis.Client
}

func NewRedis(rdb *redis.Client) *Redis { return &Redis{rdb: rdb} }

func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0 // the client reads 0 as "no expiry"
	}
	return r.rdb.Set(ctx, key, value, ttl).Err()
}

func (r *Redis) Delete(ctx context.Context, key string) error {
	return r.rdb.Del(ctx, key).Err()
}

var _ Store = (*Redis)(nil)
