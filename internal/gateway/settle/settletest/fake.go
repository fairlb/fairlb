// Package settletest provides a fake settle.Settler for the gateway's tests.
//
// # Why the gateway's tests should not drive a real accounting implementation
//
// Faking it is not about speed, it is about testing the layer under test. The
// gateway's responsibility is to invoke the right money operation at the right
// moment: refuse the request when the reservation fails, settle in the same
// transaction when it succeeds, void when it does not. Whether a reservation
// really moves a balance and a settlement really writes a ledger entry is the
// accounting implementation's responsibility, covered by its own tests and by
// end-to-end conservation-of-funds checks.
//
// Before the fake existed, a dozen pipeline test cases each had to top up an
// identical large amount first, just so a real reservation would pass -- and
// that top-up is not the semantics under test, it is noise. Two worker tests
// used "the balance changed" as evidence that the worker had settled, whereas
// asserting directly that SettleTx was called with the right arguments is
// sharper: an unchanged balance could mean the worker found no record, or that
// it settled the wrong amount, and those two have identical symptoms.
//
// There is also a structural reason: not every build has balances and a ledger,
// so a gateway test wired to a full accounting implementation would be dead
// code that does not compile in those builds.
package settletest

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/settle"
)

// Fake records the money operations it receives and lets failures be injected.
// The zero value is usable: everything succeeds and nothing is recorded yet.
type Fake struct {
	mu sync.Mutex

	// HoldErr, when non-nil, makes Hold always fail -- for testing what the
	// request does when its reservation is refused.
	HoldErr error
	// SettleErr, when non-nil, makes SettleTx always fail -- for testing the
	// transaction rollback path.
	SettleErr error

	// ProtectErr, when non-nil, makes ProtectHold always fail; ProtectMissing
	// simulates a reservation that is already gone.
	ProtectErr     error
	ProtectMissing bool

	// HoldID is what Hold hands back; zero unless a test sets it.
	HoldID   pgtype.UUID
	Holds    []settle.HoldInput
	Voids    []string // requestID
	Settles  []settle.SettleInput
	Protects []string // requestID
}

func (f *Fake) Hold(_ context.Context, in settle.HoldInput) (pgtype.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.HoldErr != nil {
		return f.HoldID, f.HoldErr
	}
	f.Holds = append(f.Holds, in)
	return f.HoldID, nil
}

func (f *Fake) Void(_ context.Context, _ pgtype.UUID, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Voids = append(f.Voids, requestID)
	return nil
}

func (f *Fake) SettleTx(_ context.Context, _ pgx.Tx, in settle.SettleInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SettleErr != nil {
		return f.SettleErr
	}
	f.Settles = append(f.Settles, in)
	return nil
}

// ProtectHold records the call. With ProtectMissing set it reports the
// reservation as already gone.
func (f *Fake) ProtectHold(_ context.Context, _ pgtype.UUID, requestID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ProtectErr != nil {
		return false, f.ProtectErr
	}
	f.Protects = append(f.Protects, requestID)
	return !f.ProtectMissing, nil
}

// Protected reports whether the reservation for a requestID was ever protected.
func (f *Fake) Protected(requestID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Protects {
		if r == requestID {
			return true
		}
	}
	return false
}

// LastSettle returns the most recent settlement, or false if there was none.
// It exists for the "one request settles exactly once" cases, where the
// requestID is generated inside the pipeline and the test never sees it.
func (f *Fake) LastSettle() (settle.SettleInput, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Settles) == 0 {
		return settle.SettleInput{}, false
	}
	return f.Settles[len(f.Settles)-1], true
}

// Counts returns how many times each of the three operations ran, as a
// concurrency-safe snapshot.
func (f *Fake) Counts() (holds, voids, settles int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Holds), len(f.Voids), len(f.Settles)
}

var _ settle.Settler = (*Fake)(nil)
