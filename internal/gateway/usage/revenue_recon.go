package usage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Reverse reconciliation: service delivered vs revenue booked.
//
// The nightly ledger invariants ask whether the accounting is internally
// consistent -- balances against entries against reservations. That criterion
// has a blind spot: a request that was never settled at all moved no balance
// and wrote no entry, so both sides agree perfectly and nothing is reported.
// This job asks the opposite question: was everything we served charged for?
//
// Each of its checks corresponds to a known way revenue goes missing:
//   1. successful requests charged zero -> an unpriced model, or a missing rate
//      for one token bucket
//   2. a deferred-charge backlog        -> settlement failed and the retry path
//      never converged
//   3. the share of estimated billing   -> upstreams that report no usage force
//      the charge onto an estimate
//   4. terminal asynchronous jobs whose reservation never moved -> the job
//      finished and the settlement transaction never landed, so the customer
//      was either overcharged or not charged at all

// RevenueReconArgs is the reverse reconciliation job.
type RevenueReconArgs struct{}

func (RevenueReconArgs) Kind() string { return "gateway_revenue_recon" }

const (
	// revenueReconWindow is how far back each round looks, matching the
	// nightly accounting checks.
	revenueReconWindow = 24 * time.Hour
	// RevenueReconInterval is how often the job runs.
	RevenueReconInterval = 24 * time.Hour

	// estimatedShareThresholdBps is the alert threshold (15%) for the share of
	// settled revenue that rests on estimated token counts.
	// Token counts are estimated with a character heuristic rather than a real
	// tokenizer, justified by "the reservation is only a ceiling, settlement
	// uses the real numbers" -- but settling on real numbers presupposes the
	// upstream reported usage. The more requests that report none, the more
	// money rests on the estimate and the weaker that justification gets. This
	// number is the trigger for adopting a real tokenizer.
	estimatedShareThresholdBps = 1500
	// unsettledPendingThreshold is the alert threshold on the deferred-charge
	// backlog. It is small on purpose: the retry worker runs every couple of
	// minutes, so in normal operation this table should be close to empty.
	unsettledPendingThreshold = 10
)

// RevenueReconWorker runs the reverse reconciliation.
type RevenueReconWorker struct {
	river.WorkerDefaults[RevenueReconArgs]
	q       *gwdb.Queries
	alerter Alerter
}

func NewRevenueReconWorker(pool *pgxpool.Pool, alerter Alerter) *RevenueReconWorker {
	return &RevenueReconWorker{q: gwdb.New(pool), alerter: alerter}
}

// RevenueReconReport is the outcome of one round; Clean reports whether
// everything passed.
type RevenueReconReport struct {
	ZeroCharged         []gwdb.ReconZeroChargedRow
	PendingUnsettled    int64
	PendingNano         int64
	AbandonedUnsettled  int64
	AbandonedNano       int64
	PricingMissing      int64
	PricingReservedNano int64
	EstimatedNano       int64
	TotalNano           int64
	// StuckJobs counts terminal asynchronous jobs still holding a reservation.
	// StuckOldest is the earliest of their terminal times, which is what
	// separates a live incident from one row stranded a month ago.
	StuckJobs   int64
	StuckOldest *time.Time
}

// EstimatedShareBps returns the estimated-billing share in basis points, or 0
// when there was no usage at all.
func (r RevenueReconReport) EstimatedShareBps() int64 {
	if r.TotalNano <= 0 {
		return 0
	}
	return r.EstimatedNano * 10000 / r.TotalNano
}

// Clean reports whether the round found nothing.
//
// Any abandoned charge at all is a violation -- those have exhausted retries
// and need a human. A small number of pending ones is tolerated, since the
// retry worker runs every couple of minutes and the queue should drain.
func (r RevenueReconReport) Clean() bool {
	return len(r.ZeroCharged) == 0 &&
		r.AbandonedUnsettled == 0 &&
		r.PendingUnsettled < unsettledPendingThreshold &&
		r.PricingMissing == 0 &&
		r.StuckJobs == 0 &&
		r.EstimatedShareBps() < estimatedShareThresholdBps
}

// Run performs one round of reverse reconciliation.
func (w *RevenueReconWorker) Run(ctx context.Context) (RevenueReconReport, error) {
	var rep RevenueReconReport
	since := pgtype.Timestamptz{Time: time.Now().Add(-revenueReconWindow), Valid: true}

	zero, err := w.q.ReconZeroCharged(ctx, since)
	if err != nil {
		return rep, fmt.Errorf("usage: query zero-charged requests: %w", err)
	}
	rep.ZeroCharged = zero

	backlog, err := w.q.ReconUnsettledBacklog(ctx)
	if err != nil {
		return rep, fmt.Errorf("usage: query deferred-charge backlog: %w", err)
	}
	rep.PendingUnsettled, rep.PendingNano = backlog.Pending, backlog.PendingNano
	rep.AbandonedUnsettled, rep.AbandonedNano = backlog.Abandoned, backlog.AbandonedNano

	pricingBacklog, err := w.q.ReconPricingUnsettledBacklog(ctx)
	if err != nil {
		return rep, fmt.Errorf("usage: query missing-price queue: %w", err)
	}
	rep.PricingMissing, rep.PricingReservedNano = pricingBacklog.Pending, pricingBacklog.ReservedNano

	// No window: unlike the other checks this one is not asking "what happened
	// in the last day" but "what is still wrong". A reservation stranded three
	// weeks ago is more urgent than one stranded this morning, not less.
	stuck, err := w.q.CountStuckMoneyJobs(ctx)
	if err != nil {
		return rep, fmt.Errorf("usage: query jobs whose money never moved: %w", err)
	}
	rep.StuckJobs = stuck.Jobs
	if stuck.OldestTerminalAt.Valid {
		oldest := stuck.OldestTerminalAt.Time.UTC()
		rep.StuckOldest = &oldest
	}

	share, err := w.q.ReconEstimatedShare(ctx, since)
	if err != nil {
		return rep, fmt.Errorf("usage: query estimated-billing share: %w", err)
	}
	rep.EstimatedNano, rep.TotalNano = share.EstimatedNano, share.TotalNano
	return rep, nil
}

func (w *RevenueReconWorker) Work(ctx context.Context, _ *river.Job[RevenueReconArgs]) error {
	rep, err := w.Run(ctx)
	if err != nil {
		return err
	}
	if rep.Clean() {
		slog.InfoContext(ctx, "revenue reconciliation clean",
			"estimated_share_bps", rep.EstimatedShareBps(), "unsettled_pending", rep.PendingUnsettled)
		return nil
	}

	detail := describeRevenueRecon(rep)
	slog.ErrorContext(ctx, "revenue reconciliation found signs of uncollected revenue", "detail", detail)
	if w.alerter != nil {
		w.alerter.Alert(ctx, "Revenue reconciliation: service delivered but not charged", detail)
	}
	// As with the nightly accounting checks: this is not a transient fault, so
	// retrying is pointless. The signal travels through the alerting chain.
	return nil
}

// describeRevenueRecon composes the alert body. Every line has to say what the
// finding means and where to look, otherwise whoever receives the alert has to
// read the source to find out what to do.
func describeRevenueRecon(r RevenueReconReport) string {
	var b strings.Builder
	if n := len(r.ZeroCharged); n > 0 {
		var reqs int64
		for _, z := range r.ZeroCharged {
			reqs += z.Requests
		}
		fmt.Fprintf(&b, "- Successful requests charged nothing: %d model/org "+
			"combinations, %d requests in total. Usually a model with no "+
			"pricing configured, or a missing cache-write rate. Check these "+
			"model slugs in the admin console: %s\n", n, reqs, topSlugs(r.ZeroCharged))
	}
	if r.AbandonedUnsettled > 0 {
		fmt.Fprintf(&b, "- Charges abandoned: %d totalling %d nano. These "+
			"services were delivered and cannot be collected; they need a "+
			"human. Look for rows in gateway_unsettled with abandoned_at "+
			"set.\n", r.AbandonedUnsettled, r.AbandonedNano)
	}
	if r.PendingUnsettled >= unsettledPendingThreshold {
		fmt.Fprintf(&b, "- Deferred-charge backlog: %d totalling %d nano. The "+
			"retry worker runs every two minutes, so a backlog means the "+
			"retries themselves keep failing.\n", r.PendingUnsettled, r.PendingNano)
	}
	if r.PricingMissing > 0 {
		fmt.Fprintf(&b, "- Requests awaiting a price for an advanced pricing "+
			"dimension: %d, holding %d nano reserved. These amounts are not "+
			"final bills and must not be charged automatically; once the "+
			"missing rate is established, settle or void each one by hand "+
			"against its snapshot. See the gateway_pricing_unsettled "+
			"table.\n", r.PricingMissing, r.PricingReservedNano)
	}
	if r.StuckJobs > 0 {
		since := ""
		if r.StuckOldest != nil {
			since = fmt.Sprintf(", the oldest waiting since %s", r.StuckOldest.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "- Asynchronous jobs terminal with their reservation "+
			"unmoved: %d%s. Each one is a customer either overcharged or not "+
			"charged at all, and nothing clears these on its own -- a "+
			"`protected` hold is deliberately exempt from the timeout sweep. "+
			"Settle or void each against its snapshot; they are the rows in "+
			"gateway_async_jobs with a terminal status and settlement_state "+
			"still held or protected.\n", r.StuckJobs, since)
	}
	if bps := r.EstimatedShareBps(); bps >= estimatedShareThresholdBps {
		fmt.Fprintf(&b, "- Estimated billing share: %.2f%% (threshold "+
			"%.2f%%). Too many requests come back without usage from the "+
			"upstream, so settlement is leaning heavily on a character "+
			"heuristic. This is the trigger for adopting a real "+
			"tokenizer.\n",
			float64(bps)/100, float64(estimatedShareThresholdBps)/100)
	}
	return strings.TrimRight(b.String(), "\n")
}

// topSlugs takes the first few slugs for the alert body; listing all of them
// would blow the message up.
func topSlugs(rows []gwdb.ReconZeroChargedRow) string {
	const max = 5
	seen := map[string]bool{}
	out := make([]string, 0, max)
	for _, r := range rows {
		if seen[r.ModelSlug] {
			continue
		}
		seen[r.ModelSlug] = true
		out = append(out, r.ModelSlug)
		if len(out) == max {
			out = append(out, "…")
			break
		}
	}
	return strings.Join(out, ", ")
}

// RevenueReconPeriodicJob builds the periodic job, registered at the assembly
// point.
func RevenueReconPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		RevenueReconInterval,
		func() (river.JobArgs, *river.InsertOpts) { return RevenueReconArgs{}, nil },
		// Run a round on every deploy, same policy as the nightly checks.
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
