// Package gwhealth assembles the gateway's operator health dashboard.
//
// It is the one place that puts four different things side by side -- the
// request rollup, the latency histogram, the in-memory breaker state and the
// kill-switch counts -- and the reason it is a package rather than a handler is
// that the rules for combining them are not presentation. Which failures may
// take the whole dashboard down and which may only blank one column is an
// operational decision: during an incident the breaker state and the error rate
// matter more than latency, and they must not go missing together with it.
package gwhealth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/drivers/breaker"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/usage"
)

// latencyBounds are the histogram's bucket upper bounds in milliseconds. They
// have to match the rollup's columns; the two are read together below.
var latencyBounds = []int64{100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000}

// Snapshot is the whole dashboard.
type Snapshot struct {
	Providers    []Provider
	RetryBudget  RetryBudget
	SwitchCounts *SwitchCounts
	StuckMoney   *StuckMoney
}

// Provider is one upstream's last hour.
type Provider struct {
	ID            uuid.UUID
	Slug          string
	BreakerStatus string
	CooldownUntil *time.Time
	Requests1h    int64
	Errors1h      int64
	// Latency1h is nil when the latency rollup could not be read at all. A
	// window that simply holds no samples comes back present with HasSamples
	// false -- the two are different sentences and the dashboard says them
	// differently.
	Latency1h *Latency
}

// Latency is the three numbers the dashboard draws plus the two honesty bits
// that say how far they can be trusted.
type Latency struct {
	HasSamples   bool
	P50Ms        int64
	P95Ms        int64
	MeanMs       int64
	P95Unbounded bool
}

// RetryBudget is the global retry budget's usage.
type RetryBudget struct{ Requests, Retries int64 }

// SwitchCounts are the kill switch's second and third levels, counted across
// the whole table rather than over a capped page.
type SwitchCounts struct {
	ProvidersTotal    int64
	ProvidersDisabled int64
	ModelsTotal       int64
	ModelsDisabled    int64
}

// StuckMoney is the operator's repair queue: asynchronous jobs that reached a
// terminal state while their reservation never moved, settled neither onto the
// customer nor back off them.
//
// It sits on the health dashboard rather than in a report because every row is a
// customer either overcharged or not charged, and a number nobody is shown is a
// number nobody acts on -- which is how this state stayed invisible while the
// index built for it had no reader at all.
//
// No amount, deliberately: a hold is denominated in its organisation's wallet
// currency, so one total across organisations would be a cross-currency sum, and
// the wallet table belongs to Cloud while this package ships in Community too.
type StuckMoney struct {
	Jobs int64
	// Oldest is nil when nothing is stuck. It is what separates a live incident
	// from a single row stranded a month ago.
	Oldest *time.Time
}

// Budget reports the global retry budget. Optional: a deployment that has not
// wired one reports zeroes.
type Budget interface {
	Stats() (requests, retries int64)
}

// Reader assembles snapshots.
type Reader struct {
	q       *gwdb.Queries
	breaker breaker.Store
	budget  Budget
}

// NewReader builds one. breaker and budget are both optional: without a breaker
// store every provider reads as closed, and without a budget the retry counters
// are zero. Neither is a reason to refuse the rest of the dashboard.
func NewReader(pool *pgxpool.Pool, br breaker.Store, budget Budget) *Reader {
	return &Reader{q: gwdb.New(pool), breaker: br, budget: budget}
}

// Read assembles the dashboard.
//
// Only the provider rollup is load-bearing: without it there is no dashboard.
// Latency and the kill-switch counts each degrade on their own.
func (r *Reader) Read(ctx context.Context) (Snapshot, error) {
	rows, err := r.q.ProviderHealthLastHour(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("gwhealth: fetch health data: %w", err)
	}
	// Latency and error rate come from the same rollup over the same window
	// (the last hour), so a post-incident review never has to reconcile two
	// numbers that disagree.
	latency := map[uuid.UUID]Latency{}
	if latRows, lErr := r.q.ProviderLatencyLastHour(ctx); lErr != nil {
		slog.WarnContext(ctx, "gwhealth: failed to fetch provider latency, the rest of the dashboard is returned as usual", "err", lErr)
	} else {
		for _, l := range latRows {
			latency[l.ID.Bytes] = latencyOf(l)
		}
	}
	out := Snapshot{Providers: make([]Provider, 0, len(rows))}
	for _, row := range rows {
		status, until := r.breakerSnapshot(ctx, row.ID)
		p := Provider{
			ID: row.ID.Bytes, Slug: row.Slug,
			BreakerStatus: status, CooldownUntil: until,
			Requests1h: row.Requests, Errors1h: row.Errors,
		}
		if lat, ok := latency[row.ID.Bytes]; ok {
			p.Latency1h = &lat
		}
		out.Providers = append(out.Providers, p)
	}
	if r.budget != nil {
		out.RetryBudget.Requests, out.RetryBudget.Retries = r.budget.Stats()
	}
	// Counting client-side from the catalog list would silently answer "how
	// many are disabled on the first page" once that list is capped, and it
	// would be wrong with no outward sign. As above, failing to read the counts
	// must not take the whole dashboard down.
	if counts, cErr := r.q.CatalogSwitchCounts(ctx); cErr != nil {
		slog.WarnContext(ctx, "gwhealth: failed to fetch kill-switch counts, the rest of the dashboard is returned as usual", "err", cErr)
	} else {
		out.SwitchCounts = &SwitchCounts{
			ProvidersTotal: counts.ProvidersTotal, ProvidersDisabled: counts.ProvidersDisabled,
			ModelsTotal: counts.ModelsTotal, ModelsDisabled: counts.ModelsDisabled,
		}
	}
	// Same rule as the counts above: failing to read the repair queue must not
	// take the dashboard down, and a failed read stays nil rather than
	// collapsing to zero. "Nothing is stuck" and "we could not tell" are
	// different sentences, and on this particular question rendering the second
	// as the first is how the queue goes unwatched.
	if stuck, sErr := r.q.CountStuckMoneyJobs(ctx); sErr != nil {
		slog.WarnContext(ctx, "gwhealth: failed to count jobs whose money never moved, the rest of the dashboard is returned as usual", "err", sErr)
	} else {
		out.StuckMoney = &StuckMoney{Jobs: stuck.Jobs}
		if stuck.OldestTerminalAt.Valid {
			// .UTC() because the wire format is UTC everywhere (CLAUDE.md), and
			// because the console renders this same column through videoTimePtr,
			// which normalizes -- without it one row leaves by two APIs with two
			// offsets. pgconv.TimePtr is not used here for the reason its own
			// doc gives: it passes the driver's location through on purpose and
			// leaves normalizing to the API that is presenting the value.
			oldest := stuck.OldestTerminalAt.Time.UTC()
			out.StuckMoney.Oldest = &oldest
		}
	}
	return out, nil
}

// latencyOf folds the rollup's cumulative histogram into the three numbers plus
// two honesty bits.
//
// The interpolation itself is reused from the usage package, the same code the
// console's usage page runs on. Rules like "how buckets combine", "how a
// quantile is interpolated" and "what to do when the upper bound cannot be
// trusted" should have exactly one implementation: two of them will eventually
// disagree at some boundary, and at that point nobody knows which to believe.
func latencyOf(r gwdb.ProviderLatencyLastHourRow) Latency {
	h := usage.LatencyHistogram{
		Bounds: latencyBounds,
		Cumulative: []int64{
			r.Le100, r.Le250, r.Le500, r.Le1000, r.Le2500,
			r.Le5000, r.Le10000, r.Le30000, r.Le60000, r.Le120000,
		},
		Total:      r.Samples,
		DurationMs: r.DurationMsSum,
	}
	st := h.Stats()
	if !st.HasSamples {
		// With no samples, report no number at all: a 0ms reading gets read as
		// "unbelievably fast".
		return Latency{}
	}
	return Latency{
		HasSamples: true, P50Ms: st.P50, P95Ms: st.P95,
		MeanMs: st.Mean, P95Unbounded: st.P95Unbounded,
	}
}

// breakerSnapshot reads the circuit breaker state. The decision itself lives in
// memory, so what comes back is a snapshot taken at query time and separate
// instances may disagree. The dashboard displays it; nothing decides on it.
func (r *Reader) breakerSnapshot(ctx context.Context, providerID pgtype.UUID) (string, *time.Time) {
	if r.breaker == nil {
		return breaker.StatusClosed, nil
	}
	st, ok, err := r.breaker.Get(ctx, proxy.ProviderBreakerScope(providerID))
	if err != nil || !ok || st.Status == "" {
		return breaker.StatusClosed, nil
	}
	if st.Status == breaker.StatusOpen && !st.Until.IsZero() && time.Now().After(st.Until) {
		return breaker.StatusHalfOpen, &st.Until // cooldown elapsed, awaiting a probe
	}
	if st.Until.IsZero() {
		return st.Status, nil
	}
	until := st.Until
	return st.Status, &until
}
