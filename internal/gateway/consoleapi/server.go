// Package gwconsoleapi implements the gateway's half of the organization console API.
//
// The generated interface comes from the usage-tagged section of the console
// spec. Usage and the model catalog are described in tokens, models and
// providers -- gateway concepts, and nothing outside the gateway should have to
// know those shapes. Margin goes the other way: it is a billing concept the
// gateway supplies data for, so it travels through an injected interface rather
// than through this API. The two mechanisms solve two different problems and
// should not be confused for each other.
package gwconsoleapi

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/fairlb/fairlb/foundation/strutil"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/foundation/money"
	"github.com/fairlb/fairlb/foundation/publicid"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/orgscope"
	"github.com/fairlb/fairlb/internal/gateway/tiers"
	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
)

// maxUsageRange caps the query period. Without a cap, one "show me everything"
// scans every partition, and the rollups are retained for years -- the console's
// charts do not need history at that resolution.
const maxUsageRange = 400 * 24 * time.Hour

// Server implements the generated StrictServerInterface.
type Server struct {
	pool *pgxpool.Pool
	q    *gwdb.Queries
	// authz decides whether the caller may read this org's data. It is injected
	// because org membership is not a gateway concept.
	scope *orgscope.Runner
	// box encrypts and decrypts organization-supplied upstream credentials, with the
	// associated data bound to the row id.
	box *crypto.Box
	// probeClient serves the BYOK connectivity test; nil means a default client
	// with a timeout. Tests inject their own.
	probeClient *http.Client
	// cat is the only way catalog and pricing are read. The organization catalog once
	// carried its own SQL that read price columns off the model row and never
	// applied the plan multiplier -- the same mechanism as a defect in the model
	// discovery classifier.
	// It is a required constructor argument rather than a WithXxx injection:
	// forgetting the injection would be a nil dereference at runtime, and an
	// invariant belongs to the value itself -- complete from construction.
	cat *catalog.Service
}

// ServerConfig is the complete construction input for the gateway console
// API. Production code uses it instead of mutating BYOK dependencies later.
type ServerConfig struct {
	Pool               *pgxpool.Pool
	OrganizationAccess orgscope.Authorizer
	Catalog            *catalog.Service
	Cipher             *crypto.Box
	ProbeClient        *http.Client
}

// OrgAuthorizer decides the caller's read and write access to an org.
//
// Org membership and roles are not gateway concepts, so the decision is
// injected at the assembly point. Same seam as the dataplane's authentication
// boundary: the gateway does gateway work, identity belongs elsewhere.
//
// Ordinary operational facts, sensitive configuration reads, and writes are
// three separate methods because their role gates and their org-status
// semantics differ. Squeezing them into one call with a bool parameter makes it
// very easy to wire a new endpoint to the wrong one.
// NewServer constructs a fully wired console server.
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		pool: cfg.Pool, q: gwdb.New(cfg.Pool),
		scope: orgscope.New(cfg.Pool, cfg.OrganizationAccess),
		cat:   cfg.Catalog, box: cfg.Cipher, probeClient: cfg.ProbeClient,
	}
}

var _ StrictServerInterface = (*Server)(nil)

// orgReadAccess and orgReadRequirements are this package's names for the
// runner's, so the handler signatures read in the contract's vocabulary.
type orgReadAccess = orgscope.Access

type orgReadRequirements = orgscope.Requirements

// The three wrappers below name what each endpoint group is allowed to do; the
// runner they delegate to is internal/gateway/orgscope (ADR-0182).

// scopeAdminRead serves the read-only view of sensitive configuration such as
// BYOK: admin or above, but a suspended org can still read.
func (s *Server) scopeAdminRead(ctx context.Context, orgPublicID OrgID, fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error) error {
	return s.scope.AdminRead(ctx, string(orgPublicID), fn)
}

// scopeWrite is the write path: admin or above, and the org's status permits
// writes.
//
// The gateway's only write endpoints here are the three BYOK ones: create,
// delete and connectivity test. The connectivity test counts as a write -- it
// sends a real request upstream, which costs money, and writes the result back
// onto the credential row.
func (s *Server) scopeWrite(ctx context.Context, orgPublicID OrgID, fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID) error) error {
	return s.scope.Write(ctx, string(orgPublicID), fn)
}

// scopeRead is the member-facing read path, carrying the sensitive dimensions
// the caller resolved to.
func (s *Server) scopeRead(
	ctx context.Context, orgPublicID OrgID, require orgReadRequirements,
	fn func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, access orgReadAccess) error,
) error {
	return s.scope.Read(ctx, string(orgPublicID), require, fn)
}

// reportQuery turns the request parameters into the read model's question.
//
// normalizeTZ belongs here rather than in the domain: the name comes from the
// browser and is therefore user-controlled input. An empty string makes
// PostgreSQL's `timezone` raise "invalid time zone name" outright, and an
// unrecognised name would turn a whole page into a 500 -- so both fall back to
// UTC, and day boundaries degrade to plain UTC instead.
func reportQuery(
	from, to time.Time, key pgtype.UUID, gran *string, groupBy *string, tz *string,
) gwusage.ReportQuery {
	// Not named `q`: in a handler package that identifier means the sqlc query
	// set, and check-handler-purity keys on it.
	out := gwusage.ReportQuery{
		From: from, To: to, APIKeyID: key,
		Granularity: gwusage.Daily, Timezone: normalizeTZ(tz),
	}
	if gran != nil && *gran == string(gwusage.Hourly) {
		out.Granularity = gwusage.Hourly
	}
	if groupBy != nil {
		out.GroupBy = gwusage.GroupBy(*groupBy)
	}
	return out
}

func normalizeTZ(tz *string) string {
	if tz == nil || *tz == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(*tz); err != nil {
		return "UTC"
	}
	return *tz
}

func usageReportDTO(rep gwusage.Report, access orgReadAccess) UsageReport {
	out := UsageReport{Series: make([]UsagePoint, 0, len(rep.Series))}
	for _, p := range rep.Series {
		point := UsagePoint{
			BucketStart: p.BucketStart, Requests: p.Requests,
			TokensIn: p.TokensIn, TokensOut: p.TokensOut,
			ChargedNano: p.ChargedNano, Errors: &p.Errors,
		}
		if !access.Finance {
			point.ChargedNano = 0
		}
		out.Series = append(out.Series, point)
	}
	out.Totals = UsageTotals{
		Requests: rep.Totals.Requests, TokensIn: rep.Totals.TokensIn,
		TokensOut: rep.Totals.TokensOut, ChargedNano: rep.Totals.ChargedNano,
		Errors: &rep.Totals.Errors, Currency: rep.Totals.Currency,
	}
	if !access.Finance {
		out.Totals.ChargedNano, out.Totals.Currency = 0, ""
	}
	lat := UsageLatency{HasSamples: rep.Totals.Latency.HasSamples}
	if lat.HasSamples {
		st := rep.Totals.Latency
		p50, p95, mean := int(st.P50), int(st.P95), int(st.Mean)
		lat.P50Ms, lat.P95Ms, lat.MeanMs = &p50, &p95, &mean
		lat.P95Unbounded = &st.P95Unbounded
	}
	out.Totals.Latency = &lat

	if rep.Groups != nil {
		groups := make([]UsageGroup, 0, len(rep.Groups))
		for _, g := range rep.Groups {
			label := g.Label
			key := g.Key
			if g.RawKey.Valid {
				key = publicid.Format(publicid.Key, g.RawKey)
			}
			group := UsageGroup{
				Key: key, Label: &label, Requests: g.Requests,
				TokensIn: g.TokensIn, TokensOut: g.TokensOut, ChargedNano: g.ChargedNano,
			}
			if !access.Finance {
				group.ChargedNano = 0
			}
			groups = append(groups, group)
		}
		// Without the finance dimension the spend ordering the query applied is
		// invisible and looks arbitrary, so re-sort by what the caller can see.
		if !access.Finance {
			sort.Slice(groups, func(i, j int) bool {
				if groups[i].Requests != groups[j].Requests {
					return groups[i].Requests > groups[j].Requests
				}
				return groups[i].Key < groups[j].Key
			})
		}
		out.Groups = &groups
	}
	return out
}

func (s *Server) GetUsage(ctx context.Context, req GetUsageRequestObject) (GetUsageResponseObject, error) {
	from, to, err := validRange(req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	key, err := optKeyID(req.Params.ApiKeyId)
	if err != nil {
		return nil, err
	}
	query := reportQuery(from, to, key,
		(*string)(req.Params.Granularity), (*string)(req.Params.GroupBy), req.Params.Tz)

	var rep gwusage.Report
	require := orgReadRequirements{KeyMetadata: key.Valid || query.GroupBy == gwusage.ByAPIKey}
	access := orgReadAccess{}
	err = s.scopeRead(ctx, req.OrgId, require, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, resolved orgReadAccess) error {
		access = resolved
		var rErr error
		rep, rErr = gwusage.NewReportReader(q).Report(ctx, org, query)
		return rErr
	})
	if err != nil {
		return nil, err
	}
	return GetUsage200JSONResponse(usageReportDTO(rep, access)), nil
}

// ExportUsageCSV exports a CSV on exactly the same basis as the usage endpoint.
//
// Sharing the basis is deliberate: the chart and the export come from the same
// queries, so cross-checking one against the other never turns up different
// numbers for definitional reasons. That kind of inconsistency costs more trust
// than not having an export at all.
func (s *Server) ExportUsageCSV(ctx context.Context, req ExportUsageCSVRequestObject) (ExportUsageCSVResponseObject, error) {
	from, to, err := validRange(req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	// Both tz and api_key_id take the same parameters as the usage endpoint.
	// The endpoint's own summary promises the same basis and the same filters,
	// and the key filter was once simply not wired up -- so filtering to one key
	// and exporting gave you everything.
	key, err := optKeyID(req.Params.ApiKeyId)
	if err != nil {
		return nil, err
	}
	query := reportQuery(from, to, key, (*string)(req.Params.Granularity), nil, req.Params.Tz)

	var rep gwusage.Report
	require := orgReadRequirements{Finance: true, KeyMetadata: key.Valid}
	err = s.scopeRead(ctx, req.OrgId, require, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, _ orgReadAccess) error {
		var rErr error
		rep, rErr = gwusage.NewReportReader(q).Report(ctx, org, query)
		return rErr
	})
	if err != nil {
		return nil, err
	}
	// The export always carries finance: the requirement above refuses the
	// caller outright when they may not see it, so there is nothing to redact.
	return csvResponse{report: usageReportDTO(rep, orgReadAccess{Finance: true, KeyMetadata: true})}, nil
}

// csvResponse writes the CSV out directly; the generated strict server does not
// handle non-JSON bodies.
type csvResponse struct{ report UsageReport }

func (r csvResponse) VisitExportUsageCSVResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gwusage.csv"`)
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"bucket_start", "requests", "tokens_in", "tokens_out",
		"charged", "charged_nano", "currency", "errors",
	}); err != nil {
		return err
	}
	for _, p := range r.report.Series {
		errs := int64(0)
		if p.Errors != nil {
			errs = *p.Errors
		}
		if err := cw.Write([]string{
			p.BucketStart.UTC().Format(time.RFC3339),
			strconv.FormatInt(p.Requests, 10),
			strconv.FormatInt(p.TokensIn, 10),
			strconv.FormatInt(p.TokensOut, 10),
			// Both the major unit and the raw nano value: the first for
			// people to read, the second so a machine can recompute exactly.
			money.FormatNano(p.ChargedNano),
			strconv.FormatInt(p.ChargedNano, 10),
			r.report.Totals.Currency,
			strconv.FormatInt(errs, 10),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ListAvailableModels returns the model catalog a organization can use.
//
// It is filtered by the organization's effective admission tier, on the same basis as
// the dataplane resolves it: "visible in the catalog, 404 on call" is exactly
// the kind of inconsistency that turns into a support ticket.
//
// orgCatalogArgs carries what was read inside the org transaction back out,
// so the catalog query can run outside it.
type orgCatalogArgs struct {
	tierID, org    pgtype.UUID
	includeFinance bool
}

func (s *Server) ListAvailableModels(ctx context.Context, req ListAvailableModelsRequestObject) (ListAvailableModelsResponseObject, error) {
	data := []AvailableModel{}
	var catalogArgs *orgCatalogArgs
	err := s.scopeRead(ctx, req.OrgId, orgReadRequirements{}, func(ctx context.Context, q *gwdb.Queries, org pgtype.UUID, access orgReadAccess) error {
		tier, tErr := tiers.EffectiveAdmission(ctx, q, org)
		if tErr != nil {
			return tErr
		}
		// A disabled tier means the dataplane refuses this organization everything,
		// and it says so with a 403 naming the tier. This surface answers the
		// same way rather than returning an empty list.
		//
		// An empty list would be a different sentence -- "this deployment has
		// no models" -- and the organization would go looking for a fault that is not
		// there, while the one fact that would resolve it in a sentence (their
		// access configuration is not in effect) stayed hidden. The two
		// catalogue surfaces have to agree; they used to disagree here.
		if !tier.Valid || tier.TierStatus != "active" {
			return httpx.ErrCodeDetail(errcode.GatewayModelTierDisabled,
				"Your model access tier is disabled; contact support")
		}
		// Same source as the dataplane's model listing: go through the catalog
		// service rather than writing a second copy of the SQL. This code once
		// read price columns off the model row, where newer models are always
		// zero -- so the organization catalog showed all four prices as zero, while
		// migrated models showed their pre-migration price with no plan
		// multiplier applied at all. Every surface that reads a price has to
		// read it the same way.
		//
		// The catalog query is deliberately left until after the transaction
		// (see the end of this function): resolving models reads settings on
		// the catalog service's own connection, and the enclosing org-scoped
		// transaction already holds one. Asking for a second from inside it
		// deadlocks against itself on a small pool. The dataplane's model
		// listing calls it outside a transaction for the same reason.
		// TestFinanceAuthorizationDoesNotReacquireInsideOrgTx witnesses this;
		// the same shape has bitten the route protocol check before.
		catalogArgs = &orgCatalogArgs{
			tierID: pgtype.UUID{Bytes: tier.TierID, Valid: true},
			org:    org, includeFinance: access.Finance,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if catalogArgs != nil {
		rows, lErr := s.cat.ModelsForOrg(ctx, catalogArgs.tierID, catalogArgs.org)
		if lErr != nil {
			return nil, fmt.Errorf("gwconsole: list models: %w", lErr)
		}
		includeFinance := catalogArgs.includeFinance
		data = make([]AvailableModel, 0, len(rows))
		for _, r := range rows {
			m := AvailableModel{
				Slug: r.Slug, Protocols: r.Protocols, Endpoints: r.Endpoints,
				DisplayName: strutil.Ptr(r.DisplayName),
			}
			cw, mo := int(r.ContextWindow), int(r.MaxOutputTokens)
			m.ContextWindow, m.MaxOutputTokens = &cw, &mo
			// capabilities is presentation metadata (vision, tools, ...) and is
			// a different thing from endpoints: endpoints answer "which URL do
			// I send to", capabilities answer "can this model look at an
			// image". The console used to display endpoints as capabilities,
			// because this column was never surfaced to organizations -- admins
			// could edit it and the dataplane listing returned it, and only
			// the console could not see it.
			if caps := r.Capabilities; len(caps) > 0 {
				if decoded := decodeCapabilities(caps); decoded != nil {
					m.Capabilities = decoded
				}
			}
			// Two criteria stack here, and neither can be dropped:
			//   1. includeFinance is the financial visibility gate. A plain
			//      member should not even receive the free/paid flag -- that
			//      is also a conclusion about money;
			//   2. what comes out of the gate is the final unit price as this
			//      organization sees it (list price x model multiplier x plan
			//      multiplier), through the same conversion the dataplane
			//      listing uses. This code once read price columns off the
			//      model row, where newer models are always zero.
			// Dropping either is a regression: without the first, financial
			// detail leaks to members; without the second, all four prices
			// read zero.
			if includeFinance {
				m.IsFree = &r.IsFree
				rates := catalog.RatesForOrgModel(r, catalog.Rates{})
				in := catalog.OrgPriceNanoPerMTok(r.PriceIn, rates)
				out := catalog.OrgPriceNanoPerMTok(r.PriceOut, rates)
				cr := catalog.OrgPriceNanoPerMTok(r.PriceCacheRead, rates)
				cwr := catalog.OrgPriceNanoPerMTok(r.PriceCacheWrite, rates)
				// A free model reports zero for all four, so the list price
				// kept on the row is not disclosed.
				if r.IsFree {
					in, out, cr, cwr = 0, 0, 0, 0
				}
				m.PriceInNanoPerMtok = &in
				m.PriceOutNanoPerMtok = &out
				m.PriceCacheReadNanoPerMtok = &cr
				m.PriceCacheWriteNanoPerMtok = &cwr
			}
			data = append(data, m)
		}
	}

	// The catalog supports conditional requests: it changes on the order of
	// days, yet three separate pages each fetch it.
	var body ListAvailableModels200JSONResponse
	body.Body.Items = data
	etag, err := payloadETag(data)
	if err != nil {
		// Failing to compute an ETag must not take the catalog down; fall
		// back to a 200 without one.
		return body, nil
	}
	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == etag {
		return ListAvailableModels304Response{}, nil
	}
	body.Headers.ETag = &etag
	return body, nil
}

// payloadETag computes a strong validator over the serialised response body.
//
// It is not assembled from a row count or a max(updated_at). That kind of
// summary-of-a-summary raises no error when it misses a field; it just quietly
// hides the change behind a 304 -- and "the catalog changed but the organization
// cannot see it" is precisely the inconsistency to avoid here. The first 16
// bytes of a sha256 give a collision probability proportionate to the use.
func payloadETag(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}

// decodeCapabilities decodes the raw jsonb into an object; both an empty object
// and a decode failure return nil.
//
// An empty object returns nil rather than `{}` because the frontend reads nil
// as "this model has no capabilities recorded yet", and in JSON an empty object
// looks identical to "recorded, and all of them false".
// A decode failure also returns nil: this column is presentation metadata, and
// broken metadata should not turn the whole catalog into a 500.
func decodeCapabilities(raw []byte) *map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return &m
}

// validRange validates the query period.
func validRange(from, to time.Time) (time.Time, time.Time, error) {
	if !to.After(from) {
		return time.Time{}, time.Time{}, httpx.ErrCodeDetail(errcode.CommonValidation, "to must be later than from")
	}
	if to.Sub(from) > maxUsageRange {
		return time.Time{}, time.Time{}, httpx.ErrCodeDetail(errcode.CommonValidation,
			"The requested range is too long; the maximum is 400 days")
	}
	return from.UTC(), to.UTC(), nil
}

// optKeyID parses the optional key filter. An empty string means no filter --
// that is what the frontend sends when the select is cleared.
func optKeyID(raw *string) (pgtype.UUID, error) {
	if raw == nil || *raw == "" {
		return pgtype.UUID{}, nil
	}
	id, err := publicid.Parse(publicid.Key, *raw)
	if err != nil {
		return pgtype.UUID{}, httpx.ErrCodeDetail(errcode.CommonValidation, "Invalid api_key_id")
	}
	return id, nil
}
