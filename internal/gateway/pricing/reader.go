package pricing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// ErrNotFound is "there is no such model, plan or organization". The transport
// maps it; the domain does not know about status codes.
var ErrNotFound = errors.New("pricing: not found")

// Reader answers the pricing questions. It holds no transaction of its own:
// WithQueries binds it to one a writer already opened, so a write can read its
// own uncommitted state through the same code that serves reads.
type Reader struct {
	pool *pgxpool.Pool
	q    *gwdb.Queries
}

func NewReader(pool *pgxpool.Pool) *Reader {
	return &Reader{pool: pool, q: gwdb.New(pool)}
}

// WithQueries returns a Reader that runs inside the caller's transaction.
func (r *Reader) WithQueries(q *gwdb.Queries) *Reader {
	return &Reader{pool: r.pool, q: q}
}

func pgID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// ceilRate applies the multiplier and rounds up. ok is false if the result does
// not fit.
//
// Up, not nearest: the multiplier is a markup, and rounding a markup down means
// charging less than the configured rate. big.Int for the intermediate, because
// nano × basis points overflows int64 long before the result does.
//
// The `ok` is not defensive padding. `upstream_*_nano_per_mtok` is capped at
// 92233720368547758 and `multiplier_bps` at 100000, and that pair is exactly
// what still fits — the cap is MaxInt64/100000. So today the false branch is
// unreachable, and it exists because the previous version ended in a bare
// `n.Int64()`, which **truncates silently**: relax either CHECK by one step and
// a price becomes a negative number with nothing to say it happened. A money
// value is the wrong place for a silent wrap.
func ceilRate(nano int64, multiplierBps int32) (int64, bool) {
	n := new(big.Int).Mul(big.NewInt(nano), big.NewInt(int64(multiplierBps)))
	n.Add(n, big.NewInt(9999)).Quo(n, big.NewInt(10000))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// published applies the multiplier to all four buckets.
func published(official TokenRates, multiplierBps int32, modelID uuid.UUID) (TokenRates, error) {
	out := TokenRates{}
	for _, f := range []struct {
		name string
		from int64
		to   *int64
	}{
		{"input", official.Input, &out.Input},
		{"output", official.Output, &out.Output},
		{"cache_read", official.CacheRead, &out.CacheRead},
		{"cache_write", official.CacheWrite, &out.CacheWrite},
	} {
		v, ok := ceilRate(f.from, multiplierBps)
		if !ok {
			return TokenRates{}, fmt.Errorf(
				"pricing: model %s %s rate %d at %d bps does not fit an int64",
				modelID, f.name, f.from, multiplierBps)
		}
		*f.to = v
	}
	return out, nil
}

var bucketFromDB = map[string]Bucket{
	"in": BucketIn, "out": BucketOut,
	"cache_read": BucketCacheRead, "cache_write": BucketCacheWrite,
	"audio_in": BucketAudioIn, "audio_out": BucketAudioOut,
}

// ModelPricing reads one model's price.
//
// A model with no price row is not an error: it comes back with Priced false.
// Absent and zero look identical on screen if they are conflated, and they call
// for completely different action.
func (r *Reader) ModelPricing(ctx context.Context, modelID uuid.UUID) (ModelPricing, error) {
	row, err := r.q.GetModelPricing(ctx, pgID(modelID))
	if db.IsNoRows(err) {
		var exists bool
		if e := r.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM models WHERE id=$1)`, modelID).Scan(&exists); e != nil {
			return ModelPricing{}, e
		}
		if !exists {
			return ModelPricing{}, ErrNotFound
		}
		return ModelPricing{ModelID: modelID, Priced: false}, nil
	}
	if err != nil {
		return ModelPricing{}, err
	}

	out := ModelPricing{
		ModelID:       modelID,
		Priced:        true,
		BillingMode:   BillingMode(row.BillingMode),
		MultiplierBps: row.MultiplierBps,
		SourceName:    row.SourceName,
		SourceURL:     row.SourceUrl,
		Reason:        row.Reason,
		UpdatedAt:     row.UpdatedAt.Time.UTC(),
		Version:       row.UpdatedAt.Time.UTC(),
	}
	if row.VerifiedAt.Valid {
		out.CheckedAt = row.VerifiedAt.Time.UTC()
	}
	// `model_pricing_complete_ck` guarantees all four are present on a row that
	// exists, so this reads them without a per-field null check. If that
	// constraint ever loosens, this is the code that has to be told.
	out.Official = TokenRates{
		Input:      row.UpstreamInNanoPerMtok.Int64,
		Output:     row.UpstreamOutNanoPerMtok.Int64,
		CacheRead:  row.UpstreamCacheReadNanoPerMtok.Int64,
		CacheWrite: row.UpstreamCacheWriteNanoPerMtok.Int64,
	}
	if out.BillingMode != BillingFree {
		if out.Published, err = published(out.Official, out.MultiplierBps, modelID); err != nil {
			return ModelPricing{}, err
		}
	}

	dims, err := r.q.ListModelPriceDimensionRates(ctx, pgID(modelID))
	if err != nil {
		return ModelPricing{}, err
	}
	for _, d := range dims {
		bucket, ok := bucketFromDB[d.Bucket]
		if !ok {
			// The column's CHECK constrains this to the six known values, so a
			// miss means the schema and this map have drifted apart. Skipping
			// the row would hide a rate that is being charged; refusing shows
			// the price is not describable.
			return ModelPricing{}, fmt.Errorf(
				"pricing: model %s has a dimension rate on unknown bucket %q", modelID, d.Bucket)
		}
		out.DimensionRates = append(out.DimensionRates, DimensionRate{
			Bucket: bucket, ServiceTier: d.ServiceTier, Variant: d.Variant,
			MinInputTokens: d.MinInputTokens, NanoPerMTok: d.NanoPerMtok,
		})
	}
	tools, err := r.q.ListModelPriceToolRates(ctx, pgID(modelID))
	if err != nil {
		return ModelPricing{}, err
	}
	for _, t := range tools {
		out.ToolRates = append(out.ToolRates, ToolRate{Tool: t.Tool, NanoPerCall: t.NanoPerCall})
	}
	return out, nil
}

// Plans lists a page of pricing plans, with how many organizations are on each.
//
// Keyed on (is_default, slug): the default plan stays first, everything else
// sorts by slug, and the cursor follows that same key (ADR-0191).
func (r *Reader) Plans(ctx context.Context, page cursorpage.KeyPage, search string) ([]Plan, error) {
	cursorIsDefault, err := page.BoolAt(0)
	if err != nil {
		return nil, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid cursor")
	}
	return r.plans(ctx, gwdb.ListPricingPlansParams{
		HasCursor:       page.HasKey(),
		CursorIsDefault: cursorIsDefault,
		CursorSlug:      page.At(1),
		Search:          fdb.SearchTerm(search),
		Lim:             page.ProbeLimit(),
	})
}

func (r *Reader) plans(ctx context.Context, arg gwdb.ListPricingPlansParams) ([]Plan, error) {
	rows, err := r.q.ListPricingPlans(ctx, arg)
	if err != nil {
		return nil, err
	}
	out := make([]Plan, 0, len(rows))
	for _, row := range rows {
		out = append(out, Plan{
			ID: uuid.UUID(row.ID.Bytes), Slug: row.Slug, Name: row.Name,
			Description: row.Description, IsDefault: row.IsDefault, Status: row.Status,
			OrgCount: row.OrgCount, DefaultMultiplierBps: row.DefaultMultiplierBps,
			Reason:    row.Reason,
			CreatedAt: row.CreatedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
		})
	}
	return out, nil
}

// Plan reads one plan.
//
// It goes through Plans because org_count is an aggregate the list query
// computes and the single-row query does not. Two queries that disagree about
// what a plan is would be two answers to the same question.
func (r *Reader) Plan(ctx context.Context, id uuid.UUID) (Plan, error) {
	row, err := r.q.GetPricingPlan(ctx, pgID(id))
	if db.IsNoRows(err) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	plans, err := r.plans(ctx, gwdb.ListPricingPlansParams{HasID: true, OnlyID: pgID(id), Lim: 1})
	if err != nil {
		return Plan{}, err
	}
	for _, p := range plans {
		if p.ID == id {
			p.Version = uuid.UUID(row.Etag.Bytes)
			return p, nil
		}
	}
	return Plan{}, ErrNotFound
}

// PlanModelOverrides lists a plan's per-model markups, with the plan's version
// so a caller can make a conditional write against the set it just read.
func (r *Reader) PlanModelOverrides(ctx context.Context, id uuid.UUID) ([]PlanModelOverride, uuid.UUID, error) {
	row, err := r.q.GetPricingPlan(ctx, pgID(id))
	if db.IsNoRows(err) {
		return nil, uuid.UUID{}, ErrNotFound
	}
	if err != nil {
		return nil, uuid.UUID{}, err
	}
	rows, err := r.q.ListPricingPlanModelOverrides(ctx, pgID(id))
	if err != nil {
		return nil, uuid.UUID{}, err
	}
	out := make([]PlanModelOverride, 0, len(rows))
	for _, o := range rows {
		out = append(out, PlanModelOverride{
			ModelID: uuid.UUID(o.ModelID.Bytes), ModelSlug: o.ModelSlug,
			MultiplierBps: o.MultiplierBps,
		})
	}
	return out, uuid.UUID(row.Etag.Bytes), nil
}

// OrgPlan reads the plan an organization is effectively on.
func (r *Reader) OrgPlan(ctx context.Context, orgID pgtype.UUID) (OrgPlan, error) {
	eff, err := r.q.GetEffectivePricingPlanForOrg(ctx, orgID)
	if db.IsNoRows(err) {
		return OrgPlan{}, ErrNotFound
	}
	if err != nil {
		return OrgPlan{}, err
	}
	planID := uuid.UUID(eff.PricingPlanID.Bytes)
	return OrgPlan{
		OrgID: uuid.UUID(orgID.Bytes), PlanID: planID,
		PlanSlug: eff.Slug, PlanName: eff.Name,
		InheritedDefault: eff.IsDefault,
		Version:          planID,
	}, nil
}

// ImportCandidate is a model the reference-price import may touch, with just
// enough of its current price to decide whether to.
//
// A domain type rather than the query's row, even though the import is the only
// caller: returning sqlc's struct would make this package's API a function of
// how a query happens to be written, and the whole point of the move was that
// the layers above stop being shaped that way.
type ImportCandidate struct {
	ModelID   uuid.UUID
	ModelSlug string
	Priced    bool
	// VerifiedAt is when a person last checked this price against the vendor.
	// Zero means never — which is what the import treats as "may touch".
	VerifiedAt    time.Time
	BillingMode   BillingMode
	MultiplierBps int32
	// Official is what is stored today. Per-field Set here and plain integers
	// on ModelPricing, because this query includes unpriced models -- and for
	// those the columns really are NULL.
	Official TokenRatesInput
}

// UsableRoute is an enabled route on an enabled provider: where the import
// reads a vendor's own model id from.
type UsableRoute struct {
	ModelID         uuid.UUID
	ProviderModelID string
	ProviderSlug    string
	ProviderVendor  string
	BaseURL         string
}

// ImportCandidates lists every model the import may consider.
func (r *Reader) ImportCandidates(ctx context.Context) ([]ImportCandidate, error) {
	rows, err := r.q.ListModelsForPriceImport(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ImportCandidate, 0, len(rows))
	for _, row := range rows {
		out = append(out, ImportCandidate{
			ModelID: uuid.UUID(row.ModelID.Bytes), ModelSlug: row.ModelSlug,
			Priced: row.Priced, VerifiedAt: verifiedTime(row.VerifiedAt),
			BillingMode: BillingMode(row.BillingMode), MultiplierBps: row.MultiplierBps,
			Official: TokenRatesInput{
				Input:      RateInput{Nano: row.UpstreamInNanoPerMtok.Int64, Set: row.UpstreamInNanoPerMtok.Valid},
				Output:     RateInput{Nano: row.UpstreamOutNanoPerMtok.Int64, Set: row.UpstreamOutNanoPerMtok.Valid},
				CacheRead:  RateInput{Nano: row.UpstreamCacheReadNanoPerMtok.Int64, Set: row.UpstreamCacheReadNanoPerMtok.Valid},
				CacheWrite: RateInput{Nano: row.UpstreamCacheWriteNanoPerMtok.Int64, Set: row.UpstreamCacheWriteNanoPerMtok.Valid},
			},
		})
	}
	return out, nil
}

// UsableRoutes lists the enabled routes the import can read upstream ids from.
func (r *Reader) UsableRoutes(ctx context.Context) ([]UsableRoute, error) {
	rows, err := r.q.ListUsableRoutesForPriceImport(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UsableRoute, 0, len(rows))
	for _, row := range rows {
		out = append(out, UsableRoute{
			ModelID: uuid.UUID(row.ModelID.Bytes), ProviderModelID: row.ProviderModelID,
			ProviderSlug: row.ProviderSlug, ProviderVendor: row.ProviderVendor,
			BaseURL: row.BaseUrl,
		})
	}
	return out, nil
}

func verifiedTime(v pgtype.Timestamptz) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time.UTC()
}

// PlanCursor points just past p. Booleans travel as "t"/"f" — the same one-letter
// form PostgreSQL itself prints — so the cursor stays a text tuple.
func PlanCursor(p Plan) string {
	return cursorpage.EncodeKey(boolKey(p.IsDefault), p.Slug)
}

// PlanCursorParts is the component count the transport hands to ParseKeyPage.
const PlanCursorParts = 2

func boolKey(b bool) string {
	if b {
		return "t"
	}
	return "f"
}
