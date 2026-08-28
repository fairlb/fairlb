package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/drivers/breaker"
)

// The backoff ladder has to climb one rung at a time from the first, with a
// failed half-open probe moving up a rung. Two separate mistakes have broken
// that:
//   - indexing the rung by the failure count, which at the moment the circuit
//     opens equals the threshold, sending the very first open straight to the
//     last rung;
//   - giving the open state a TTL exactly equal to the cooldown, so the state
//     vanished the instant the cooldown expired, the rung reset, and the ladder
//     was stuck on its first rung forever.
//
// These tests guard both.

func providerID(b byte) pgtype.UUID {
	var u pgtype.UUID
	u.Bytes[0], u.Valid = b, true
	return u
}

// tripProvider drives a provider's circuit open by reaching the consecutive
// failure threshold.
func tripProvider(t *testing.T, b *Breaker, id pgtype.UUID) time.Duration {
	t.Helper()
	ctx := context.Background()
	for range providerFailureThreshold {
		b.RecordProviderFailure(ctx, id, "injected")
	}
	st, ok, err := b.store.Get(ctx, providerScope(id))
	if err != nil || !ok || st.Status != breaker.StatusOpen {
		t.Fatalf("the circuit did not open: ok=%v status=%q err=%v", ok, st.Status, err)
	}
	return time.Until(st.Until)
}

// inRung reports whether a cooldown falls inside a rung's jitter band, plus or
// minus 20% with a little slack for timing.
func inRung(d, rung time.Duration) bool {
	lo := time.Duration(float64(rung) * (1 - jitterFraction - 0.05))
	hi := time.Duration(float64(rung) * (1 + jitterFraction + 0.05))
	return d >= lo && d <= hi
}

func TestProviderBackoffStartsAtFirstRung(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	got := tripProvider(t, b, providerID(1))
	if !inRung(got, backoffLadder[0]) {
		t.Errorf("first cooldown = %s, should fall inside the jitter band of the first rung %s", got.Round(time.Second), backoffLadder[0])
	}
}

func TestProviderBackoffEscalatesAcrossOpens(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	id := providerID(2)
	ctx := context.Background()

	// The first open goes through the threshold path; after that each failed
	// half-open probe opens it again.
	got := tripProvider(t, b, id)
	if !inRung(got, backoffLadder[0]) {
		t.Fatalf("the first rung does not match: %s", got)
	}
	for i := 1; i < len(backoffLadder); i++ {
		b.RecordProviderFailure(ctx, id, "half-open probe failed")
		st, _, _ := b.store.Get(ctx, providerScope(id))
		got := time.Until(st.Until)
		if !inRung(got, backoffLadder[i]) {
			t.Fatalf("cooldown %d = %s, should fall inside the jitter band of rung %d, %s",
				i+1, got.Round(time.Second), i+1, backoffLadder[i])
		}
	}
	// Capped: further failures stay on the last rung.
	b.RecordProviderFailure(ctx, id, "still failing")
	st, _, _ := b.store.Get(ctx, providerScope(id))
	if got := time.Until(st.Until); !inRung(got, backoffLadder[len(backoffLadder)-1]) {
		t.Errorf("past the end of the ladder it did not cap on the last rung: %s", got.Round(time.Second))
	}
}

// The open state must outlive the cooldown itself, or the rung expires along
// with the state.
func TestBreakerStateOutlivesCooldown(t *testing.T) {
	mem := breaker.NewMemory()
	b := NewBreaker(mem, nil)
	id := providerID(3)
	cooldown := tripProvider(t, b, id)

	st, ok, _ := mem.Get(context.Background(), providerScope(id))
	if !ok {
		t.Fatal("there is no open state")
	}
	if st.Opens != 1 {
		t.Errorf("Opens = %d, should be 1 after the first open", st.Opens)
	}
	if st.Failures != 0 {
		t.Errorf("Failures = %d, should reset to zero after opening, or the first failure after recovery crosses the threshold again", st.Failures)
	}
	_ = cooldown
}

// A successful probe restores fully: the rung resets and the next fault starts
// from the first rung again.
func TestBreakerResetClearsLadder(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	id := providerID(4)
	ctx := context.Background()

	tripProvider(t, b, id)
	b.RecordSuccess(ctx, id, pgtype.UUID{})

	got := tripProvider(t, b, id)
	if !inRung(got, backoffLadder[0]) {
		t.Errorf("cooldown after recovering and reopening = %s, should be back on the first rung %s", got.Round(time.Second), backoffLadder[0])
	}
}

// Credential-level backoff climbs with the number of opens too. The long
// cooldown for a 401 or 403 takes a different branch and is unaffected.
func TestKeyBackoffEscalates(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	id := providerID(5)
	ctx := context.Background()

	for i := range 3 {
		b.RecordKeyFailure(ctx, id, 0, false)
		st, _, _ := b.store.Get(ctx, providerKeyScope(id))
		if got := time.Until(st.Until); !inRung(got, backoffLadder[i]) {
			t.Fatalf("credential open %d = %s, should fall on rung %d, %s",
				i+1, got.Round(time.Second), i+1, backoffLadder[i])
		}
	}
}

// A 401 or 403 earns the long cooldown on a single occurrence and does not
// take part in the ladder.
func TestKeyAuthFailureUsesLongCooldown(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	id := providerID(6)
	b.RecordKeyFailure(context.Background(), id, 0, true)

	st, _, _ := b.store.Get(context.Background(), providerKeyScope(id))
	if got := time.Until(st.Until); got < keyAuthCooldown-time.Minute {
		t.Errorf("rejected-credential cooldown = %s, should be %s", got.Round(time.Second), keyAuthCooldown)
	}
}

// Breaker records happen on the request path from many goroutines at once. A
// jitter source built on a *Rand, which is documented as not safe for
// concurrent use, would race. This test guards that under the race detector.
func TestBreakerConcurrentRecording(t *testing.T) {
	b := NewBreaker(breaker.NewMemory(), nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			id := providerID(byte(i))
			for range 20 {
				b.RecordKeyFailure(ctx, id, 0, false)
				b.RecordProviderFailure(ctx, id, "concurrent injection")
			}
		})
	}
	wg.Wait()
}
