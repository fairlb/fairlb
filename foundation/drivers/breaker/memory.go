package breaker

import (
	"context"
	"sync"
	"time"
)

// Memory is the in-process implementation.
type Memory struct {
	mu sync.RWMutex
	m  map[string]memState
}

type memState struct {
	st        State
	expiresAt time.Time // zero value means no expiry
}

func NewMemory() *Memory {
	return &Memory{m: make(map[string]memState)}
}

func (s *Memory) Get(_ context.Context, key string) (State, bool, error) {
	s.mu.RLock()
	e, ok := s.m[key]
	s.mu.RUnlock()
	if !ok {
		return State{}, false, nil
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		// Delete only after comparing. Between releasing the read lock and
		// taking the write lock, a Set may have stored fresh state; an
		// unconditional delete would wipe it, so a breaker that just tripped
		// would be swallowed by expiry cleanup and never open at all.
		s.mu.Lock()
		if cur, still := s.m[key]; still && cur == e {
			delete(s.m, key)
		}
		s.mu.Unlock()
		return State{}, false, nil
	}
	return e.st, true, nil
}

func (s *Memory) Set(_ context.Context, key string, st State, ttl time.Duration) error {
	e := memState{st: st}
	if ttl > 0 {
		e.expiresAt = time.Now().Add(ttl)
	}
	s.mu.Lock()
	s.m[key] = e
	s.mu.Unlock()
	return nil
}

func (s *Memory) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return nil
}

var _ Store = (*Memory)(nil)
