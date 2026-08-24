package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is a single-node redlock: SET NX to acquire, and a checked release. The
// contract suite runs against a real redis, including the rule that only the
// holder may release — a late release from an owner whose lock already expired
// must not unlock the new holder's.
type Redis struct {
	rdb *redis.Client
}

func NewRedis(rdb *redis.Client) *Redis { return &Redis{rdb: rdb} }

// releaseScript deletes the key only when the holder token matches, so a stale
// owner cannot release a lock someone else has since acquired.
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

func (l *Redis) TryAcquire(ctx context.Context, name string, ttl time.Duration) (func(context.Context) error, bool, error) {
	if ttl <= 0 {
		ttl = time.Minute
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, false, err
	}
	val := hex.EncodeToString(token)
	key := "flb:lock:" + name

	ok, err := l.rdb.SetNX(ctx, key, val, ttl).Result()
	if err != nil || !ok {
		return nil, false, err
	}
	release := func(ctx context.Context) error {
		return releaseScript.Run(ctx, l.rdb, []string{key}, val).Err()
	}
	return release, true, nil
}

var _ Locker = (*Redis)(nil)
