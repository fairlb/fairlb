package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"
	"github.com/fairlb/fairlb/foundation/money"

	"github.com/fairlb/fairlb/foundation/publicid"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/settings"
)

// Per-org spend anomaly alerting.
//
// It alerts; it never cuts service off. The most common reason for a spend
// spike is that the organization's own business grew, and cutting them off then means
// cutting them off at the exact moment they most want to be served -- which is
// also the moment they are most willing to pay. The situations that genuinely
// need a hard gate (insufficient balance, key budget exhausted) already have
// one each, and all of those criteria are deterministic. This one is
// statistical, and a statistical criterion should not drive a destructive
// action.

const (
	// KeyAnomalyMultiplier: alert when this hour's spend exceeds the baseline
	// by this multiple.
	KeyAnomalyMultiplier = "gateway.anomaly_multiplier"
	// KeyAnomalyFloor: absolute floor, in the billing currency; nothing below
	// it ever alerts.
	KeyAnomalyFloor = "gateway.anomaly_floor"
)

// Specs is what this package contributes to the settings registry（ADR-0194）。
func Specs() []settings.Spec {
	var out []settings.Spec
	// Both keys are ImpactNormal: this job only alerts (see the top of the
	// file), so a bad value means more alerts or fewer. It is reversible on the
	// spot and touches neither live traffic nor anyone's bill.
	out = append(out, settings.Spec{
		Key: KeyAnomalyMultiplier, Kind: settings.KindInt,
		Range:          &settings.Range{Min: 2, Max: 1000},
		Description:    "Alert when an org's hourly spend exceeds this multiple of its 7-day median. Alerts only; service is never interrupted.",
		DescriptionKey: "settingDescAnomalyMultiplier",
		Group:          settings.GroupOperations, Impact: settings.ImpactNormal,
		Default: json.RawMessage(`10`),
	})
	out = append(out, settings.Spec{
		Key: KeyAnomalyFloor, Kind: settings.KindMoney,
		// The upper bound is not decoration. This once declared only a minimum,
		// and the old range validation treated "min and max both zero" as the
		// sentinel for "unconstrained" -- so a zero maximum skipped validation
		// entirely, a negative floor could be stored, every org exceeded it
		// every hour, and the alert became a broadcast. With an explicit range,
		// the minimum of 0 is a real bound; the maximum of 1e15 nano (a million
		// dollars an hour) is wide enough to be no obstacle while still
		// catching an extra zero.
		Range: &settings.Range{Min: 0, Max: 1_000_000_000_000_000},
		// An absolute floor is required: a new org's baseline is zero, so a
		// pure multiple alerts on their very first spend. The default of 1e9
		// nano is one dollar -- a "spike" below that is not worth anyone's
		// attention. Typed in main units; the range above is in nano.
		Description:    "Absolute floor for anomaly alerts, in the billing currency. An hour spending less than this never alerts.",
		DescriptionKey: "settingDescAnomalyFloor",
		Group:          settings.GroupOperations, Impact: settings.ImpactNormal,
		Default: json.RawMessage(`"1"`),
	})
	return out
}

// AnomalyArgs is the spend anomaly detection job.
type AnomalyArgs struct{}

func (AnomalyArgs) Kind() string { return "gateway_spend_anomaly" }

// AnomalyInterval is how often detection runs. It is aligned with the hourly
// buckets but denser: a bucket keeps accumulating past the top of the hour, and
// finding a spike early is worth more than waiting for the bucket to close.
const AnomalyInterval = 15 * time.Minute

// AnomalyWorker detects per-org spend spikes.
type AnomalyWorker struct {
	river.WorkerDefaults[AnomalyArgs]
	q       *gwdb.Queries
	set     *settings.Store
	alerter Alerter
	// alerted suppresses duplicates: one alert per org per hour. Without it a
	// single spike would fire four times over an hour of 15-minute rounds.
	alerted map[string]time.Time
}

func NewAnomalyWorker(pool *pgxpool.Pool, set *settings.Store, alerter Alerter) *AnomalyWorker {
	return &AnomalyWorker{
		q: gwdb.New(pool), set: set, alerter: alerter,
		alerted: make(map[string]time.Time),
	}
}

func (w *AnomalyWorker) Work(ctx context.Context, _ *river.Job[AnomalyArgs]) error {
	multiplier := w.intSetting(ctx, KeyAnomalyMultiplier, 10)
	floor := w.moneySetting(ctx, KeyAnomalyFloor, 1_000_000_000)

	rows, err := w.q.DetectSpendAnomalies(ctx, gwdb.DetectSpendAnomaliesParams{
		FloorNano: floor, Multiplier: multiplier,
	})
	if err != nil {
		return fmt.Errorf("usage: detect spend anomalies: %w", err)
	}
	hour := time.Now().UTC().Truncate(time.Hour)
	for _, r := range rows {
		org := publicid.UUIDString(r.OrgID)
		if last, ok := w.alerted[org]; ok && last.Equal(hour) {
			continue
		}
		w.alerted[org] = hour
		w.report(ctx, r, multiplier)
	}
	w.pruneAlerted(hour)
	return nil
}

// report raises one anomaly alert. The body has to let the reader tell "the
// organization's business grew" from "something is wrong" straight away.
func (w *AnomalyWorker) report(ctx context.Context, r gwdb.DetectSpendAnomaliesRow, multiplier int64) {
	org := publicid.UUIDString(r.OrgID)
	var b strings.Builder
	fmt.Fprintf(&b, "Org %s spent %d nano this hour", org, r.CurrentSpend)
	if r.BaselineSpend > 0 {
		fmt.Fprintf(&b, ", which is %.1f times its 7-day median of %d nano",
			float64(r.CurrentSpend)/float64(r.BaselineSpend), r.BaselineSpend)
	} else {
		b.WriteString(", and this org has no recorded spend over the previous 7 days (a new organization, or one returning after a long idle period)")
	}
	fmt.Fprintf(&b, ". The alert threshold is %dx.\n\n"+
		"This is an alert, not a block: service is unaffected. The most "+
		"common reason for a spike is that the organization's own traffic grew. "+
		"If you suspect a leaked key or abusive calls, inspect the org's "+
		"usage in the console, and revoke its keys or suspend it from the "+
		"admin console if needed.", multiplier)

	detail := b.String()
	slog.WarnContext(ctx, "org spend anomaly", "org", org,
		"current_nano", r.CurrentSpend, "baseline_nano", r.BaselineSpend)
	if w.alerter != nil {
		w.alerter.Alert(ctx, "Org spend spike", detail)
	}
}

// pruneAlerted drops expired suppression entries so a long-running process does
// not accumulate the map forever.
func (w *AnomalyWorker) pruneAlerted(hour time.Time) {
	for org, t := range w.alerted {
		if t.Before(hour) {
			delete(w.alerted, org)
		}
	}
}

// intSetting reads an integer setting, falling back to the default if the read
// fails or the key is absent -- a detection job should not stop entirely
// because one setting could not be read.
// moneySetting reads a KindMoney key as nano. The default is in nano too,
// because that is the unit the query takes.
func (w *AnomalyWorker) moneySetting(ctx context.Context, key string, def int64) int64 {
	if w.set == nil {
		return def
	}
	var v string
	found, err := w.set.Get(ctx, key, &v)
	if err != nil || !found || v == "" {
		return def
	}
	n, err := money.ParseDecimalNano(v)
	if err != nil {
		return def
	}
	return n
}

func (w *AnomalyWorker) intSetting(ctx context.Context, key string, def int64) int64 {
	if w.set == nil {
		return def
	}
	var v int64
	found, err := w.set.Get(ctx, key, &v)
	if err != nil || !found {
		return def
	}
	return v
}

// AnomalyPeriodicJob builds the periodic job, registered at the assembly point.
func AnomalyPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		AnomalyInterval,
		func() (river.JobArgs, *river.InsertOpts) { return AnomalyArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: false},
	)
}
