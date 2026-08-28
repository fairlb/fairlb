package refdata

import (
	"encoding/json"
	"fmt"
	"github.com/fairlb/fairlb/foundation/money"
	"math/big"
	"slices"
	"strings"
	"time"
)

// Storage granularity: the gateway keeps a rate as an integer count of
// billionths of a US dollar per million tokens. A dataset value carrying more
// precision than that has to be rounded to be stored at all, and the rounding
// is reported rather than performed quietly.
const storedFractionDigits = 9

// The bucket names the dataset uses for the four base rates the gateway
// stores, plus the two image rates it can store as dimensions.
//
// The dataset prices further dimensions -- audio, cached image reads, web
// searches -- which have no home here yet and are deliberately not read. The
// two image rates are read because there is somewhere to put them: an image
// model charges image input well above its text input rate, and a model that
// *generates* images reports the pixels as output tokens priced far above text
// output -- the dataset states 30 USD/Mtok against 2.5 for one Gemini image
// model, and 32 against 10 for gpt-image. Without these buckets both were
// billed at the text rate.
const (
	bucketInput       = "input_mtok"
	bucketOutput      = "output_mtok"
	bucketCacheRead   = "cache_read_mtok"
	bucketCacheWrite  = "cache_write_mtok"
	bucketInputImage  = "input_image_mtok"
	bucketOutputImage = "output_image_mtok"
)

// baseBuckets are the four a stored price must be complete in. Image input is
// not among them on purpose: a missing base bucket becomes an explicit zero,
// and an image rate of zero would be a claim the dataset never made.
var baseBuckets = [...]string{bucketInput, bucketOutput, bucketCacheRead, bucketCacheWrite}

// conditionalPrice is one link of the dataset's price chain: a set of rates,
// and the condition under which it applies.
type conditionalPrice struct {
	// startDate is set when this link only applies from a given day. The
	// dataset publishes announced price changes before they take effect, so a
	// chain read without a date returns a price that is not in force.
	startDate *time.Time
	// timeOfDay marks a link that only applies during part of the day. It is
	// carried so that resolution can refuse the model outright: one stored rate
	// cannot express two prices for the same tokens.
	timeOfDay bool
	rates     map[string]priceValue
}

// priceValue is one rate. `tiered` records that the dataset prices this bucket
// in steps by input size and only the base step is being read.
type priceValue struct {
	literal string
	tiered  bool
}

func parseConditionalPrices(raw json.RawMessage) ([]conditionalPrice, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("the entry carries no prices")
	}
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var links []struct {
			Constraint json.RawMessage            `json:"constraint"`
			Prices     map[string]json.RawMessage `json:"prices"`
		}
		if err := json.Unmarshal(raw, &links); err != nil {
			return nil, fmt.Errorf("read the price chain: %w", err)
		}
		out := make([]conditionalPrice, 0, len(links))
		for _, l := range links {
			link, err := parseConstraint(l.Constraint)
			if err != nil {
				return nil, err
			}
			if link.rates, err = parseRateSet(l.Prices); err != nil {
				return nil, err
			}
			out = append(out, link)
		}
		return out, nil
	}
	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("read the prices: %w", err)
	}
	rates, err := parseRateSet(flat)
	if err != nil {
		return nil, err
	}
	return []conditionalPrice{{rates: rates}}, nil
}

func parseConstraint(raw json.RawMessage) (conditionalPrice, error) {
	var link conditionalPrice
	if len(raw) == 0 || string(raw) == "null" {
		return link, nil
	}
	var c struct {
		StartDate string `json:"start_date"`
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return link, fmt.Errorf("read a price constraint: %w", err)
	}
	if c.StartTime != "" || c.EndTime != "" {
		link.timeOfDay = true
	}
	if c.StartDate != "" {
		d, err := time.Parse(time.DateOnly, c.StartDate)
		if err != nil {
			return link, fmt.Errorf("read a price start date: %w", err)
		}
		link.startDate = &d
	}
	return link, nil
}

func parseRateSet(in map[string]json.RawMessage) (map[string]priceValue, error) {
	out := make(map[string]priceValue, len(baseBuckets)+1)
	for _, bucket := range [...]string{
		bucketInput, bucketOutput, bucketCacheRead, bucketCacheWrite,
		bucketInputImage, bucketOutputImage,
	} {
		raw, ok := in[bucket]
		if !ok {
			continue
		}
		v, err := parsePriceValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", bucket, err)
		}
		out[bucket] = v
	}
	return out, nil
}

// parsePriceValue reads one rate, which the dataset states either as a plain
// number or as a base rate plus higher steps for larger inputs.
func parsePriceValue(raw json.RawMessage) (priceValue, error) {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		var tiered struct {
			Base json.Number `json:"base"`
		}
		if err := json.Unmarshal(raw, &tiered); err != nil {
			return priceValue{}, err
		}
		return priceValue{literal: tiered.Base.String(), tiered: true}, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return priceValue{}, err
	}
	return priceValue{literal: n.String()}, nil
}

// referenceFrom turns one dataset entry into the rates the gateway stores, or
// says why it cannot.
func referenceFrom(providerID string, m modelEntry, on time.Time, matchedBy string) Result {
	var chosen *conditionalPrice
	for i := range m.prices {
		link := &m.prices[i]
		if link.timeOfDay {
			// Refused for the whole entry, not just skipped past. Taking the
			// unconstrained link and ignoring the other one would store a rate
			// that is right for part of the day and wrong for the rest, with
			// nothing on the row to say so.
			return Result{
				Outcome: TimeOfDay,
				Detail:  providerID + "/" + m.id + " has a different rate at different times of day",
			}
		}
		if link.startDate != nil && on.Before(*link.startDate) {
			continue
		}
		chosen = link
	}
	if chosen == nil {
		return Result{
			Outcome: Unusable,
			Detail:  providerID + "/" + m.id + " has no rate in force on " + on.Format(time.DateOnly),
		}
	}

	ref := Reference{Provider: providerID, ModelKey: m.id, MatchedBy: matchedBy}
	// The input rate is the one bucket that must be present. Every other bucket
	// missing means "this model does not charge for that", which is a real and
	// common shape (an embedding model has no output tokens); input missing
	// means the entry does not price the model at all, and inventing a zero
	// there would hand out a free model.
	if _, ok := chosen.rates[bucketInput]; !ok {
		return Result{
			Outcome: Unusable,
			Detail:  providerID + "/" + m.id + " carries no input rate",
		}
	}
	dst := map[string]*string{
		bucketInput:      &ref.Rates.Input,
		bucketOutput:     &ref.Rates.Output,
		bucketCacheRead:  &ref.Rates.CacheRead,
		bucketCacheWrite: &ref.Rates.CacheWrite,
	}
	for _, bucket := range baseBuckets {
		v, ok := chosen.rates[bucket]
		if !ok {
			// An explicit zero, because a stored price is complete in all four
			// buckets or it is not a price at all. Which buckets got there this
			// way is recorded, so "the dataset says this is free" stays
			// distinguishable from "the dataset says nothing".
			*dst[bucket] = "0"
			ref.Defaulted = append(ref.Defaulted, bucket)
			continue
		}
		if v.tiered {
			ref.ContextTiered = true
		}
		lit, rounded, err := quantize(v.literal)
		if err != nil {
			return Result{
				Outcome: Unusable,
				Detail:  fmt.Sprintf("%s/%s: %s %v", providerID, m.id, bucket, err),
			}
		}
		if rounded {
			ref.Rounded = append(ref.Rounded, bucket)
		}
		*dst[bucket] = lit
	}
	// The two image rates, when the dataset states them. Absent leaves the
	// field empty rather than zero: empty means "no separate image rate", and
	// billing falls back to the text rate on that side. Zero would mean "image
	// tokens are free", which no entry says.
	for _, im := range []struct {
		bucket string
		dst    *string
	}{
		{bucketInputImage, &ref.Rates.InputImage},
		{bucketOutputImage, &ref.Rates.OutputImage},
	} {
		v, ok := chosen.rates[im.bucket]
		if !ok {
			continue
		}
		lit, rounded, err := quantize(v.literal)
		if err != nil {
			return Result{
				Outcome: Unusable,
				Detail:  fmt.Sprintf("%s/%s: %s %v", providerID, m.id, im.bucket, err),
			}
		}
		if rounded {
			ref.Rounded = append(ref.Rounded, im.bucket)
		}
		if v.tiered {
			ref.ContextTiered = true
		}
		*im.dst = lit
	}
	slices.Sort(ref.Defaulted)
	slices.Sort(ref.Rounded)
	return Result{Outcome: Matched, Ref: &ref}
}

// quantize brings a rate literal down to the precision the gateway stores,
// rounding half up, and reports whether it had to.
//
// The whole conversion runs on the digit string. Going through float64 here
// would be the one place in the pipeline where a bill can pick up a difference
// that no later audit can account for -- and the dataset does carry values such
// as 0.08333333333333334, which is a float artefact of one twelfth and exactly
// the kind of input that makes the shortcut look harmless.
func quantize(literal string) (string, bool, error) {
	if !money.IsPlainDecimal(literal, 0) {
		return "", false, fmt.Errorf("rate %q is not a plain non-negative decimal", literal)
	}
	whole, fraction, _ := strings.Cut(literal, ".")
	if len(fraction) <= storedFractionDigits {
		return literal, false, nil
	}
	keep, rest := fraction[:storedFractionDigits], fraction[storedFractionDigits:]
	n, ok := new(big.Int).SetString(whole+keep, 10)
	if !ok {
		return "", false, fmt.Errorf("rate %q is not a readable decimal", literal)
	}
	if rest[0] >= '5' {
		n.Add(n, big.NewInt(1))
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(storedFractionDigits), nil)
	q, r := new(big.Int).QuoRem(n, scale, new(big.Int))
	out := q.String()
	if r.Sign() != 0 {
		frac := strings.TrimRight(fmt.Sprintf("%0*s", storedFractionDigits, r.String()), "0")
		out += "." + frac
	}
	return out, true, nil
}
