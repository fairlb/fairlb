package cache

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyChannel is the LISTEN/NOTIFY channel invalidations are broadcast on;
// the payload is the deleted key.
const notifyChannel = "flb_cache_invalidate"

// Memory is the in-process LRU implementation; invalidations are broadcast
// across instances over LISTEN/NOTIFY.
type Memory struct {
	lru  *lru.Cache[string, memEntry]
	pool *pgxpool.Pool
}

type memEntry struct {
	value     []byte
	expiresAt time.Time // zero value means no expiry
}

// NewMemory creates an in-process cache holding at most size entries.
func NewMemory(pool *pgxpool.Pool, size int) (*Memory, error) {
	c, err := lru.New[string, memEntry](size)
	if err != nil {
		return nil, err
	}
	return &Memory{lru: c, pool: pool}, nil
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	e, ok := m.lru.Get(key)
	if !ok {
		return nil, false, nil
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		// Do not remove here. Reading an expired entry and a concurrent Set
		// storing a fresh one are not atomic with respect to each other, so
		// deleting would take the new value with it. An expired entry is simply
		// treated as a miss and is later overwritten by a Set or evicted by the
		// LRU.
		return nil, false, nil
	}
	// Defensive copy. If the in-process cache shares the backing array with the
	// caller, one byte written by that caller poisons every later reader — and
	// this cache holds entries that cross org boundaries. The shared-store
	// implementation copies by nature, and the two must behave the same.
	return bytes.Clone(e.value), true, nil
}

func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	e := memEntry{value: bytes.Clone(value)}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl)
	}
	m.lru.Add(key, e)
	return nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	m.lru.Remove(key)
	_, err := m.pool.Exec(ctx, "SELECT pg_notify($1, $2)", notifyChannel, key)
	return err
}

// Listen blocks, receiving invalidation broadcasts and dropping the local copy,
// until ctx is cancelled; it reconnects on its own if the connection drops. The
// assembly layer runs it as a goroutine.
func (m *Memory) Listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := m.listenOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("cache: invalidation listener interrupted, reconnecting", "error", err)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (m *Memory) listenOnce(ctx context.Context) error {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		m.lru.Remove(n.Payload)
	}
}

var _ Store = (*Memory)(nil)
