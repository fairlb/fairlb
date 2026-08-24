// Package settle is how the gateway asks for money to move.
//
// # Why the interface lives here and the implementation elsewhere
//
// Balances, ledgers, reservations and reconciliation are a whole accounting
// subsystem, and not every deployment has one. So the gateway does not depend
// on a concrete implementation; it declares "I need someone to do these four
// things for me" and the assembly point decides who that someone is. In this
// repository it is internal/community/settle.
//
// # Why no generated database types cross this boundary
//
// The richer implementations return database row types from Hold and a result
// struct from SettleTx -- and every call site here writes `_, err :=`. The
// gateway has never consumed those return values. So these methods return only
// error, and take local input structs. That is not fastidiousness: letting a
// generated database type cross the seam welds one implementation's data model
// into everything on this side of it, and prying it out later means changing
// signatures, call sites and tests.
//
// # Transaction semantics are not weakened by the indirection
//
// SettleTx passes the pgx.Tx straight through. Every implementation runs
// against the same PostgreSQL database, so handing a transaction across the
// interface costs nothing -- the gateway can still orchestrate "write the usage
// row and settle" inside one transaction, succeeding or failing as a unit.
// Controls internal to an implementation, such as a timeout ceiling inside the
// transaction, stay in that implementation rather than in the interface.
package settle

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// HoldInput is a request to reserve funds.
//
// TTL crosses the interface because only the caller knows how long this kind of
// request runs -- image generation takes far longer than a chat completion, and
// the reservation has to outlive it. The accounting side only knows that a
// reservation left unsettled past its deadline is an orphan for the sweeper to
// void and release. A zero value lets the implementation pick its own default.
type HoldInput struct {
	OrgID      pgtype.UUID
	RequestID  string
	AmountNano int64
	TTL        time.Duration
}

// SettleInput is a request to settle. ActualNano is what this request actually
// cost; APIKeyID is carried so per-key spend can be accumulated.
type SettleInput struct {
	OrgID      pgtype.UUID
	RequestID  string
	ActualNano int64
	APIKeyID   pgtype.UUID
}

// Settler is everything the gateway needs from the accounting side.
//
// The implementation in this repository is internal/community/settle: Hold and
// Void do nothing at all, and SettleTx only accumulates per-key daily spend.
// Reserving funds has no meaning without a balance to reserve them from, so the
// concept is absent rather than always-approving.
//
// Spend limits are still enforced, but not here: the per-key budget check is a
// gate on the request path, reading the same per-key daily spend that SettleTx
// writes. Which is why an implementation whose Hold returns nil is not a hole
// in the enforcement — the enforcement was never in Hold.
type Settler interface {
	// Hold reserves funds. Business refusals such as an insufficient balance
	// are returned by the implementation as an error carrying an error code.
	// Hold reserves funds and returns the reservation's id, or a zero id where
	// the concept does not exist. The id is what lets a usage row and the
	// accounting entries that funded it be looked up from each other; without
	// it that cross-reference is a convention rather than a stored fact, and a
	// convention leaves nothing behind when it is not honoured.
	//
	// A bare UUID, deliberately: the richer implementations have a whole row
	// here, and letting that row cross would weld one implementation's data
	// model into this side. The id is not a generated type, and this seam
	// already carries UUIDs in both input structs.
	Hold(ctx context.Context, in HoldInput) (pgtype.UUID, error)

	// Void releases a reservation, called when the request failed. It is
	// idempotent: voiding the same requestID twice must not be an error.
	Void(ctx context.Context, orgID pgtype.UUID, requestID string) error

	// SettleTx settles inside the caller's transaction. The transaction belongs
	// to the caller; the implementation must not commit or roll it back.
	SettleTx(ctx context.Context, tx pgx.Tx, in SettleInput) error

	// ProtectHold stops the timeout sweeper from reclaiming a reservation. It
	// covers the window where the service has already been delivered but the
	// charge cannot yet be computed. It returns false when the reservation is
	// already gone, which tells the caller whether recording a follow-up item
	// is still meaningful.
	//
	// This method exists because the operation used to be raw SQL in the
	// gateway that reached straight into the accounting tables. No import-based
	// guard can see that: dependency linters and `go list -deps` only know
	// about Go imports, not table names inside SQL. And where those tables do
	// not exist, the UPDATE touches zero rows and the whole path silently does
	// nothing. Cross-layer dependencies do not only hide in imports; they hide
	// in table names too.
	ProtectHold(ctx context.Context, orgID pgtype.UUID, requestID string) (bool, error)
}
