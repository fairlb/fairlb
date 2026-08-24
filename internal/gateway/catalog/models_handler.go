package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/money"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// PublicModelsHandler serves GET /v1/public/models: the unauthenticated price
// list.
//
// It is a different thing from the authenticated /v1/models, hence a separate
// endpoint. This one answers "what is offered and at what published price",
// for a marketing site or documentation to consume; that one answers "what can
// this key call, and at what rate for you". Collapsing the two meanings onto
// one path is a lasting source of ambiguity, and OpenRouter separates them the
// same way.
//
// The authenticated view's handler lives in the proxy layer, because that is
// where authentication and the two protocols' error rendering are.
//
// The price it publishes is the *list* price: the upstream's official rate
// times the model's own multiplier times the default plan's. That equality is
// the point -- the anonymous catalog is what a reader who signs up right now
// will be charged, not a number nobody is on. A customer-specific plan never
// reaches here; it is resolved from an org id this endpoint does not have.
//
// A paid model also publishes the upstream's own rate as official_price, which
// does publish the margin on that model. That is deliberate rather than an
// oversight: "the same price the vendor charges" is only a claim a reader can
// check if both numbers are in front of them. A free model omits it, so the
// retained official rate stays undisclosed.
func (s *Service) PublicModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		models, err := s.PublicModels(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "failed to read the public catalog", "error", err)
			writeOpenAIError(w, http.StatusInternalServerError, "internal", "Internal server error")
			return
		}
		// Both multipliers are already resolved on each row -- the model's
		// own from its price row, the default plan's from the query -- so the
		// base table carries nothing but the fixed exchange rate.
		s.WriteModelList(ctx, w, models, Rates{FXRate: "1"})
	}
}

// WriteModelList renders the catalog response.
//
// It is exported so the authenticated handler in the proxy layer reuses the
// same rendering: the two endpoints must produce an identical response shape --
// a client should be able to switch base_url and carry on -- and a second copy
// of the rendering will eventually drift. The rendering stays here rather than
// moving to the proxy because the shape and the price conversion are the
// catalog's own domain knowledge.
func (s *Service) WriteModelList(
	ctx context.Context, w http.ResponseWriter, models []PublicModel, rates Rates,
) {
	// The deployment's own commercial terms travel with the price list rather
	// than in a second endpoint: a client that renders "here is the rate, and
	// here is what we add on top of your own key" needs both, and two fetches
	// are two chances for them to disagree about which moment they describe.
	//
	// The fee is quoted the same way the prices are -- at the list plan -- so
	// "5% on top of the vendor rate" and the rates beside it are answers to the
	// same question. An organization on a negotiated plan gets its own figure
	// from the authenticated catalog.
	list := modelList{
		Object: "list",
		Data:   make([]modelEntry, 0, len(models)),
		Meta:   listMeta{BYOKFeeBps: multiplyBps(s.settings.BYOKFeeBps(ctx), planOf(models))},
	}
	for _, m := range models {
		modelRates := RatesForOrgModel(m, rates)
		e := modelEntry{
			ID:      m.Slug,
			Object:  "model",
			Name:    m.DisplayName,
			OwnedBy: ownerOf(m.Slug),
			Pricing: pricing{Currency: m.Currency},
			Meta: modelMeta{
				BillingMode:         map[bool]string{true: "free", false: "paid"}[m.IsFree],
				ContextWindow:       m.ContextWindow,
				MaxOutputTokens:     m.MaxOutputTokens,
				Endpoints:           m.Endpoints,
				Protocols:           m.Protocols,
				SupportedOperations: m.Endpoints,
				Capabilities:        m.Capabilities,
				PricingPlanID:       uuidStr(m.PricingPlanID),
				PriceUpdatedAt:      timestampString(m.PriceUpdatedAt),
			},
		}
		if e.Meta.Endpoints == nil {
			e.Meta.Endpoints = []string{}
		}
		if e.Meta.SupportedOperations == nil {
			e.Meta.SupportedOperations = []string{}
		}
		if !m.IsFree {
			e.Pricing.InputPerMTok = orgPricePerMTok(m.PriceIn, modelRates)
			e.Pricing.OutputPerMTok = orgPricePerMTok(m.PriceOut, modelRates)
			e.Pricing.CacheReadPerMTok = orgPricePerMTok(m.PriceCacheRead, modelRates)
			e.Pricing.CacheWritePerMTok = orgPricePerMTok(m.PriceCacheWrite, modelRates)
			e.Pricing.OfficialPrice = &officialPrice{
				InputPerMTok:      money.FormatNanoExact(max64(m.PriceIn, 0)),
				OutputPerMTok:     money.FormatNanoExact(max64(m.PriceOut, 0)),
				CacheReadPerMTok:  omitZeroDecimal(m.PriceCacheRead),
				CacheWritePerMTok: omitZeroDecimal(m.PriceCacheWrite),
				SourceName:        m.SourceName,
				SourceURL:         m.SourceURL,
				VerifiedAt:        timestampString(m.VerifiedAt),
			}
		} else {
			e.Pricing.InputPerMTok = "0"
			e.Pricing.OutputPerMTok = "0"
			e.Pricing.CacheReadPerMTok = "0"
			e.Pricing.CacheWritePerMTok = "0"
		}
		list.Data = append(list.Data, e)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(list); err != nil {
		slog.ErrorContext(ctx, "failed to write the catalog response", "error", err)
	}
}

// orgPricePerMTok converts a nano rate into the organization-facing decimal
// string, per million tokens in the major unit. A string rather than a float,
// consistent with never using floating point for money, and so that different
// client languages cannot disagree about the last decimal place.
func orgPricePerMTok(nanoPerMTok int64, rates Rates) string {
	return money.FormatNanoExact(OrgPriceNanoPerMTok(nanoPerMTok, rates))
}

// OrgPriceNanoPerMTok converts an upstream official rate into the final rate
// this organization sees -- official rate times the model multiplier times the plan
// multiplier -- in nano per million.
//
// It is exported so the console's organization catalog and the data plane's
// /v1/models share one conversion, including both current multipliers.
func OrgPriceNanoPerMTok(nanoPerMTok int64, rates Rates) int64 {
	if nanoPerMTok <= 0 {
		return 0
	}
	// The exchange rate is fixed at "1": the catalog's currency is carried by
	// PublicModel.Currency, and this only applies the multipliers, not a
	// currency conversion.
	rates.FXRate = "1"
	// The catalog shows the organization-facing price, which has nothing to do with
	// upstream cost, so the same table is passed for both arguments; the cost
	// figure Compute produces is unused here.
	listed := Price{InNanoPerMTok: nanoPerMTok}
	q, err := Compute(Flat(listed), Flat(listed), Tokens{In: tokensPerMTok}, rates)
	if err != nil {
		return 0
	}
	return q.ChargedNano
}

// RatesForOrgModel returns the multipliers in force for this model and this
// organization. The data plane and the console share this one function; writing it
// twice is how the two drift apart.
func RatesForOrgModel(m PublicModel, base Rates) Rates {
	r := base
	if m.ModelMultiplierBps > 0 {
		r.ModelMultiplierBps = m.ModelMultiplierBps
	}
	if m.PlanMultiplierBps > 0 {
		r.PlanMultiplierBps = m.PlanMultiplierBps
	}
	return r
}

func max64(v, floor int64) int64 {
	if v < floor {
		return floor
	}
	return v
}

// omitZeroDecimal renders a nullable rate: zero or negative produces the empty
// string, which omitempty then drops from the response.
func omitZeroDecimal(nano int64) string {
	if nano <= 0 {
		return ""
	}
	return money.FormatNanoExact(nano)
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
	Meta   listMeta     `json:"fairlb"`
}

// listMeta carries what is true of the deployment rather than of any one model.
// Nested under the same extension key the per-model block uses, so nothing here
// can collide with a field an upstream's own list response might grow.
type listMeta struct {
	// BYOKFeeBps is what this deployment charges on top of the vendor's rate
	// when the request runs on the caller's own provider key, in basis points
	// of that rate. 500 is 5%.
	BYOKFeeBps int64 `json:"byok_fee_bps"`
}

// planOf returns the plan multiplier the list is quoted at. One query resolves
// one plan, so every row carries the same default; a per-model override changes
// that model's rate, not which plan the reader is on, so it deliberately does
// not move the deployment-level fee.
func planOf(models []PublicModel) int64 {
	if len(models) == 0 {
		return noDiscountBps
	}
	return models[0].PlanDefaultMultiplierBps
}

// multiplyBps applies a plan multiplier to a rate expressed in basis points.
// A zero or negative multiplier means "none", consistent with Rates, so a
// missing assignment cannot multiply the fee to nothing.
func multiplyBps(bps, multiplierBps int64) int64 {
	if multiplierBps <= 0 {
		return bps
	}
	return (bps*multiplierBps + 5_000) / 10_000
}

type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	// name is required by the contract, so no omitempty; an empty value is
	// covered by the fallback to the slug.
	Name string `json:"name"`
	// OwnedBy is the model's creator: the first segment of a
	// `<creator>/<name>` slug. See ownerOf.
	OwnedBy string    `json:"owned_by,omitempty"`
	Pricing pricing   `json:"pricing"`
	Meta    modelMeta `json:"fairlb"`
}

type pricing struct {
	Currency          string `json:"currency"`
	InputPerMTok      string `json:"input_per_mtok"`
	OutputPerMTok     string `json:"output_per_mtok"`
	CacheReadPerMTok  string `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMTok string `json:"cache_write_per_mtok,omitempty"`
	// OfficialPrice is the upstream's own published rate in USD per million.
	// It is the comparison anchor that lets a client show "official $3, here
	// $2.55". It is omitted for a free model, so the retained official rate is
	// not disclosed.
	OfficialPrice *officialPrice `json:"official_price,omitempty"`
}

type officialPrice struct {
	InputPerMTok      string `json:"input_per_mtok"`
	OutputPerMTok     string `json:"output_per_mtok"`
	CacheReadPerMTok  string `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMTok string `json:"cache_write_per_mtok,omitempty"`
	// Where this rate came from, and whether a person confirmed it against the
	// vendor's own list. An absent verified_at is the whole difference between
	// "a reference dataset suggested this" and "somebody checked this" -- the
	// two facts the import deliberately keeps apart (see
	// docs/design/reference-prices.md). A client that shows the rate without
	// showing which of the two it is lets the reader assume the stronger one.
	SourceName string `json:"source_name,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

type modelMeta struct {
	BillingMode         string          `json:"billing_mode"`
	ContextWindow       int32           `json:"context_window,omitempty"`
	MaxOutputTokens     int32           `json:"max_output_tokens,omitempty"`
	Endpoints           []string        `json:"endpoints"`
	Protocols           []string        `json:"protocols"`
	SupportedOperations []string        `json:"supported_operations"`
	Capabilities        json.RawMessage `json:"capabilities,omitempty"`
	// There is no price version id and no engine mode: with a single set of
	// prices, such fields would be permanently empty or permanently constant,
	// and keeping them would tell clients there is more than one price to
	// choose between.
	PricingPlanID  string `json:"pricing_plan_id,omitempty"`
	PriceUpdatedAt string `json:"price_updated_at,omitempty"`
}

var uuidStr = publicid.UUIDString

func timestampString(v pgtype.Timestamptz) string {
	if !v.Valid {
		return ""
	}
	return v.Time.UTC().Format(time.RFC3339Nano)
}

// writeOpenAIError renders OpenAI's native error shape. The data plane is the
// one surface that does not use problem+json, because clients expect the
// upstream's own error format.
func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// ownerOf is the `owned_by` a catalog slug carries: the creator segment of a
// `<creator>/<name>` slug (`openai/gpt-5.6-sol` → `openai`). The creator is
// who trained the model, not which provider serves it — one slug can be
// routed through several providers. A bare slug with no `/` has no creator
// to report, and nothing else stands in for one -- a model owns no protocol
// -- so the field is omitted rather than filled with a guess.
func ownerOf(slug string) string {
	if i := strings.IndexByte(slug, '/'); i > 0 {
		return slug[:i]
	}
	return ""
}
