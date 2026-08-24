// Package settle implements the money operations for a self-hosted deployment.
//
// # With no prepaid balance, what is left of "settlement"
//
// A prepaid-account model reserves funds, settles against the real amount,
// refunds or charges the difference, writes a ledger and reconciles it
// periodically. Self-hosting is a different arrangement: the operator uses
// their own upstream credentials and pays their own upstream bill, nothing is
// collected on anyone's behalf, and a balance or ledger has nothing to
// represent.
//
// But a spending limit is not billing. "This key may spend at most $10 today"
// is an operational guardrail, and self-hosting needs it just as much -- a
// runaway script burning money on your own upstream bill hurts more, not less.
// So per-key daily spend is still accumulated here, which keeps the spend-limit
// enforcement on the request path working. See docs/design/key-budgets.md.
//
// How the four methods degrade:
//   - Hold        -> no-op: there is no balance to reserve against. Note this
//     is not "always approve"; see below.
//   - Void        -> no-op: there is no reservation to release.
//   - SettleTx    -> accumulates per-key spend (daily limit and total) only,
//     writing no ledger.
//   - ProtectHold -> always (false, nil): there is no reservation to protect,
//     so protection "fails" and the caller skips the follow-up queue. That
//     queue exists for manually establishing a price after the fact, and there
//     is no such workflow here.
//
// # Why Hold does not check a balance, rather than "always approving"
//
// The two behave identically today but mean different things. "Always approve"
// says a check was performed and passed; this says the concept does not exist.
// Writing the former would let whoever adds a balance concept later believe
// this path already gates on it.
// The real enforcement is on the request path -- requests per minute, tokens
// per minute, spend limit -- reading configuration from the key itself, with no
// relation to any balance. The spending guardrail here does not depend on this
// package.
package settle

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/gateway"
)

// Recorder is this package's implementation of the money operations.
// Named for what it does rather than for the port it satisfies: `settle.Settler`
// is the gateway's interface, and a struct with that same name in a second
// package made `settle.Settler` mean two different things.
type Recorder struct{ keys *apikeys.Store }

func New(pool *pgxpool.Pool) Recorder { return Recorder{keys: apikeys.NewStore(pool)} }

// Hold has no reservation concept here; see the package comment. The zero id it
// returns is the honest answer rather than a placeholder: the usage row's
// hold column stays NULL, which reads as "nothing reserved this" -- true here,
// and distinguishable from a reservation whose id went missing.
func (Recorder) Hold(context.Context, gateway.HoldInput) (pgtype.UUID, error) {
	return pgtype.UUID{}, nil
}

// Void has no reservation to release; an idempotent no-op.
func (Recorder) Void(context.Context, pgtype.UUID, string) error { return nil }

// SettleTx accumulates per-key spend only -- the daily limit and the running
// total -- inside the caller's transaction, so it succeeds or fails together
// with the usage row the gateway writes.
func (s Recorder) SettleTx(ctx context.Context, tx pgx.Tx, in gateway.SettleInput) error {
	if !in.APIKeyID.Valid || in.ActualNano <= 0 {
		return nil
	}
	day := pgtype.Date{Time: time.Now().UTC().Truncate(24 * time.Hour), Valid: true}
	return s.keys.WithTx(tx).AddSpend(ctx, in.APIKeyID, day, in.ActualNano)
}

// ProtectHold always reports the reservation as gone, because there is none to
// protect.
//
// It returns false rather than true because the caller uses the answer to
// decide whether to record a follow-up item. That queue exists for "establish
// the price by hand, then settle explicitly", a workflow that does not exist
// here, so recording into it would only accumulate rows nobody will ever
// process.
func (Recorder) ProtectHold(context.Context, pgtype.UUID, string) (bool, error) {
	return false, nil
}

var _ gateway.Settlement = Recorder{}
