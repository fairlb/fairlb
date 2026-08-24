package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// The console's usage report.
//
// It moved out of the handler with everything else (ADR-0182), and the move
// made one boundary explicit that the old shape blurred: the report is built
// **whole**, and the finance dimension is applied to it afterwards by whoever
// knows what the caller may see. The old code filled a DTO in place and then
// redacted the same struct, so "what the report is" and "what this caller may
// read" were the same object at two points in time.

// Granularity is the series bucket size.
type Granularity string

const (
	// Hourly buckets are aligned to the hour in UTC.
	Hourly Granularity = "hour"
	// Daily buckets are aligned to midnight in the caller's timezone, which is
	// why the timezone travels with the request rather than being applied to
	// the result.
	Daily Granularity = "day"
)

// GroupBy is the breakdown axis.
type GroupBy string

const (
	ByAPIKey GroupBy = "api_key"
	ByModel  GroupBy = "model"
)

// ReportQuery is one usage question.
type ReportQuery struct {
	From        time.Time
	To          time.Time
	APIKeyID    pgtype.UUID
	Granularity Granularity
	// Timezone is a PostgreSQL timezone name, already normalised: an empty
	// string makes `timezone` raise "invalid time zone name" outright.
	Timezone string
	// GroupBy is empty when no breakdown was asked for.
	GroupBy GroupBy
}

// Point is one series bucket.
type Point struct {
	BucketStart time.Time
	Requests    int64
	TokensIn    int64
	TokensOut   int64
	ChargedNano int64
	Errors      int64
}

// Group is one row of the breakdown.
type Group struct {
	Key   string
	Label string
	// RawKey is the api-key uuid when grouping by key; the transport formats
	// it. Grouping by model has no uuid, and Key already carries the slug.
	RawKey      pgtype.UUID
	Requests    int64
	TokensIn    int64
	TokensOut   int64
	ChargedNano int64
}

// Totals is the period summary.
type Totals struct {
	Requests    int64
	TokensIn    int64
	TokensOut   int64
	ChargedNano int64
	Errors      int64
	Currency    string
	Latency     LatencyStats
}

// Report is the whole answer.
type Report struct {
	Series []Point
	// Groups is nil when no breakdown was asked for, which is different from an
	// empty breakdown.
	Groups []Group
	Totals Totals
}

// ReportReader builds usage reports. Bind it to the caller's org-scoped
// transaction: these rows are under row-level security.
type ReportReader struct{ q *gwdb.Queries }

func NewReportReader(q *gwdb.Queries) *ReportReader { return &ReportReader{q: q} }

// Report answers one usage question in full.
func (r *ReportReader) Report(ctx context.Context, orgID pgtype.UUID, in ReportQuery) (Report, error) {
	var out Report
	var err error
	if out.Series, err = r.series(ctx, orgID, in); err != nil {
		return Report{}, err
	}
	if in.GroupBy != "" {
		if out.Groups, err = r.groups(ctx, orgID, in); err != nil {
			return Report{}, err
		}
	}
	if out.Totals, err = r.totals(ctx, orgID, in); err != nil {
		return Report{}, err
	}
	return out, nil
}

func stamps(in ReportQuery) (pgtype.Timestamptz, pgtype.Timestamptz) {
	return pgtype.Timestamptz{Time: in.From, Valid: true},
		pgtype.Timestamptz{Time: in.To, Valid: true}
}

func (r *ReportReader) series(ctx context.Context, orgID pgtype.UUID, in ReportQuery) ([]Point, error) {
	f, t := stamps(in)
	if in.Granularity == Hourly {
		rows, err := r.q.UsageSeriesByHour(ctx, gwdb.UsageSeriesByHourParams{
			OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID,
		})
		if err != nil {
			return nil, fmt.Errorf("usage: read hourly series: %w", err)
		}
		out := make([]Point, 0, len(rows))
		for _, row := range rows {
			out = append(out, Point{
				BucketStart: row.Bucket.Time, Requests: row.Requests,
				TokensIn: row.TokensIn, TokensOut: row.TokensOut,
				ChargedNano: row.ChargedNano, Errors: row.Errors,
			})
		}
		return out, nil
	}
	rows, err := r.q.UsageSeriesByDay(ctx, gwdb.UsageSeriesByDayParams{
		OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID, Tz: in.Timezone,
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read daily series: %w", err)
	}
	out := make([]Point, 0, len(rows))
	for _, row := range rows {
		out = append(out, Point{
			BucketStart: row.Bucket.Time, Requests: row.Requests,
			TokensIn: row.TokensIn, TokensOut: row.TokensOut,
			ChargedNano: row.ChargedNano, Errors: row.Errors,
		})
	}
	return out, nil
}

// groups fills in the grouped breakdown.
//
// The key filter is carried all the way here. Within one usage response the
// series and the totals both honour api_key_id -- and the groups once did not,
// so "this key's totals" and "the whole org's model distribution" ended up in
// the same report. The console does not hit it today (the key drawer wants the
// series, not the groups), but the spec offers both parameters to any
// management key: nobody having hit it is not the same as it not being broken.
func (r *ReportReader) groups(ctx context.Context, orgID pgtype.UUID, in ReportQuery) ([]Group, error) {
	f, t := stamps(in)
	groups := []Group{}
	if in.GroupBy == ByAPIKey {
		rows, err := r.q.UsageGroupByKey(ctx, gwdb.UsageGroupByKeyParams{
			OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID,
		})
		if err != nil {
			return nil, fmt.Errorf("usage: group by key: %w", err)
		}
		for _, row := range rows {
			groups = append(groups, Group{
				RawKey: row.Key, Label: row.Label, Requests: row.Requests,
				TokensIn: row.TokensIn, TokensOut: row.TokensOut, ChargedNano: row.ChargedNano,
			})
		}
		return groups, nil
	}
	rows, err := r.q.UsageGroupByModel(ctx, gwdb.UsageGroupByModelParams{
		OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID,
	})
	if err != nil {
		return nil, fmt.Errorf("usage: group by model: %w", err)
	}
	for _, row := range rows {
		// Prefer the display name, fall back to the slug: a model deleted from
		// the catalog still shows up in historical usage, and then the label is
		// empty -- a slug reads better than a blank.
		label := row.Label
		if label == "" {
			label = row.Key
		}
		groups = append(groups, Group{
			Key: row.Key, Label: label, Requests: row.Requests,
			TokensIn: row.TokensIn, TokensOut: row.TokensOut, ChargedNano: row.ChargedNano,
		})
	}
	return groups, nil
}

func (r *ReportReader) totals(ctx context.Context, orgID pgtype.UUID, in ReportQuery) (Totals, error) {
	f, t := stamps(in)
	row, err := r.q.UsageTotals(ctx, gwdb.UsageTotalsParams{
		OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID,
	})
	if err != nil {
		return Totals{}, fmt.Errorf("usage: read totals: %w", err)
	}
	// The currency comes from the org: the rollups carry no currency dimension,
	// and an org's currency cannot change once created.
	currency, err := r.q.GetOrgCurrency(ctx, orgID)
	if err != nil {
		currency = "USD"
	}
	out := Totals{
		Requests: row.Requests, TokensIn: row.TokensIn, TokensOut: row.TokensOut,
		ChargedNano: row.ChargedNano, Errors: row.Errors, Currency: currency,
	}
	if out.Latency, err = r.latency(ctx, orgID, in); err != nil {
		return Totals{}, err
	}
	return out, nil
}

// latency reads the period's latency quantiles. The histogram columns are
// summed and then interpolated -- not the quantiles of each bucket averaged,
// which would not be a quantile at all.
func (r *ReportReader) latency(ctx context.Context, orgID pgtype.UUID, in ReportQuery) (LatencyStats, error) {
	f, t := stamps(in)
	row, err := r.q.UsageLatencyHistogram(ctx, gwdb.UsageLatencyHistogramParams{
		OrgID: orgID, FromTs: f, ToTs: t, ApiKeyID: in.APIKeyID,
	})
	if err != nil {
		return LatencyStats{}, fmt.Errorf("usage: read latency histogram: %w", err)
	}
	return LatencyHistogram{
		Bounds: LatencyBounds(),
		Cumulative: []int64{
			row.Le100, row.Le250, row.Le500, row.Le1000, row.Le2500,
			row.Le5000, row.Le10000, row.Le30000, row.Le60000, row.Le120000,
		},
		Total:      row.Samples,
		DurationMs: row.DurationMsSum,
	}.Stats(), nil
}
