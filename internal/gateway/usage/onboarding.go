package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// The gateway's contribution to the onboarding checklist.
//
// "Made a first request" is a gateway concept -- the checklist framework knows
// about keys, accounts and members, not about requests. So the criterion for
// this step is supplied here, while where it sits in the list and what it is
// called there is wired up at the assembly point. The framework's step type and
// ordering constants are its own orchestration knowledge and are deliberately
// not imported here.

// StepFirstRequest is this step's stable identifier. It lives here because it
// evolves with the criterion; the framework treats it as an opaque key.
const StepFirstRequest = "first_request"

// FirstRequestDone reports whether this org has ever completed a request
// successfully.
//
// The criterion is "succeeded at least once", not "made a request": a pile of
// 401s does not mean anything is working, and working is exactly what the
// checklist is asking about.
//
// It runs inside the caller's transaction: the checklist is one snapshot, and
// every step has to read the same instant.
func FirstRequestDone(ctx context.Context, q *gwdb.Queries, tx pgx.Tx, orgID pgtype.UUID) (bool, error) {
	return q.WithTx(tx).HasSuccessfulRequest(ctx, orgID)
}
