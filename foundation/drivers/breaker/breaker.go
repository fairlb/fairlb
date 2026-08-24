// Package breaker is the shared store for circuit-breaker and cooldown state.
//
// The driver only reads and writes state. The decision logic lives with the
// caller, and recovery after a restart is the caller's job too — it reloads
// from its own tables. The driver knows nothing about them.
package breaker

import (
	"context"
	"time"
)

// States of the circuit-breaker state machine; the decisions are made by the
// caller.
const (
	StatusClosed   = "closed"
	StatusOpen     = "open"
	StatusHalfOpen = "half_open"
)

// State is a snapshot of the shared breaker state.
//
// Failures and Opens are two independent counters, and conflating them breaks
// the backoff ladder. Failures is "how many failures have accumulated since the
// last trip" and resets every time the breaker opens; Opens is "how many times
// this scope has tripped" and is the index into the backoff ladder, resetting
// only on recovery.
type State struct {
	Status   string
	Failures int
	Opens    int       // times tripped; the index into the backoff ladder
	Until    time.Time // when the open cooldown ends
}

// Store reads and writes the state; implementations must be safe for concurrent
// use.
type Store interface {
	Get(ctx context.Context, key string) (State, bool, error)
	// Set writes the state; a ttl of zero or less means no expiry.
	Set(ctx context.Context, key string, st State, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}
