package gwstaffapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/pricing"
	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
)

// Prefilling model prices from a bundled reference dataset.
//
// # What this is not
//
// It is not price synchronisation. Nothing here runs on a timer, nothing here
// reaches the network, and no request path consults it: an operator runs it,
// once, and what it writes is ordinary configuration they can then edit or
// delete. The reason a gateway that bills by the token keeps external price
// feeds off the request path is unchanged by this -- a bill has to be traceable
// to a number somebody stands behind, and a feed that can move on its own is
// not that.
//
// What it removes is a different problem. A fresh install has an empty
// catalogue and an empty price table, and an unpriced model is refused rather
// than served free. So the shortest path from "installed" to "first request"
// runs through typing four rates per model by hand, from a vendor page, into a
// form -- for every model the operator wants. That is transcription, and
// transcription is where digits get dropped.
//
// # What keeps it honest
//
// Every row it writes is marked unverified: `verified_at` stays NULL, which is
// the same field the console's editor fills in when a person confirms a price
// against the vendor's own list. So "a dataset suggested this" never quietly
// becomes "somebody checked this", and a price a human has already checked is
// never overwritten -- not even with --force, which exists to overwrite the
// ones nobody has.
//
// Every row also records where it came from: the dataset, the snapshot it was
// taken from, the entry that matched and how it matched. And every model the
// import declined to price is named, with the reason. A price left blank
// without saying so is the failure mode this whole feature exists to remove;
// reintroducing it in the import would be an odd way to finish.

// ImportOptions configures one import run.
type ImportOptions struct {
	// Data is the reference dataset. The caller chooses between the copy
	// bundled with the binary and one it read from a file.
	Data *refdata.Dataset
	// Force overwrites a stored price that nobody has verified. Without it, a
	// model that already has any price is left alone: refilling prices an
	// operator may have adjusted is not what "import the reference rates"
	// should mean by default.
	Force bool
	// Now is the day the reference is resolved against. The dataset carries
	// announced price changes before they take effect, so reading it without a
	// date returns rates that are not in force.
	Now time.Time
	// DryRun reports every outcome without keeping any of it. The work is
	// really done -- each price is validated, risk-assessed and written inside
	// a transaction that is then rolled back -- so the report is what a real
	// run would do rather than a second opinion arrived at by other means.
	DryRun bool
	// Models narrows the run to these models. Empty means the whole catalogue,
	// which is what the command line asks for; a caller that has just wired up
	// a handful of models names them, so that pressing "fill these in" cannot
	// quietly price the rest of the catalogue as well.
	//
	// An id that names no model is reported rather than dropped: a run that
	// silently ignores part of what it was asked to do is the same failure this
	// whole command exists to remove.
	Models []uuid.UUID
	// Actor is the signed-in identity that asked for this run, recorded on
	// every row it writes.
	//
	// It is empty for a run nobody signed in for. The command line inside a
	// container has no identity to record, and inventing one would be worse
	// than the blank: the row would claim a person did something they did not.
	// It never fills in verified_at, whoever runs it -- pressing a button is
	// not the same act as checking a rate against the vendor's own list.
	Actor pgtype.UUID
}

// ImportOutcome is what happened to one model. Everything except Priced,
// Updated and Unchanged means no price was written, and every one of those is
// meant to be shown to the operator.
type ImportOutcome string

const (
	// ImportPriced: the model had no price and now has the reference rates.
	ImportPriced ImportOutcome = "priced"
	// ImportUpdated: an unverified price differed from the reference and was
	// overwritten (--force only).
	ImportUpdated ImportOutcome = "updated"
	// ImportUnchanged: the stored price already equals the reference.
	ImportUnchanged ImportOutcome = "unchanged"
	// ImportKept: the model already has a price; --force would overwrite it.
	ImportKept ImportOutcome = "kept"
	// ImportVerified: a human has checked this price. Never overwritten.
	ImportVerified ImportOutcome = "verified"
	// ImportSkipped: no reference rate could be resolved. Detail says why.
	ImportSkipped ImportOutcome = "skipped"
)

// ImportResult is one model's outcome.
type ImportResult struct {
	ModelSlug string
	Outcome   ImportOutcome
	// Detail is written for a person reading a terminal: for a skip it is the
	// reason, for a write it is the dataset entry that supplied the rates.
	Detail string
}

// ImportReport is the whole run, in the order models were considered.
type ImportReport struct {
	Snapshot       refdata.Snapshot
	DatasetEntries int
	Results        []ImportResult
	// DryRun says nothing was kept. It travels with the report because every
	// other field reads identically either way: "priced 40 models" is the same
	// sentence whether or not the rows survived, and only this distinguishes
	// them.
	DryRun bool
	// Acknowledged lists the warning codes the import confirmed on the
	// operator's behalf. It is reported rather than left implicit: a bulk write
	// that silently accepts every warning the interactive editor would stop on
	// is worth being able to see.
	Acknowledged []string
}

// Count returns how many models ended with the given outcome.
func (r *ImportReport) Count(o ImportOutcome) int {
	n := 0
	for _, res := range r.Results {
		if res.Outcome == o {
			n++
		}
	}
	return n
}

// Of returns the results with the given outcome, in order.
func (r *ImportReport) Of(o ImportOutcome) []ImportResult {
	var out []ImportResult
	for _, res := range r.Results {
		if res.Outcome == o {
			out = append(out, res)
		}
	}
	return out
}

// importAcknowledged are the warnings this import confirms on the operator's
// behalf, and each is here for a stated reason rather than because confirming
// everything was easier:
//
//   - unknown_procurement_cost fires when a model has no usable provider, which
//     the import already refuses to price -- so reaching it means a route was
//     disabled mid-run, and the margin being uncomputable does not make the
//     vendor's list price wrong.
//   - customer_price_drop and switch_to_free compare against the price being
//     replaced. By default there is nothing to replace; under --force the whole
//     point is to replace an unverified guess with the vendor's own number, and
//     that number is allowed to be lower.
//   - negative_margin and multi_tenant_impact follow from the same comparison.
//
// Blockers are deliberately absent. A blocker is not a business decision to be
// signed off -- it means the resulting row would not add up, and a bulk writer
// is the last thing that should be allowed to wave that through.
var importAcknowledged = []PricingRiskCode{
	UnknownProcurementCost, CustomerPriceDrop, SwitchToFree,
	NegativeMargin, MultiTenantImpact,
}

// ImportReferencePrices fills model prices in from a reference dataset.
//
// The catalog service is optional and only used to invalidate cached prices, so
// a run against a database no gateway is currently serving needs nothing.
type ReferencePriceImportConfig struct {
	Pool    *pgxpool.Pool
	Catalog *catalog.Service
	Options ImportOptions
}

func ImportReferencePrices(ctx context.Context, cfg ReferencePriceImportConfig) (*ImportReport, error) {
	return NewPGPricingAdminService(PGPricingAdminConfig{
		Pool: cfg.Pool, Catalog: cfg.Catalog,
	}).ImportReferencePrices(ctx, cfg.Options)
}

// ImportReferencePrices is the same run reached through an already-assembled
// service, which is how the staff endpoint gets at it: that assembly already
// holds the pool and the catalog, and building a second one per request would
// give the invalidation a different cache to talk to than the one serving
// traffic.
func (s *pgPricingAdminService) ImportReferencePrices(
	ctx context.Context, opts ImportOptions,
) (*ImportReport, error) {
	if opts.Data == nil {
		return nil, fmt.Errorf("pricing import: no reference dataset was supplied")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	// Checked once, up front, rather than met on the first save. It is a
	// blocker for every model equally, so discovering it at model one leaves a
	// run that wrote nothing but still had to be explained -- and discovering
	// it at model forty leaves a half-applied import.
	var hasDefaultPlan bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pricing_plans WHERE is_default AND status='active')`).
		Scan(&hasDefaultPlan); err != nil {
		return nil, fmt.Errorf("pricing import: check the default pricing plan: %w", err)
	}
	if !hasDefaultPlan {
		return nil, fmt.Errorf(
			"pricing import: there is no active default pricing plan, so every priced model " +
				"would still fail closed at request time; restore it first")
	}

	models, err := s.reader.ImportCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("pricing import: list models: %w", err)
	}
	routes, err := s.reader.UsableRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("pricing import: list routes: %w", err)
	}
	byModel := map[uuid.UUID][]pricing.UsableRoute{}
	for _, r := range routes {
		byModel[r.ModelID] = append(byModel[r.ModelID], r)
	}

	report := &ImportReport{
		Snapshot: opts.Data.Snapshot, DatasetEntries: opts.Data.Entries,
		DryRun:  opts.DryRun,
		Results: make([]ImportResult, 0, len(models)),
	}
	for _, code := range importAcknowledged {
		report.Acknowledged = append(report.Acknowledged, string(code))
	}
	models, missing := narrowToRequested(models, opts.Models)
	for _, m := range models {
		res, err := s.importOneModel(ctx, opts, m, byModel[m.ModelID])
		if err != nil {
			return report, err
		}
		report.Results = append(report.Results, res)
	}
	report.Results = append(report.Results, missing...)
	return report, nil
}

// narrowToRequested keeps the models the caller asked for, and turns the ids
// that match nothing into results of their own.
//
// The second return value is why this is not a one-line filter. An id naming no
// model usually means the caller's list is stale -- a model deleted, or a
// creation that failed earlier in the same request -- and dropping it silently
// would leave the caller believing something was considered when it never was.
func narrowToRequested(
	all []pricing.ImportCandidate, want []uuid.UUID,
) ([]pricing.ImportCandidate, []ImportResult) {
	if len(want) == 0 {
		return all, nil
	}
	wanted := make(map[[16]byte]bool, len(want))
	for _, id := range want {
		wanted[id] = true
	}
	kept := make([]pricing.ImportCandidate, 0, len(want))
	for _, m := range all {
		if wanted[m.ModelID] {
			kept = append(kept, m)
			delete(wanted, m.ModelID)
		}
	}
	var missing []ImportResult
	for _, id := range want {
		if !wanted[id] {
			continue
		}
		delete(wanted, id)
		missing = append(missing, ImportResult{
			ModelSlug: id.String(),
			Outcome:   ImportSkipped,
			Detail:    "no model with this id exists here, so there was nothing to price",
		})
	}
	return kept, missing
}

func (s *pgPricingAdminService) importOneModel(
	ctx context.Context, opts ImportOptions,
	m pricing.ImportCandidate, routes []pricing.UsableRoute,
) (ImportResult, error) {
	out := ImportResult{ModelSlug: m.ModelSlug}
	// A price a person has checked outranks anything a dataset says, so that
	// decision is read before any lookup is attempted -- there is no reference
	// rate that could change the answer.
	if !m.VerifiedAt.IsZero() {
		out.Outcome = ImportVerified
		out.Detail = "checked by a human on " + m.VerifiedAt.Format(time.DateOnly)
		return out, nil
	}
	if m.Priced && !opts.Force {
		out.Outcome = ImportKept
		out.Detail = "already priced; --force overwrites prices nobody has verified"
		return out, nil
	}
	if len(routes) == 0 {
		out.Outcome = ImportSkipped
		out.Detail = "no enabled route on an enabled provider, so the upstream model is unknown"
		return out, nil
	}

	ref, detail := resolveReference(opts, routes)
	if ref == nil {
		out.Outcome, out.Detail = ImportSkipped, detail
		return out, nil
	}

	rates, err := referenceRatesNano(ref)
	if err != nil {
		out.Outcome, out.Detail = ImportSkipped, err.Error()
		return out, nil
	}
	if m.Priced && sameStoredRates(m, rates) {
		out.Outcome = ImportUnchanged
		out.Detail = "the stored price already equals " + ref.Provider + "/" + ref.ModelKey
		return out, nil
	}

	dropped, err := s.writeReferencePrice(ctx, opts, m, ref)
	if err != nil {
		return out, fmt.Errorf("pricing import: %s: %w", m.ModelSlug, err)
	}
	out.Outcome = ImportPriced
	if m.Priced {
		out.Outcome = ImportUpdated
	}
	out.Detail = describeReference(ref) + describeDropped(dropped)
	return out, nil
}

// describeDropped names the advanced rates the write removed, and says why they
// had to go. Silence here would be the whole defect: an operator reading
// "unverified price replaced" has no way to know that a per-dimension rate they
// entered by hand went with it.
func describeDropped(d pricing.ReplacedRates) string {
	var parts []string
	if d.Dimensions > 0 {
		parts = append(parts, plural(d.Dimensions, "advanced rate", "advanced rates"))
	}
	if d.Tools > 0 {
		parts = append(parts, plural(d.Tools, "tool rate", "tool rates"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; dropped " + strings.Join(parts, " and ") +
		", which were set against the base rate this replaced"
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// refdataSlugFor is the name the price dataset knows this provider's platform
// by.
//
// The vendor answers this directly when the registry records an id for it: a
// provider slug is whatever the operator typed, and "my-claude" resolves against
// nothing while its vendor says plainly that it is Anthropic. Falling back to
// the slug keeps the previous behaviour for the custom vendor and for platforms
// the dataset does not carry, where the base-URL pass is the one that answers.
//
// A vendor whose dataset split depends on the endpoint (one id per region) is
// deliberately recorded with no id, so this returns the slug and the base URL
// decides -- which is the only signal that can tell those apart.
func refdataSlugFor(r pricing.UsableRoute) string {
	if v, ok := catalog.LookupVendor(r.ProviderVendor); ok && v.RefdataProvider != "" {
		return v.RefdataProvider
	}
	return r.ProviderSlug
}

// resolveReference reads the model's routes and returns the one reference they
// agree on, or the reason there is none.
//
// Several routes for one model is ordinary -- the same model served by two
// providers -- and they normally resolve to the same vendor rates. When they do
// not, that is two different upstream costs for one stored number, and there is
// no defensible way to pick: the disagreement is reported instead.
func resolveReference(
	opts ImportOptions, routes []pricing.UsableRoute,
) (*refdata.Reference, string) {
	var chosen *refdata.Reference
	var chosenRoute string
	var reasons []string
	seen := map[string]bool{}
	for _, r := range routes {
		scope := opts.Data.Scope(refdataSlugFor(r), r.BaseURL)
		res := opts.Data.Lookup(scope, r.ProviderModelID, opts.Now)
		if res.Outcome != refdata.Matched {
			reason := fmt.Sprintf("route %s on %s: %s (%s)",
				r.ProviderModelID, r.ProviderSlug, res.Outcome, res.Detail)
			if !seen[reason] {
				seen[reason], reasons = true, append(reasons, reason)
			}
			continue
		}
		if chosen == nil {
			chosen, chosenRoute = res.Ref, r.ProviderSlug
			continue
		}
		if chosen.Rates != res.Ref.Rates {
			return nil, fmt.Sprintf(
				"routes disagree on the upstream price: %s says %s/%s, %s says %s/%s",
				chosenRoute, chosen.Provider, chosen.ModelKey,
				r.ProviderSlug, res.Ref.Provider, res.Ref.ModelKey)
		}
	}
	if chosen == nil {
		return nil, joinReasons(reasons)
	}
	return chosen, ""
}

func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return "no usable route to read an upstream model id from"
	}
	return strings.Join(reasons, "; ")
}

// nanoRates is the reference converted to what the database stores.
type nanoRates struct {
	in, out, cacheRead, cacheWrite int64
	free                           bool
}

func referenceRatesNano(ref *refdata.Reference) (nanoRates, error) {
	var n nanoRates
	for _, f := range []struct {
		name string
		src  string
		dst  *int64
	}{
		{"input", ref.Rates.Input, &n.in},
		{"output", ref.Rates.Output, &n.out},
		{"cache_read", ref.Rates.CacheRead, &n.cacheRead},
		{"cache_write", ref.Rates.CacheWrite, &n.cacheWrite},
	} {
		v, err := parseConfigurableUSDPerMToNano(f.src)
		if err != nil {
			return nanoRates{}, fmt.Errorf("%s/%s: %s: %w", ref.Provider, ref.ModelKey, f.name, err)
		}
		*f.dst = v
	}
	// All four at zero is a model the vendor gives away, and that has to be
	// stored as free rather than as a paid model priced at nothing: keeping
	// those two apart is why the price row refuses the second shape at all.
	n.free = n.in == 0 && n.out == 0 && n.cacheRead == 0 && n.cacheWrite == 0
	return n, nil
}

func sameStoredRates(m pricing.ImportCandidate, want nanoRates) bool {
	mode := "paid"
	if want.free {
		mode = "free"
	}
	return string(m.BillingMode) == mode &&
		m.Official.Input.Set && m.Official.Input.Nano == want.in &&
		m.Official.Output.Set && m.Official.Output.Nano == want.out &&
		m.Official.CacheRead.Set && m.Official.CacheRead.Nano == want.cacheRead &&
		m.Official.CacheWrite.Set && m.Official.CacheWrite.Nano == want.cacheWrite
}

func (s *pgPricingAdminService) writeReferencePrice(
	ctx context.Context, opts ImportOptions,
	m pricing.ImportCandidate, ref *refdata.Reference,
) (pricing.ReplacedRates, error) {
	var dropped pricing.ReplacedRates
	rates, err := referenceRatesNano(ref)
	if err != nil {
		return dropped, err
	}
	mode := ModelPricingInputBillingModePaid
	if rates.free {
		mode = ModelPricingInputBillingModeFree
	}
	acked := append([]PricingRiskCode(nil), importAcknowledged...)
	// The whole rate set is replaced, not just the four base rates.
	//
	// A dimension rate is a rate for one slice of one bucket, and an operator
	// enters it in proportion to the base rate it sits next to -- twice the base
	// above 200k, half the base for batch. Replace the base rate and leave those
	// rows behind and the model keeps multiplying by a number that no longer
	// exists anywhere: a base moved from 99 to 3 USD/M leaves a long-context band
	// still charging 198, a step of sixty-six times rather than two. Empty sets
	// rather than nil are what say "replace them"; nil means "leave them alone",
	// which is what the console editor sends when it is only touching the base
	// rates.
	//
	// The dataset has no opinion about any of these dimensions, so there is
	// nothing to put back. Dropping them leaves the model priced by the reference
	// alone, which is a state the operator can see and re-enter; keeping them
	// leaves an invented one nobody chose.
	noDimensions := []ModelPriceDimensionRate{}
	noTools := []ModelPriceToolRate{}
	in := ModelPricingInput{
		BillingMode: mode,
		OfficialRates: DraftTokenRatesUSDPerM{
			Input: literalPtr(ref.Rates.Input), Output: literalPtr(ref.Rates.Output),
			CacheRead: literalPtr(ref.Rates.CacheRead), CacheWrite: literalPtr(ref.Rates.CacheWrite),
		},
		DimensionRates: &noDimensions,
		ToolRates:      &noTools,
		// The sales multiplier is a commercial decision and no dataset has an
		// opinion about it. An existing one is carried over untouched; a new row
		// gets the column's own default, which is "charge the list price".
		Adjustment:        PricingAdjustment{MultiplierBps: int(m.MultiplierBps)},
		SourceName:        opts.Data.Snapshot.Dataset,
		SourceUrl:         literalPtr(opts.Data.Snapshot.SourceURL),
		AcknowledgedRisks: &acked,
	}
	prov := importProvenance{
		Maintenance:    "reference-import",
		Dataset:        opts.Data.Snapshot.Dataset,
		SnapshotDate:   opts.Data.Snapshot.SnapshotDate,
		Digest:         opts.Data.Snapshot.SHA256,
		UpstreamCommit: opts.Data.Snapshot.UpstreamCommit,
		ProviderKey:    ref.Provider,
		ModelKey:       ref.ModelKey,
		MatchedBy:      ref.MatchedBy,
		Defaulted:      ref.Defaulted,
		DefaultedMeans: defaultedMeans(ref.Defaulted),
		Rounded:        ref.Rounded,
		ContextTiered:  ref.ContextTiered,
	}
	in.Reason = "reference price import: " + describeReference(ref)
	provenance, err := json.Marshal(prov)
	if err != nil {
		return dropped, fmt.Errorf("record where the price came from: %w", err)
	}
	write, err := modelPricingWriteFromDTO(in)
	if err != nil {
		return dropped, err
	}
	// No Expected: a bulk run holds no earlier read of this row to be stale
	// against, and its own rules -- never touch a verified price, touch an
	// unverified one only under --force -- are what decide instead.
	//
	// Actor is whoever asked for the run, if anybody did: a request carries a
	// signed-in identity, a command line inside a container does not, and the
	// blank is the honest record of the second case.
	write.Actor = opts.Actor
	// Never a verification date, whichever of the two ran it. That field means a
	// person compared this number against the vendor's own list, and neither
	// pressing a button nor typing a command is that act. It is also the only
	// thing the console's "unverified" marker rests on, so filling it in here
	// would erase the distinction permanently.
	write.VerifiedAt = pgtype.Timestamptz{}
	write.Provenance = provenance
	write.DryRun = opts.DryRun
	// What the write removed is stamped onto the row that removed it, in the
	// same transaction. A count that lives only in terminal output survives as
	// long as the scrollback, and the question it answers -- "where did my
	// long-context band go" -- is asked much later than that.
	write.AfterReplace = func(ctx context.Context, tx pgx.Tx, r pricing.ReplacedRates) error {
		dropped = r
		if r.Dimensions == 0 && r.Tools == 0 {
			return nil
		}
		prov.DroppedDimensionRates, prov.DroppedToolRates = r.Dimensions, r.Tools
		stamped, mErr := json.Marshal(prov)
		if mErr != nil {
			return fmt.Errorf("record the replaced rates: %w", mErr)
		}
		if _, eErr := tx.Exec(ctx,
			`UPDATE model_pricing SET provenance=$2, reason=$3 WHERE model_id=$1`,
			m.ModelID, stamped, in.Reason+describeDropped(r)); eErr != nil {
			return fmt.Errorf("record the replaced rates: %w", eErr)
		}
		return nil
	}
	_, _, err = s.saveModelPricing(ctx, m.ModelID, in, write)
	return dropped, err
}

// importProvenance is what gets stored alongside each imported price. It is the
// answer to "where did this number come from", which is the question nobody can
// answer afterwards unless it was written down at the time.
type importProvenance struct {
	Maintenance  string `json:"maintenance"`
	Dataset      string `json:"dataset"`
	SnapshotDate string `json:"snapshot_date"`
	// Digest pins the exact bytes the rates were read from. The dataset name
	// and date say which release; this says which file, which is the only one
	// of the three that cannot be true of two different sets of numbers.
	Digest         string `json:"sha256,omitempty"`
	UpstreamCommit string `json:"upstream_commit,omitempty"`
	ProviderKey    string `json:"provider_key"`
	ModelKey       string `json:"model_key"`
	MatchedBy      string `json:"matched_by"`
	// Defaulted names the buckets the dataset does not price, stored as an
	// explicit zero because a price row has to be complete.
	Defaulted []string `json:"defaulted_buckets,omitempty"`
	// DefaultedMeans says what that zero does at billing time, on the row
	// itself.
	//
	// The list above is a fact about the dataset; on its own it does not say
	// that those tokens are now charged nothing, and a zero on a cache bucket is
	// exactly where that matters. Cached input is subtracted out of the ordinary
	// input count before pricing, so a zero cache-read rate is not "billed as
	// input" -- it is billed as nothing at all. The person reading this row
	// later is asking about a bill, so the row answers in those terms.
	DefaultedMeans string `json:"defaulted_buckets_effect,omitempty"`
	// Rounded names the buckets whose dataset value carried more precision than
	// the database stores.
	Rounded []string `json:"rounded_buckets,omitempty"`
	// ContextTiered records that the dataset prices this model in steps by
	// input size and only the base step was stored.
	ContextTiered bool `json:"context_tiers_dropped,omitempty"`
	// DroppedDimensionRates and DroppedToolRates count the advanced rates this
	// write removed, because they were entered against the base rate it
	// replaced. A rate per dimension is set in proportion to the base rate next
	// to it; carrying one over onto a new base multiplies by a number that no
	// longer exists.
	DroppedDimensionRates int64 `json:"dropped_dimension_rates,omitempty"`
	DroppedToolRates      int64 `json:"dropped_tool_rates,omitempty"`
}

// defaultedMeans states, for the row itself, what a defaulted zero costs the
// customer. Empty when nothing was defaulted, so the field only appears where
// it says something.
func defaultedMeans(defaulted []string) string {
	if len(defaulted) == 0 {
		return ""
	}
	return "the reference prices none of these buckets: " + shortBuckets(defaulted) +
		". A zero was stored for each, so usage on them is charged nothing until " +
		"somebody enters a rate"
}

func describeReference(ref *refdata.Reference) string {
	out := fmt.Sprintf("%s/%s matched by %s", ref.Provider, ref.ModelKey, ref.MatchedBy)
	var notes []string
	if len(ref.Defaulted) > 0 {
		// Not "zero written for cache_read", which reads as bookkeeping. A zero
		// on a token bucket is a price, and the price it is happens to be free:
		// cached input is subtracted out of the ordinary input count before
		// billing, so those tokens are not charged as input either. Most of these
		// zeroes are right -- an embedding model really has no output tokens --
		// but the ones that are not are silently free, and the only way an
		// operator finds out is by being told which buckets they are.
		notes = append(notes, "charged nothing on "+shortBuckets(ref.Defaulted)+
			", which the reference does not price")
	}
	if len(ref.Rounded) > 0 {
		notes = append(notes, "rounded "+shortBuckets(ref.Rounded))
	}
	if ref.ContextTiered {
		notes = append(notes, "base rate only, the reference also prices larger inputs higher")
	}
	for i, n := range notes {
		if i == 0 {
			out += " ("
		} else {
			out += ", "
		}
		out += n
	}
	if len(notes) > 0 {
		out += ")"
	}
	return out
}

// shortBuckets drops the dataset's `_mtok` suffix, which says nothing a reader
// of this line does not already know.
func shortBuckets(in []string) string {
	out := make([]string, 0, len(in))
	for _, b := range in {
		out = append(out, strings.TrimSuffix(b, "_mtok"))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// literalPtr differs from strPtr next door, and the difference is load-bearing
// here: strPtr maps the empty string to nil, while a rate of "0" has to survive
// as a pointer to "0". Absent and zero are different prices.
func literalPtr(s string) *string { return &s }
