// Package refdata carries a bundled snapshot of a public model-price dataset
// and turns an upstream model id into the four token rates the gateway stores.
//
// It is a reference, never a source of truth. Nothing in here runs at request
// time and nothing in here reaches the network: the snapshot is compiled into
// the binary, an operator triggers a read of it by hand, and every rate it
// yields is meant to be stored as unverified. "A dataset suggested this number"
// and "a human agreed with it" are different facts, and a gateway that bills by
// the token cannot afford to merge them.
//
// The dataset is vendored under its own licence; see LICENSE next to this file,
// and snapshot.json for where it came from and when.
package refdata

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The bundled snapshot. Both files are embedded so that the binary is the whole
// deliverable: an operator running the import inside a container without a
// shell has no way to supply a data file that was left on the build machine.
//
//go:embed genai-prices.json
var bundledData []byte

//go:embed snapshot.json
var bundledSnapshot []byte

// Snapshot records the provenance of the bundled data: which dataset, which
// upstream revision, and when it was taken.
//
// It is written into every row the import produces. A price with no answer to
// "where did this come from" is a number nobody can ever check.
//
// The struct is narrower than snapshot.json on purpose. That file is the
// verbatim provenance record kept beside the dataset and its LICENSE, so it
// also carries `file_url` and `upstream_date`; neither is read here, because
// `upstream_commit` already pins the revision exactly and `source_url` plus
// that commit reconstruct the file URL. Parsing a field nobody writes into a
// row would only look like provenance.
type Snapshot struct {
	Dataset        string `json:"dataset"`
	SourceURL      string `json:"source_url"`
	UpstreamCommit string `json:"upstream_commit"`
	SnapshotDate   string `json:"snapshot_date"`
	License        string `json:"license"`
	SHA256         string `json:"sha256"`
}

// Dataset is a parsed price dataset, ready to answer lookups.
type Dataset struct {
	Snapshot  Snapshot
	providers []provider
	// Entries is how many model entries the dataset holds. Reported by callers
	// so that "the import matched nothing" can be told apart from "the dataset
	// failed to load and nobody noticed".
	Entries int
}

type provider struct {
	id         string
	apiPattern *regexp.Regexp
	models     []modelEntry
}

type modelEntry struct {
	id    string
	match clause
	// prices is the conditional chain exactly as the dataset states it. The
	// last entry whose constraint holds is the one in force, so resolution
	// needs the whole chain rather than a single price set.
	prices []conditionalPrice
}

var (
	bundledOnce sync.Once
	bundled     *Dataset
	bundledErr  error
)

// Bundled parses the snapshot compiled into the binary. Repeated calls return
// the same dataset; parsing is not free and nothing mutates it.
func Bundled() (*Dataset, error) {
	bundledOnce.Do(func() { bundled, bundledErr = loadBundled(bundledData, bundledSnapshot) })
	return bundled, bundledErr
}

// loadBundled is Bundled's body, taking its two inputs as arguments so that the
// digest check below can be shown to fail on a mismatch. A self-check nobody
// can make fail is indistinguishable from no self-check at all.
func loadBundled(data, snapshotRecord []byte) (*Dataset, error) {
	var snap Snapshot
	if err := json.Unmarshal(snapshotRecord, &snap); err != nil {
		return nil, fmt.Errorf("refdata: read the bundled snapshot record: %w", err)
	}
	// The digest is checked rather than trusted. Updating the data file and
	// forgetting to update the record beside it would otherwise leave every
	// imported row stamped with a date and a revision describing some earlier
	// set of numbers -- and a wrong provenance is worse than none, because it
	// still answers the question people ask.
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != snap.SHA256 {
		return nil, fmt.Errorf(
			"refdata: the bundled data does not match its snapshot record "+
				"(record says %s, data hashes to %s); update snapshot.json alongside the data",
			snap.SHA256, got)
	}
	return parse(data, snap)
}

// Parse reads a dataset supplied by the caller, in the same format as the
// bundled one. The snapshot record describes where the caller got it.
func Parse(data []byte, snap Snapshot) (*Dataset, error) { return parse(data, snap) }

// wireProvider and wireModel mirror the dataset's own shape. Only the fields
// this package uses are declared; the rest -- usage extractors, display names,
// deprecation flags -- describe problems the gateway solves elsewhere, and
// naming them here would suggest they are consulted.
type wireProvider struct {
	ID         string      `json:"id"`
	APIPattern string      `json:"api_pattern"`
	Models     []wireModel `json:"models"`
}

type wireModel struct {
	ID    string          `json:"id"`
	Match json.RawMessage `json:"match"`
	// Prices is either one price set or a chain of conditional ones, so it has
	// to be held raw until its shape is known.
	Prices json.RawMessage `json:"prices"`
}

func parse(data []byte, snap Snapshot) (*Dataset, error) {
	var wire []wireProvider
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as their literal text. Every rate here ends up on a bill,
	// and float64 is a place where a value can pick up a difference that nobody
	// can explain afterwards -- the conversion to storage units is done on the
	// decimal string instead.
	dec.UseNumber()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("refdata: parse the dataset: %w", err)
	}
	d := &Dataset{Snapshot: snap, providers: make([]provider, 0, len(wire))}
	for _, wp := range wire {
		p := provider{id: wp.ID, models: make([]modelEntry, 0, len(wp.Models))}
		if wp.APIPattern != "" {
			re, err := regexp.Compile(wp.APIPattern)
			if err != nil {
				// Reported rather than skipped. A provider whose pattern was
				// dropped still matches by name, so the failure would show up
				// only as "some base URLs stopped being recognised" months
				// later, with nothing to point at.
				return nil, fmt.Errorf("refdata: provider %q has an unusable endpoint pattern: %w", wp.ID, err)
			}
			p.apiPattern = re
		}
		for _, wm := range wp.Models {
			c, err := parseClause(wm.Match)
			if err != nil {
				return nil, fmt.Errorf("refdata: %s/%s: %w", wp.ID, wm.ID, err)
			}
			prices, err := parseConditionalPrices(wm.Prices)
			if err != nil {
				return nil, fmt.Errorf("refdata: %s/%s: %w", wp.ID, wm.ID, err)
			}
			p.models = append(p.models, modelEntry{id: wm.ID, match: c, prices: prices})
			d.Entries++
		}
		d.providers = append(d.providers, p)
	}
	if d.Entries == 0 {
		return nil, fmt.Errorf("refdata: the dataset holds no model entries")
	}
	return d, nil
}

// Outcome is what a lookup concluded. Everything other than Matched is meant to
// be printed: a price the gateway declined to fill in has to be named, because
// the alternative is an operator wondering why one model out of forty is still
// refusing traffic.
type Outcome string

const (
	// Matched: the dataset prices this model and the rates are storable.
	Matched Outcome = "matched"
	// NoEntry: no entry in scope recognises this upstream model id.
	NoEntry Outcome = "no reference entry"
	// Ambiguous: several entries recognise it and they disagree on the rates.
	Ambiguous Outcome = "several reference entries disagree"
	// TimeOfDay: the dataset prices this model differently at different times
	// of day, which a single stored rate cannot express.
	TimeOfDay Outcome = "priced by time of day"
	// Unusable: the entry carries no input rate, or a rate the gateway cannot
	// store.
	Unusable Outcome = "unusable reference rate"
)

// Result is one lookup's conclusion. Ref is set only when Outcome is Matched.
type Result struct {
	Outcome Outcome
	Detail  string
	Ref     *Reference
}

// Reference is one dataset entry reduced to what the gateway stores.
type Reference struct {
	Provider string
	ModelKey string
	Rates    Rates
	// MatchedBy is "equals" when the dataset names this upstream id exactly and
	// "pattern" when it was recognised by a prefix or substring rule. An exact
	// name is a much stronger claim than a substring, and the difference is
	// worth keeping for whoever reviews the imported price later.
	MatchedBy string
	// Defaulted names the buckets the dataset does not price at all, written as
	// an explicit zero because a stored price has to be complete in all four.
	// Recorded rather than assumed: "the dataset says this is free" and "the
	// dataset says nothing" are different, and only one of them is safe to bill
	// against.
	Defaulted []string
	// Rounded names the buckets whose dataset value carried more precision than
	// the gateway stores and was rounded to fit.
	Rounded []string
	// ContextTiered is true when the dataset prices this model in steps by
	// input size. Only the base step is carried, because there is nowhere to
	// put the others yet; saying so lets the operator go and check.
	ContextTiered bool
}

// Rates are the four token rates as exact decimal strings, in USD per million
// tokens, plus the two image rates that have somewhere to be stored.
type Rates struct {
	Input      string
	Output     string
	CacheRead  string
	CacheWrite string
	// InputImage and OutputImage are the two image rates, empty when the
	// dataset states none.
	//
	// Empty and "0" mean different things here, which is why the four base
	// rates default to "0" and these do not: a stored price has to be complete
	// in the four, while a missing image rate means the model has no separate
	// one and its image tokens bill at the text rate on that side -- the
	// behaviour of every model that has no image dimension configured.
	InputImage  string
	OutputImage string
}

// Scope resolves one of the gateway's providers to a dataset provider id, so
// that a lookup only considers entries that provider could plausibly serve. An
// empty result means "no scope", and the lookup then searches everything.
//
// Two signals, in order, and neither of them guesses:
//
//   - the provider's own name, when it is spelled the same as a dataset
//     provider;
//   - the base URL, when exactly one dataset provider claims that endpoint.
//
// "Exactly one" matters. Several claimants is not a tie to be broken by
// ordering -- it is the dataset saying it cannot tell, and inventing an answer
// there means silently pricing a model against the wrong vendor.
func (d *Dataset) Scope(providerSlug, baseURL string) string {
	norm := normalizeSlug(providerSlug)
	for _, p := range d.providers {
		if normalizeSlug(p.id) == norm && norm != "" {
			return p.id
		}
	}
	var hit string
	var n int
	for _, p := range d.providers {
		if p.apiPattern != nil && baseURL != "" && p.apiPattern.MatchString(baseURL) {
			hit, n = p.id, n+1
		}
	}
	if n == 1 {
		return hit
	}
	return ""
}

func normalizeSlug(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

// Lookup resolves one upstream model id to a reference price.
//
// scope narrows the search to a single dataset provider; an empty scope
// searches all of them. `on` selects among dated prices -- the dataset carries
// announced price changes ahead of the day they take effect, so resolution
// needs a date rather than "whatever is last in the file".
//
// The matching itself is case-sensitive, because the id being matched is the
// exact string the gateway sends upstream, and upstream vendors do ship model
// names that differ only in case.
func (d *Dataset) Lookup(scope, upstreamModelID string, on time.Time) Result {
	if upstreamModelID == "" {
		return Result{Outcome: NoEntry, Detail: "the route carries no upstream model id"}
	}
	type hit struct {
		provider string
		entry    modelEntry
		exact    bool
	}
	var hits []hit
	for _, p := range d.providers {
		if scope != "" && p.id != scope {
			continue
		}
		for _, m := range p.models {
			if !m.match.matches(upstreamModelID) {
				continue
			}
			hits = append(hits, hit{provider: p.id, entry: m, exact: m.match.namesExactly(upstreamModelID)})
		}
	}
	if len(hits) == 0 {
		return Result{Outcome: NoEntry, Detail: detailScope(scope)}
	}
	// An exact name beats a prefix or substring rule. Where the dataset both
	// names a model and has a catch-all that happens to cover it, the name is
	// the specific statement and the catch-all is the fallback.
	exact := make([]hit, 0, len(hits))
	for _, h := range hits {
		if h.exact {
			exact = append(exact, h)
		}
	}
	matchedBy := "pattern"
	if len(exact) > 0 {
		hits, matchedBy = exact, "equals"
	}

	var chosen *Reference
	for _, h := range hits {
		res := referenceFrom(h.provider, h.entry, on, matchedBy)
		if res.Outcome != Matched {
			// One unusable candidate does not condemn the rest, but if it is
			// the only one its reason is what the operator needs to read.
			if len(hits) == 1 {
				return res
			}
			continue
		}
		if chosen == nil {
			chosen = res.Ref
			continue
		}
		if chosen.Rates != res.Ref.Rates {
			return Result{
				Outcome: Ambiguous,
				Detail: fmt.Sprintf("%s/%s and %s/%s price it differently",
					chosen.Provider, chosen.ModelKey, res.Ref.Provider, res.Ref.ModelKey),
			}
		}
	}
	if chosen == nil {
		return Result{Outcome: Unusable, Detail: "every matching entry carries an unusable rate"}
	}
	return Result{Outcome: Matched, Ref: chosen}
}

func detailScope(scope string) string {
	if scope == "" {
		return "no entry in the dataset recognises this upstream model id"
	}
	return "no entry under " + scope + " recognises this upstream model id"
}
