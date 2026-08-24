package breaker

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis stores the state in a hash, so every instance sees the same value. The
// contract suite runs against a real redis.
type Redis struct {
	rdb *redis.Client
}

func NewRedis(rdb *redis.Client) *Redis { return &Redis{rdb: rdb} }

func rkey(key string) string { return "flb:breaker:" + key }

func (s *Redis) Get(ctx context.Context, key string) (State, bool, error) {
	m, err := s.rdb.HGetAll(ctx, rkey(key)).Result()
	if err != nil {
		return State{}, false, err
	}
	if len(m) == 0 {
		return State{}, false, nil
	}
	st := State{Status: m["status"]}
	st.Failures, _ = strconv.Atoi(m["failures"])
	st.Opens, _ = strconv.Atoi(m["opens"])
	if v := m["until"]; v != "" {
		if ns, err := strconv.ParseInt(v, 10, 64); err == nil {
			st.Until = time.Unix(0, ns)
		}
	}
	return st, true, nil
}

func (s *Redis) Set(ctx context.Context, key string, st State, ttl time.Duration) error {
	k := rkey(key)
	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, k,
		"status", st.Status,
		"failures", strconv.Itoa(st.Failures),
		"opens", strconv.Itoa(st.Opens),
		"until", strconv.FormatInt(st.Until.UnixNano(), 10),
	)
	if ttl > 0 {
		// PExpire (milliseconds) rather than Expire (seconds): redis's EXPIRE
		// only understands whole seconds, so a sub-second TTL is rounded up to
		// one second while the in-process implementation is millisecond
		// accurate. That would fork the expiry semantics of the two drivers.
		// Production TTLs are minutes and would never reveal it, but the whole
		// point of the contract suite is that the two are interchangeable.
		pipe.PExpire(ctx, k, ttl)
	} else {
		pipe.Persist(ctx, k)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Redis) Delete(ctx context.Context, key string) error {
	return s.rdb.Del(ctx, rkey(key)).Err()
}

var _ Store = (*Redis)(nil)
