// Package drivers assembles the four pluggable drivers. Each is an interface
// with two implementations — one in-process, one shared — selected by the
// DRIVER_* settings.
package drivers

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/fairlb/fairlb/foundation/config"
	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/drivers/lock"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
)

// memoryCacheSize caps the in-process cache. What it holds (keys, orgs, the
// model catalog) is orders of magnitude smaller than this.
const memoryCacheSize = 65536

// Drivers holds one instance of each driver.
type Drivers struct {
	Cache     cache.Store
	RateLimit ratelimit.Limiter
	Breaker   breaker.Store
	Lock      lock.Locker

	// rdb is the client shared by every driver on the redis setting; nil means
	// all four are in-process. It is retained for exactly two things, Ping and
	// Close.
	rdb *redis.Client
	// stop halts the in-process cache's invalidation listener. That goroutine
	// holds a database connection open for LISTEN/NOTIFY, and without stopping
	// it the pool's Close waits forever for the connection to come back — which
	// shows up as a test run hanging until it times out.
	stop context.CancelFunc
}

// Ping checks the redis connection. With all four drivers in-process it always
// returns nil, because there is no external dependency to check.
//
// Calling it during assembly is the point: constructing a redis client does not
// dial, so a wrong address is not discovered until the first request — by which
// time the process is already behind the proxy taking traffic, and the symptom
// is scattered 500s in production rather than "this deployment failed to start".
// Pinging at assembly moves the failure back into the deploy.
func (d *Drivers) Ping(ctx context.Context) error {
	if d.rdb == nil {
		return nil
	}
	if err := d.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("drivers: redis unreachable: %w", err)
	}
	return nil
}

// Close releases every driver resource; it runs at the end of graceful
// shutdown.
//
// "Every" is literal. Besides the redis pool, it must stop the in-process
// cache's LISTEN goroutine, which holds a database connection: without that,
// the connection pool's own Close waits forever for a connection that is never
// returned. Closing only redis here would leave Close half-implemented in a way
// that looks fine right up until shutdown hangs.
func (d *Drivers) Close() error {
	if d.stop != nil {
		d.stop()
	}
	if d.rdb == nil {
		return nil
	}
	return d.rdb.Close()
}

// New assembles the four drivers from configuration. With the in-process cache,
// the invalidation listener runs for the lifetime of ctx.
func New(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*Drivers, error) {
	var rdb *redis.Client
	redisClient := func() (*redis.Client, error) {
		if rdb != nil {
			return rdb, nil
		}
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("drivers: parse REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(opts)
		return rdb, nil
	}

	d := &Drivers{}

	switch cfg.Drivers.Cache {
	case config.DriverMemory:
		mem, err := cache.NewMemory(pool, memoryCacheSize)
		if err != nil {
			return nil, err
		}
		// Derive a cancellable context for Close to use. The context the
		// caller passes in is often a long-lived one that is never cancelled,
		// and this goroutine has to be stoppable regardless.
		listenCtx, stop := context.WithCancel(ctx)
		d.stop = stop
		go mem.Listen(listenCtx)
		d.Cache = mem
	case config.DriverRedis:
		c, err := redisClient()
		if err != nil {
			return nil, err
		}
		d.Cache = cache.NewRedis(c)
	}

	switch cfg.Drivers.RateLimit {
	case config.DriverMemory:
		d.RateLimit = ratelimit.NewMemory()
	case config.DriverRedis:
		c, err := redisClient()
		if err != nil {
			return nil, err
		}
		d.RateLimit = ratelimit.NewRedis(c)
	}

	switch cfg.Drivers.Breaker {
	case config.DriverMemory:
		d.Breaker = breaker.NewMemory()
	case config.DriverRedis:
		c, err := redisClient()
		if err != nil {
			return nil, err
		}
		d.Breaker = breaker.NewRedis(c)
	}

	switch cfg.Drivers.Lock {
	case config.DriverMemory:
		d.Lock = lock.NewPostgres(pool)
	case config.DriverRedis:
		c, err := redisClient()
		if err != nil {
			return nil, err
		}
		d.Lock = lock.NewRedis(c)
	}

	// If any driver selected redis, check it once here and fail to start when
	// it is unreachable. This is deliberately fail-closed: rate limiting,
	// circuit breaking and locking all depend on it, and starting with a broken
	// connection means the rate limiter does nothing and the distributed lock
	// grants everyone the lock — failures that are entirely silent.
	d.rdb = rdb
	if err := d.Ping(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
