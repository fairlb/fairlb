package refdata

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Per-unit reference rates: a second of produced video, or one generation.
//
// # Why this is a second file rather than rows in the bundled dataset
//
// The dataset next to it is a vendored snapshot of somebody else's price list,
// with its own LICENCE and its own upstream commit. It carries token rates and
// nothing else -- as of the snapshot beside it, not one video model appears in
// it at all -- so per-unit rates cannot come from there, and adding our own rows
// to a vendored file would destroy the one thing that makes it checkable: that
// every line in it came from a named revision of somebody else's work.
//
// So this list is ours. That difference is worth stating plainly, because it
// changes what the file can be trusted for:
//
//   - **The rates are still only a prefill.** Everything ADR-0128 requires of
//     the other dataset applies here unchanged: a row written from this is
//     stored with `verified_at` NULL, a row a person has checked is never
//     overwritten, and the console shows the difference. "This repository
//     suggested a number" and "somebody checked it against the vendor's page"
//     stay two different facts.
//   - **There is no external cross-check.** For token rates, a mistyped
//     upstream id fails to match a third party's spelling and a test catches
//     it. Here we would be checking our own file against our own file, so no
//     such test exists and none is claimed. What guards this list is the same
//     thing that guards the runbook table it was lifted from: a person reading
//     it against the vendor's price page, on the date recorded below.
//
// # Why entries match on a prefix
//
// The other dataset can name exact model ids because they are somebody else's
// published identifiers. These vendors' ids carry release dates and deployment
// suffixes -- one publishes `veo-3.1-generate-preview`, another appends a
// datestamp, a third lets the operator use an endpoint id -- and guessing the
// full string would produce an entry that silently matches nothing. A prefix
// matches the id the operator actually wired, which is the one the upstream
// itself listed.

//go:embed unit-rates.json
var bundledUnitRates []byte

// UnitRate is one row of a per-unit rate card, priced in USD per unit -- per
// second, never per million of them. The token rates beside these are quoted
// per million and divided on the way in; a per-second rate through that
// conversion would be wrong by a factor of a million (ADR-0220).
type UnitRate struct {
	Unit       string `json:"unit"`
	Resolution string `json:"resolution"`
	Audio      string `json:"audio"`
	Variant    string `json:"variant"`
	USDPerUnit string `json:"usd_per_unit"`
}

// UnitReference is one model's rate card and where it came from.
type UnitReference struct {
	Vendor     string     `json:"vendor"`
	Label      string     `json:"label"`
	Prefixes   []string   `json:"prefixes"`
	SourceName string     `json:"source_name"`
	SourceURL  string     `json:"source_url"`
	Rates      []UnitRate `json:"rates"`
	// CheckedOn is the day a person last read these against the vendor's own
	// page. It is the list's date, not the row's: the whole file is checked
	// together or not at all.
	CheckedOn string `json:"-"`
	// Dataset names this list in the provenance the import records, so a stored
	// price can always say which of the two sources produced it.
	Dataset string `json:"-"`
}

type unitFile struct {
	Dataset   string          `json:"dataset"`
	CheckedOn string          `json:"checked_on"`
	Models    []UnitReference `json:"models"`
}

// UnitDataset answers "what does this repository suggest for this model".
type UnitDataset struct {
	Dataset   string
	CheckedOn string
	models    []UnitReference
}

var (
	unitOnce sync.Once
	unitData *UnitDataset
	unitErr  error
)

// BundledUnitRates returns the list compiled into the binary.
func BundledUnitRates() (*UnitDataset, error) {
	unitOnce.Do(func() { unitData, unitErr = ParseUnitRates(bundledUnitRates) })
	return unitData, unitErr
}

// ParseUnitRates reads a per-unit list, refusing one that cannot be billed
// from. A row with no unit or no price would import as a rate card that prices
// nothing, and a model on the per-unit family with no usable row answers 503 to
// every request against it -- so it is refused here, where the file is.
func ParseUnitRates(raw []byte) (*UnitDataset, error) {
	var f unitFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("refdata: parse the per-unit rates: %w", err)
	}
	if f.Dataset == "" {
		return nil, fmt.Errorf("refdata: the per-unit rates name no list, so a price written " +
			"from them could not say where it came from")
	}
	if f.CheckedOn == "" {
		return nil, fmt.Errorf("refdata: the per-unit rates record no day they were checked, " +
			"which is the only thing separating them from numbers nobody has read")
	}
	seen := map[string]string{}
	for i := range f.Models {
		m := &f.Models[i]
		m.Dataset, m.CheckedOn = f.Dataset, f.CheckedOn
		switch {
		case m.Vendor == "":
			return nil, fmt.Errorf("refdata: a per-unit entry names no vendor")
		case len(m.Prefixes) == 0:
			return nil, fmt.Errorf("refdata: %s: a per-unit entry matches no upstream id", m.Vendor)
		case len(m.Rates) == 0:
			return nil, fmt.Errorf("refdata: %s: a per-unit entry carries no rates, so it would "+
				"import a model that cannot be charged for", m.Vendor)
		case m.SourceName == "" || m.SourceURL == "":
			return nil, fmt.Errorf("refdata: %s: a per-unit entry with no source is a number "+
				"nobody can ever check", m.Vendor)
		}
		for _, r := range m.Rates {
			if r.Unit == "" || r.USDPerUnit == "" {
				return nil, fmt.Errorf("refdata: %s: a per-unit rate is missing its unit or its price", m.Vendor)
			}
		}
		// Two entries whose prefixes overlap under one vendor would make the
		// answer depend on iteration order, and the wrong one is a wrong bill.
		for _, p := range m.Prefixes {
			key := m.Vendor + "/" + p
			for existing := range seen {
				v, prefix, _ := strings.Cut(existing, "/")
				if v != m.Vendor {
					continue
				}
				if strings.HasPrefix(p, prefix) || strings.HasPrefix(prefix, p) {
					return nil, fmt.Errorf(
						"refdata: %s: prefixes %q and %q overlap, so which rate card applies "+
							"would depend on the order of this file", m.Vendor, prefix, p)
				}
			}
			seen[key] = m.Label
		}
	}
	return &UnitDataset{Dataset: f.Dataset, CheckedOn: f.CheckedOn, models: f.Models}, nil
}

// Lookup finds the rate card this repository suggests for one upstream model on
// one vendor, if it has an opinion at all. Most models are not per-unit, and no
// answer is the ordinary case rather than a failure.
func (d *UnitDataset) Lookup(vendor, upstreamModelID string) (UnitReference, bool) {
	if d == nil {
		return UnitReference{}, false
	}
	id := strings.ToLower(strings.TrimSpace(upstreamModelID))
	for _, m := range d.models {
		if m.Vendor != vendor {
			continue
		}
		for _, p := range m.Prefixes {
			if strings.HasPrefix(id, strings.ToLower(p)) {
				return m, true
			}
		}
	}
	return UnitReference{}, false
}

// Vendors lists which vendors this file has any opinion about. Reported so an
// operator can be told plainly which video models they still have to price by
// hand, rather than finding out by pressing a button that does nothing.
func (d *UnitDataset) Vendors() []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, m := range d.models {
		if !slices.Contains(out, m.Vendor) {
			out = append(out, m.Vendor)
		}
	}
	slices.Sort(out)
	return out
}
