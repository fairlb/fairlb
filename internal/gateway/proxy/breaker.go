package proxy

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Two-level circuit breaking, one level for a provider credential and one for a
// provider. The decisions and the counters live entirely in memory, in the
// breaker store; the cooldowns table exists only so state survives a restart,
// which is what keeps the hot path off the database. See
// docs/design/failover-and-cooldowns.md.

// The starting backoff ladder.
var backoffLadder = []time.Duration{
	30 * time.Second, time.Minute, 4 * time.Minute, 10 * time.Minute, 30 * time.Minute,
}

const (
	// providerFailureThreshold: this many consecutive failures opens the
	// provider's circuit.
	providerFailureThreshold = 5
	// jitterFraction: plus or minus 20% on the backoff, so that several
	// instances do not all probe at once and knock the upstream back down.
	jitterFraction = 0.2
	// keyAuthCooldown: one 401 or 403 earns a long cooldown outright. A bad
	// credential does not heal itself, and a short cooldown only means an
	// invalid key gets tried again and again.
	keyAuthCooldown = 30 * time.Minute
	// ladderMemory is how long the ladder position is remembered. An open
	// circuit's TTL is the cooldown plus this window rather than exactly the
	// cooldown: the latter would erase the state at the very instant the
	// cooldown expires, taking the ladder position with it, so the ladder would
	// be stuck on its first rung forever and a repeat failure could never move
	// up a rung.
	ladderMemory = 10 * time.Minute
)

// Breaker is the two-level circuit breaker.
type Breaker struct {
	store breaker.Store
	gw    *gwdb.Queries
	// jitter produces a factor in [0,1). It is a function rather than a
	// *rand.Rand field because the latter is documented as not safe for
	// concurrent use, while breaker records happen on the request path from
	// many goroutines at once. The package-level rand.Float64 is concurrency
	// safe. It is a field only so tests can inject a deterministic sequence.
	jitter func() float64
	ladder []time.Duration
}

func NewBreaker(store breaker.Store, gw *gwdb.Queries) *Breaker {
	return &Breaker{
		store: store, gw: gw,
		//nolint:gosec // jitter only; cryptographic randomness is not needed
		jitter: rand.Float64,
		ladder: backoffLadder,
	}
}

func providerKeyScope(id pgtype.UUID) string { return "gw:brk:pk:" + uuidStr(id) }
func providerScope(id pgtype.UUID) string    { return "gw:brk:p:" + uuidStr(id) }

// ProviderBreakerScope exports how the provider-level key is built, so the
// health dashboard can read the state. The dashboard only reads a snapshot and
// takes no part in the decision, so it goes to the store directly rather than
// through a Breaker.
func ProviderBreakerScope(id pgtype.UUID) string { return providerScope(id) }

// Available reports whether this provider or credential can be used right now:
// open and still inside its cooldown means no. Half-open lets a request
// through, so that once the cooldown expires a single real request probes.
func (b *Breaker) Available(ctx context.Context, scope string) bool {
	st, ok, err := b.store.Get(ctx, scope)
	if err != nil {
		// If the state cannot be read, let the request through: availability
		// wins, and if something really is wrong the next failure starts
		// counting again.
		slog.WarnContext(ctx, "dataplane: reading breaker state failed; letting the request through", "scope", scope, "error", err)
		return true
	}
	if !ok || st.Status == breaker.StatusClosed {
		return true
	}
	if time.Now().After(st.Until) {
		return true // cooldown expired: probe half-open
	}
	return false
}

// ProviderAvailable and KeyAvailable are the convenience forms of the two
// levels.
func (b *Breaker) ProviderAvailable(ctx context.Context, id pgtype.UUID) bool {
	return b.Available(ctx, providerScope(id))
}

func (b *Breaker) KeyAvailable(ctx context.Context, id pgtype.UUID) bool {
	return b.Available(ctx, providerKeyScope(id))
}

// RecordSuccess closes the circuit: a successful half-open probe restores it.
func (b *Breaker) RecordSuccess(ctx context.Context, providerID, keyID pgtype.UUID) {
	b.reset(ctx, providerScope(providerID), "provider", providerID)
	if keyID.Valid {
		b.reset(ctx, providerKeyScope(keyID), "provider_key", keyID)
	}
}

func (b *Breaker) reset(ctx context.Context, scope, dbScope string, id pgtype.UUID) {
	st, ok, err := b.store.Get(ctx, scope)
	if err != nil || !ok || st.Status == breaker.StatusClosed {
		return
	}
	if err := b.store.Delete(ctx, scope); err != nil {
		slog.WarnContext(ctx, "dataplane: clearing breaker state failed", "scope", scope, "error", err)
	}
	// Clear the persisted row too, or a restart would put a recovered provider
	// back into cooldown.
	if b.gw != nil {
		if err := b.gw.DeleteCooldown(ctx, gwdb.DeleteCooldownParams{Scope: dbScope, RefID: id}); err != nil {
			slog.WarnContext(ctx, "dataplane: deleting the persisted cooldown failed", "scope", dbScope, "error", err)
		}
	}
}

// RecordKeyFailure records a credential-level failure. A 401 or 403 earns a
// long cooldown on its own; a 429 takes the greater of the upstream's
// Retry-After and the backoff rung -- if the upstream says wait this long, wait
// at least that long.
func (b *Breaker) RecordKeyFailure(ctx context.Context, keyID pgtype.UUID, hint time.Duration, authFailure bool) {
	if !keyID.Valid {
		return
	}
	scope := providerKeyScope(keyID)
	st, _, _ := b.store.Get(ctx, scope)

	var cooldown time.Duration
	if authFailure {
		cooldown = keyAuthCooldown
	} else {
		cooldown = b.nextBackoff(st.Opens)
		if hint > cooldown {
			cooldown = hint
		}
	}
	b.open(ctx, scope, "provider_key", keyID, st.Opens, cooldown, "credential-level failure")
}

// RecordProviderFailure records a provider-level failure. The circuit opens
// only once consecutive failures reach the threshold; below it the count simply
// accumulates, because one blip should not take a whole provider offline.
func (b *Breaker) RecordProviderFailure(ctx context.Context, providerID pgtype.UUID, reason string) {
	scope := providerScope(providerID)
	st, _, _ := b.store.Get(ctx, scope)
	failures := st.Failures + 1

	if failures < providerFailureThreshold && st.Status != breaker.StatusOpen {
		// Accumulate only, staying closed. The count itself must expire, or
		// occasional failures hours apart would add up to a spurious open --
		// one backoff rung's duration serves as the counting window. Opens is
		// carried through unchanged: it records the history of opening, and
		// the failure count's expiry must not wash it away.
		_ = b.store.Set(ctx, scope, breaker.State{
			Status: breaker.StatusClosed, Failures: failures, Opens: st.Opens,
		}, b.ladder[0])
		return
	}
	// The rung is indexed by Opens, not Failures: the latter is 5 right now
	// (the threshold), and using it as the index would send the very first
	// open straight to the last rung, skipping the whole ladder from 30s up.
	b.open(ctx, scope, "provider", providerID, st.Opens, b.nextBackoff(st.Opens), reason)
}

// open trips the circuit and persists the cooldown deadline for restart
// recovery. opens is how many times the circuit had opened *before* this one;
// it is incremented on the way in, and the next open reads it to move up a
// rung.
func (b *Breaker) open(ctx context.Context, scope, dbScope string, id pgtype.UUID, opens int, cooldown time.Duration, reason string) {
	until := time.Now().Add(cooldown)
	if err := b.store.Set(ctx, scope, breaker.State{
		// Failures resets to zero: the threshold count has been spent on this
		// open, and leaving it would let the first failure after recovery cross
		// the threshold again immediately.
		Status: breaker.StatusOpen, Failures: 0, Opens: opens + 1, Until: until,
	}, cooldown+ladderMemory); err != nil {
		slog.ErrorContext(ctx, "dataplane: writing breaker state failed", "scope", scope, "error", err)
	}
	slog.WarnContext(ctx, "dataplane: circuit opened", "scope", dbScope, "opens", opens+1, "until", until, "reason", reason)

	if b.gw == nil {
		return
	}
	// Write-behind: persistence serves restart recovery only, and a failure
	// here does not affect this decision.
	if err := b.gw.UpsertCooldown(ctx, gwdb.UpsertCooldownParams{
		Scope: dbScope, RefID: id,
		Until:  pgtype.Timestamptz{Time: until, Valid: true},
		Reason: reason,
	}); err != nil {
		slog.WarnContext(ctx, "dataplane: persisting the cooldown failed (only restart recovery is affected)", "error", err)
	}
}

// nextBackoff picks the ladder rung for the number of opens so far and adds
// plus or minus 20% of jitter. The jitter is necessary: instances probing in
// lockstep would all hit the upstream the instant the cooldown expires and take
// down a provider that had just recovered.
func (b *Breaker) nextBackoff(opens int) time.Duration {
	idx := opens
	if idx >= len(b.ladder) {
		idx = len(b.ladder) - 1
	}
	if idx < 0 {
		idx = 0
	}
	base := b.ladder[idx]
	delta := float64(base) * jitterFraction
	offset := (b.jitter()*2 - 1) * delta
	return time.Duration(float64(base) + offset)
}

// RestoreCooldowns reads unexpired cooldowns back from the database at start-up.
// Without it a restart would make every open provider instantly available again
// and put the full load back onto a failing upstream.
func (b *Breaker) RestoreCooldowns(ctx context.Context) error {
	if b.gw == nil {
		return nil
	}
	rows, err := b.gw.ListActiveCooldowns(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		scope := providerScope(r.RefID)
		if r.Scope == "provider_key" {
			scope = providerKeyScope(r.RefID)
		}
		ttl := time.Until(r.Until.Time)
		if ttl <= 0 {
			continue
		}
		// Opens is not persisted, so after a restart the ladder starts from the
		// bottom. That is a deliberate trade: the cooldowns table exists only
		// to stop a restart from reviving every failing provider at once, the
		// ladder history is in-memory state, and giving it a column would
		// amplify hot-path writes down into storage. Restarts are rare enough
		// for the cost to be acceptable.
		if err := b.store.Set(ctx, scope, breaker.State{
			Status: breaker.StatusOpen, Until: r.Until.Time,
		}, ttl); err != nil {
			slog.WarnContext(ctx, "dataplane: restoring breaker state failed", "scope", scope, "error", err)
		}
	}
	slog.InfoContext(ctx, "dataplane: breaker state restored from the database", "count", len(rows))
	return nil
}
