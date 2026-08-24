package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"

	"github.com/fairlb/fairlb/foundation/db"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/settle"
)

// Retrying charges whose settlement failed.
//
// When the "settle plus usage row" transaction fails on the request path, the
// response must not be cut off: the service has already been delivered, so
// cutting off costs the customer something and recovers nothing. But logging it
// is not enough either -- the nightly checks verify the accounting's internal
// consistency, and a request that was never settled leaves both sides perfectly
// consistent and therefore invisible.
//
// So the request path records the failure in gateway_unsettled and this worker
// replays it.

// UnsettledArgs is the retry job.
type UnsettledArgs struct{}

func (UnsettledArgs) Kind() string { return "gateway_unsettled_retry" }

const (
	// unsettledBatch is how many rows one round handles. It is deliberately
	// small: this is a repair path, not a throughput path, and taking too many
	// at once piles load onto the database at the exact moment it is recovering.
	unsettledBatch = 50
	// unsettledMaxAttempts is when to give up and alert. There are situations
	// where retrying can never succeed (the reservation record itself is gone),
	// and retrying forever only buries the failure in log noise.
	unsettledMaxAttempts = 8
	// UnsettledInterval is the retry polling interval.
	UnsettledInterval = 2 * time.Minute
)

// UnsettledWorker replays failed settlements.
type UnsettledWorker struct {
	river.WorkerDefaults[UnsettledArgs]
	pool    *pgxpool.Pool
	q       *gwdb.Queries
	billing settle.Settler
	alerter Alerter
}

func NewUnsettledWorker(pool *pgxpool.Pool, bill settle.Settler, alerter Alerter) *UnsettledWorker {
	return &UnsettledWorker{pool: pool, q: gwdb.New(pool), billing: bill, alerter: alerter}
}

func (w *UnsettledWorker) Work(ctx context.Context, _ *river.Job[UnsettledArgs]) error {
	rows, err := w.q.ListUnsettledPending(ctx, unsettledBatch)
	if err != nil {
		return fmt.Errorf("usage: fetch pending charges: %w", err)
	}
	var resolved, abandoned int
	for _, r := range rows {
		switch w.retry(ctx, r) {
		case outcomeResolved:
			resolved++
		case outcomeAbandoned:
			abandoned++
		}
	}
	if resolved > 0 || abandoned > 0 {
		slog.InfoContext(ctx, "charge retry round complete",
			"pending", len(rows), "resolved", resolved, "abandoned", abandoned)
	}
	return nil
}

type retryOutcome int

const (
	outcomeRetryLater retryOutcome = iota
	outcomeResolved
	outcomeAbandoned
)

// retry replays one charge.
//
// The replay takes exactly the same path as the request path does -- settle and
// insert the usage row in one transaction -- so atomicity between the ledger
// and the usage record is not weakened by going through the retry route.
//
// The reservation can be in either of two states by now, and both converge
// correctly:
//   - still pending: settle normally;
//   - already swept and expired: settle as a late debit. Only an expired
//     reservation may be debited late; a late settlement against one that was
//     explicitly voided is always refused.
func (w *UnsettledWorker) retry(ctx context.Context, r gwdb.ListUnsettledPendingRow) retryOutcome {
	params, err := DecodeUsageReplayPayload(r.Payload)
	if err != nil {
		// A payload that will not decode decodes no better on the tenth
		// attempt; give up immediately.
		w.abandon(ctx, r, "payload decode failed: "+err.Error())
		return outcomeAbandoned
	}
	err = db.WithSystemTx(ctx, w.pool, func(tx pgx.Tx) error {
		if sErr := w.billing.SettleTx(ctx, tx, settle.SettleInput{
			OrgID: r.OrgID, RequestID: r.RequestID,
			ActualNano: r.ChargedNano, APIKeyID: params.ApiKeyID,
		}); sErr != nil {
			return sErr
		}
		return w.q.WithTx(tx).InsertUsageLog(ctx, params)
	})
	if err == nil {
		if mErr := w.q.MarkUnsettledResolved(ctx, r.RequestID); mErr != nil {
			// Settled but not marked: the next round replays it, and
			// settlement is idempotent per request_id (the reservation is
			// already settled), so nobody is charged twice.
			slog.ErrorContext(ctx, "charge settled but marking it resolved failed", "error", mErr, "request_id", r.RequestID)
		}
		return outcomeResolved
	}

	if r.Attempts+1 >= unsettledMaxAttempts {
		w.abandon(ctx, r, err.Error())
		return outcomeAbandoned
	}
	if bErr := w.q.BumpUnsettledAttempt(ctx, gwdb.BumpUnsettledAttemptParams{
		RequestID: r.RequestID, Reason: err.Error(),
	}); bErr != nil {
		slog.ErrorContext(ctx, "updating the retry counter failed", "error", bErr, "request_id", r.RequestID)
	}
	return outcomeRetryLater
}

// abandon gives up on a charge and alerts. This money needs a human; it must
// not sit quietly in a table.
func (w *UnsettledWorker) abandon(ctx context.Context, r gwdb.ListUnsettledPendingRow, reason string) {
	if err := w.q.AbandonUnsettled(ctx, gwdb.AbandonUnsettledParams{
		RequestID: r.RequestID, Reason: reason,
	}); err != nil {
		slog.ErrorContext(ctx, "marking the charge abandoned failed", "error", err, "request_id", r.RequestID)
	}
	slog.ErrorContext(ctx, "charge abandoned, needs a human",
		"request_id", r.RequestID, "org_id", r.OrgID, "charged_nano", r.ChargedNano, "reason", reason)
	if w.alerter == nil {
		return
	}
	w.alerter.Alert(ctx, "A usage charge could not be settled and needs a human",
		fmt.Sprintf("request_id=%s org=%v amount=%d %s, still failing after "+
			"%d retries: %s\nThis service was delivered but never charged "+
			"for. Look it up in the gateway_unsettled table.",
			r.RequestID, r.OrgID, r.ChargedNano, r.Currency, r.Attempts, reason))
}

// UnsettledPeriodicJob builds the periodic retry job, registered at the
// assembly point.
func UnsettledPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		UnsettledInterval,
		func() (river.JobArgs, *river.InsertOpts) { return UnsettledArgs{}, nil },
		// Run a round on start: charges left behind by the previous process
		// crashing should converge as soon as possible.
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
