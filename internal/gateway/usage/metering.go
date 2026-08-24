package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/jobs"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	publicusage "github.com/fairlb/fairlb/usage"
)

// latencyBounds are the cumulative histogram's bucket upper bounds in
// milliseconds. They must correspond one-to-one with the rollup's lat_le_*
// columns.
//
// A histogram is stored rather than a p50/p95 per bucket, because percentiles
// cannot be averaged across buckets: averaging hourly p95s does not give the
// week's p95, it gives a number with no statistical meaning. Summing histogram
// columns and computing the quantile afterwards is correct everywhere.
// The top bucket is 120s so long completions and upstream stalls remain visible
// without inventing precision for the +Inf bucket.
var latencyBounds = []int64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000}

// latencyFields are the sum-field names for each bucket (le_<bound>, with
// Prometheus's cumulative semantics).
// MarginSource implements the margin data source that periodic reporting reads.
// The consumer defines the interface, the gateway provides the implementation,
// and the assembly point injects it -- so reporting never queries these tables
// directly. Same inversion as the metering source above.
type MarginSource struct{ q *gwdb.Queries }

func NewMarginSource(q *gwdb.Queries) *MarginSource { return &MarginSource{q: q} }

// CurrencyMargin is one aggregated row, declared here because this is where it
// is produced.
//
// The consumer used to keep a field-for-field twin and the assembly point mapped
// between them, to stop the gateway's dependency closure reaching into the
// consumer. That closure is still intact — nothing here knows what a Cloud
// billing report is — but it never required *two* structs: the consumer now
// aliases this one through the public `gateway` façade, which is the direction
// cross-module integration is supposed to run (ADR-0190).
type CurrencyMargin struct {
	Currency        string
	UpstreamUSDNano int64
	ChargedNano     int64
}

// MarginByCurrency returns upstream cost and charged amount for a period,
// aggregated by currency, for periodic reporting.
//
// It has to be separate from the per-org Margin below: the report is a
// deployment-wide, per-currency figure, and summing per-org results instead
// would hand the reporting side org-level data it has no business seeing.
func (m *MarginSource) MarginByCurrency(ctx context.Context, from, to time.Time) ([]CurrencyMargin, error) {
	rows, err := m.q.MarginByCurrency(ctx, gwdb.MarginByCurrencyParams{
		FromTs: pgtype.Timestamptz{Time: from, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read margin by currency: %w", err)
	}
	out := make([]CurrencyMargin, 0, len(rows))
	for _, r := range rows {
		out = append(out, CurrencyMargin{
			Currency: r.Currency, UpstreamUSDNano: r.UpstreamUsdNano, ChargedNano: r.ChargedNano,
		})
	}
	return out, nil
}

// ===== The aggregation job =====

// AggregateArgs is the usage-to-rollup aggregation job.
type AggregateArgs struct{}

func (AggregateArgs) Kind() string { return "gateway_usage_aggregate" }

// AggregateInterval matches the accounting side's collection interval; both run
// off the same watermark mechanism.
const AggregateInterval = 15 * time.Minute

const (
	aggregationGraceMinutes = 5
	aggregationBatchWindow  = 6 * time.Hour
	aggregationWatermarkKey = "metering:gateway_usage"
)

// Aggregator performs one bounded, set-based usage aggregation transaction.
// It owns concrete gateway semantics rather than exposing generic source/sink
// interfaces for the only rollup implementation in the product.
type Aggregator struct {
	pool    *pgxpool.Pool
	posting *publicusage.PostingStore
	gw      *gwdb.Queries
}

func NewAggregator(pool *pgxpool.Pool, posting *publicusage.PostingStore, gw *gwdb.Queries) *Aggregator {
	return &Aggregator{pool: pool, posting: posting, gw: gw}
}

// Run aggregates at most six hours beginning at the first pending event. Empty
// gaps are skipped in one step, while the watermark and rollups remain atomic.
func (a *Aggregator) Run(ctx context.Context) (int64, error) {
	var applied int64
	err := db.WithSystemTx(ctx, a.pool, func(tx pgx.Tx) error {
		posting := a.posting.WithTx(tx)
		gateway := a.gw.WithTx(tx)
		if err := posting.Ensure(ctx, aggregationWatermarkKey); err != nil {
			return fmt.Errorf("usage: initialize aggregation watermark: %w", err)
		}
		old, err := posting.GetForUpdate(ctx, aggregationWatermarkKey)
		if err != nil {
			return fmt.Errorf("usage: lock aggregation watermark: %w", err)
		}
		cursor, err := posting.AggregationCursor(ctx, aggregationGraceMinutes)
		if err != nil {
			return fmt.Errorf("usage: read aggregation cursor: %w", err)
		}
		if !cursor.Time.After(old.Time) {
			return nil
		}

		first, err := gateway.FirstUsageForAggregation(ctx, gwdb.FirstUsageForAggregationParams{
			After: old,
			Until: cursor,
		})
		if err != nil {
			return fmt.Errorf("usage: find first pending event: %w", err)
		}
		until := cursor.Time
		if first.Valid {
			batchStart := first.Time.UTC().Truncate(time.Hour)
			if batchStart.Before(old.Time) {
				batchStart = old.Time
			}
			if bounded := batchStart.Add(aggregationBatchWindow); bounded.Before(until) {
				until = bounded
			}
			applied, err = gateway.AggregateUsageRollups(ctx, gwdb.AggregateUsageRollupsParams{
				After: old,
				Until: pgtype.Timestamptz{Time: until, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("usage: aggregate rollups: %w", err)
			}
		}
		if err := posting.Set(ctx, aggregationWatermarkKey,
			pgtype.Timestamptz{Time: until, Valid: true}); err != nil {
			return fmt.Errorf("usage: advance aggregation watermark: %w", err)
		}
		return nil
	})
	return applied, err
}

// AggregateWorker drives one bounded aggregation pass.
type AggregateWorker struct {
	river.WorkerDefaults[AggregateArgs]
	agg *Aggregator
}

func NewAggregateWorker(agg *Aggregator) *AggregateWorker {
	return &AggregateWorker{agg: agg}
}

func (w *AggregateWorker) Work(ctx context.Context, _ *river.Job[AggregateArgs]) error {
	n, err := w.agg.Run(ctx)
	if err != nil {
		return fmt.Errorf("gateway: aggregate usage: %w", err)
	}
	if n > 0 {
		slog.InfoContext(ctx, "usage aggregated", "buckets", n)
	}
	return nil
}

// AggregatePeriodicJob builds the periodic aggregation job.
func AggregatePeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		AggregateInterval,
		func() (river.JobArgs, *river.InsertOpts) { return AggregateArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}

// LatencyBoundsSeconds renders the same bucket bounds in seconds, which is what
// OpenTelemetry histograms take.
//
// It exists so the operational histogram and the stored rollup cannot drift
// apart. Two hand-written bucket lists would agree on the day they were written
// and then diverge the first time one of them is tuned -- and the symptom of
// that divergence is the worst kind: both sources answer "what is p95", the
// numbers differ, and nothing says which one moved. One definition, two
// renderings.
func LatencyBoundsSeconds() []float64 {
	out := make([]float64, len(latencyBounds))
	for i, ms := range latencyBounds {
		out[i] = float64(ms) / 1000
	}
	return out
}
