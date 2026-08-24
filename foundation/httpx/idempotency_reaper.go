package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"
)

// The reaper for the table the Idempotency middleware writes.
//
// It lives beside the middleware on purpose. The rows are written here, they
// carry cached response bodies, and they are useless past their expiry -- so
// the code that creates them owns the code that removes them. The alternative,
// which is what this replaces, is a cleanup that belongs to whichever product
// happened to think of it: the delete existed, but only the hosted build ran
// it, so a self-hosted deployment accumulated cached response bodies with
// nothing to ever remove them. Nothing failed and nothing was logged; the table
// simply grew. A reaper one assembly point away from its writer is a reaper one
// of the builds will not have.
//
// Deleting by expires_at means the retention window is the middleware's TTL and
// is not separately configurable: a key past its TTL can no longer be replayed,
// so keeping the row serves nobody.

// IdempotencyReapArgs drives the job that removes expired idempotency keys.
type IdempotencyReapArgs struct{}

func (IdempotencyReapArgs) Kind() string { return "idempotency_keys_reap" }

// IdempotencyReapWorker deletes idempotency keys whose replay window has closed.
type IdempotencyReapWorker struct {
	river.WorkerDefaults[IdempotencyReapArgs]
	store *idempotencyStore
}

func NewIdempotencyReapWorker(pool *pgxpool.Pool) *IdempotencyReapWorker {
	return &IdempotencyReapWorker{store: newIdempotencyStore(pool)}
}

func (w *IdempotencyReapWorker) Work(ctx context.Context, _ *river.Job[IdempotencyReapArgs]) error {
	deleted, err := w.store.deleteExpired(ctx)
	if err != nil {
		return fmt.Errorf("httpx: delete expired idempotency keys: %w", err)
	}
	// Logged rather than silent: the number is how an operator confirms the job
	// is doing something, and a run that deletes nothing for weeks while the
	// table grows is the signal that the expiry itself has stopped being set.
	slog.InfoContext(ctx, "expired idempotency keys removed", "deleted", deleted)
	return nil
}

// idempotencyReapInterval is how often expired keys are swept. The rows expire
// on a 24-hour TTL, so an hourly sweep keeps the table within one hour of its
// steady-state size while staying far cheaper than the writes it cleans up
// after.
const idempotencyReapInterval = time.Hour

// IdempotencyReapPeriodicJob builds the periodic sweep, registered at the
// assembly point. It runs on start so a deployment that has been down does not
// wait an hour before catching up.
func IdempotencyReapPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		idempotencyReapInterval,
		func() (river.JobArgs, *river.InsertOpts) { return IdempotencyReapArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
