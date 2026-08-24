package catalog

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
)

// The registry is data, and data that nothing checks rots in place. Each
// assertion below stands for a way an entry can be wrong that would otherwise
// surface as a support question rather than as a failing build:
// a preset the write path would reject, a base URL that cannot be requested, a
// reference-price id that resolves to nothing, a protocol nobody serves.

var vendorSlugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestVendorSlugsAreWellFormedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range Vendors() {
		if !vendorSlugRE.MatchString(v.Slug) {
			t.Errorf("slug %q does not match the shape the column allows", v.Slug)
		}
		if len(v.Slug) > 40 {
			t.Errorf("slug %q is longer than the column allows", v.Slug)
		}
		if seen[v.Slug] {
			t.Errorf("slug %q appears twice", v.Slug)
		}
		seen[v.Slug] = true
		if strings.TrimSpace(v.Label) == "" {
			t.Errorf("%s has no display label", v.Slug)
		}
	}
	if !seen[VendorCustom] {
		t.Error("the custom vendor is missing; every upstream without an entry depends on it")
	}
}

func TestCustomIsLastAndSpeaksEveryKnownProtocol(t *testing.T) {
	all := Vendors()
	if last := all[len(all)-1].Slug; last != VendorCustom {
		t.Errorf("custom should be last in the list, found %q there", last)
	}
	custom, ok := LookupVendor(VendorCustom)
	if !ok {
		t.Fatal("custom is not in the registry")
	}
	if !slices.Equal(custom.Protocols, KnownProtocols()) {
		t.Errorf("custom speaks %v, the gateway speaks %v; a protocol missing here is one "+
			"no unlisted upstream can be configured for", custom.Protocols, KnownProtocols())
	}
}

// A vendor whose preset transport already carries the messages path is wired
// for both protocols, and its default must say so: a provider created by
// picking that vendor speaks both, which is the whole point of a model owning
// no protocol -- the same Claude model is reachable on chat and on messages
// through one record.
func TestVendorsWiredForBothProtocolsDefaultToBoth(t *testing.T) {
	for _, v := range Vendors() {
		if v.Slug == VendorCustom {
			continue
		}
		wired := v.Transport.PathOverrides[PathMessages] != "" && slices.Contains(v.Protocols, ProtocolOpenAI)
		if wired && !slices.Equal(v.DefaultProtocols, []string{ProtocolOpenAI, ProtocolAnthropic}) {
			t.Errorf("%s has a messages path override but defaults to %v; a provider created from it would speak only one of the two protocols it is wired for", v.Slug, v.DefaultProtocols)
		}
	}
}

func TestVendorProtocolsAreServedAndDefaultsAreASubset(t *testing.T) {
	for _, v := range Vendors() {
		if len(v.Protocols) == 0 {
			t.Errorf("%s declares no protocol", v.Slug)
		}
		for _, p := range v.Protocols {
			if _, ok := ProtocolEndpoints(p); !ok {
				t.Errorf("%s declares protocol %q, which this gateway does not serve", v.Slug, p)
			}
		}
		if len(v.DefaultProtocols) == 0 {
			t.Errorf("%s has no default protocol; the create form would open with none ticked", v.Slug)
		}
		for _, p := range v.DefaultProtocols {
			if !slices.Contains(v.Protocols, p) {
				t.Errorf("%s defaults to %q, which is not in its own protocol set", v.Slug, p)
			}
		}
		for p := range v.Fidelity {
			if !slices.Contains(v.Protocols, p) {
				t.Errorf("%s rates protocol %q that it does not declare", v.Slug, p)
			}
		}
		for p, f := range v.Fidelity {
			switch f {
			case FidelityFull, FidelityPartial, FidelityTotalsOnly, FidelityUnknown:
			default:
				t.Errorf("%s rates %s as %q, which is not a fidelity value", v.Slug, p, f)
			}
		}
	}
}

func TestVendorBaseURLsAreRequestable(t *testing.T) {
	for _, v := range Vendors() {
		if v.Slug == VendorCustom {
			if len(v.BaseURLs) != 0 {
				t.Error("custom should prefill no base URL; there is nothing to prefill it from")
			}
			continue
		}
		if len(v.BaseURLs) == 0 {
			t.Errorf("%s prefills no base URL", v.Slug)
		}
		labelled := len(v.BaseURLs) > 1
		for _, b := range v.BaseURLs {
			if labelled && b.Label == "" {
				t.Errorf("%s offers several endpoints with an unlabelled one among them", v.Slug)
			}
			hasPlaceholder := strings.ContainsAny(b.URL, "{}")
			if hasPlaceholder != b.Template {
				t.Errorf("%s base URL %q: Template is %v but the URL %s a placeholder",
					v.Slug, b.URL, b.Template,
					map[bool]string{true: "carries", false: "does not carry"}[hasPlaceholder])
			}
			if b.Template {
				continue // not a URL yet; the operator has to finish it
			}
			u, err := url.Parse(b.URL)
			if err != nil {
				t.Errorf("%s base URL %q does not parse: %v", v.Slug, b.URL, err)
				continue
			}
			if u.Scheme != "https" || u.Host == "" {
				t.Errorf("%s base URL %q is not an absolute https URL", v.Slug, b.URL)
			}
			if u.RawQuery != "" || u.Fragment != "" {
				t.Errorf("%s base URL %q carries a query or fragment; those belong in the "+
					"transport profile, which appends them to every request", v.Slug, b.URL)
			}
			if strings.HasSuffix(b.URL, "/") {
				t.Errorf("%s base URL %q ends in a slash, which doubles the one in every path",
					v.Slug, b.URL)
			}
		}
	}
}

// The presets have to survive the same validation a hand-typed profile does.
// Anything this catches would otherwise be a vendor whose create form fills in
// a profile the save then refuses -- with the operator looking at a field they
// did not write.
func TestVendorPresetsPassTheWritePathValidation(t *testing.T) {
	for _, v := range Vendors() {
		raw, err := json.Marshal(v.Transport)
		if err != nil {
			t.Fatalf("%s: marshalling the preset: %v", v.Slug, err)
		}
		got, err := ValidateTransport(raw)
		if err != nil {
			t.Errorf("%s preset is rejected by ValidateTransport: %v", v.Slug, err)
			continue
		}
		// Round-tripping is the actual claim: what the form prefills is what
		// the server would store.
		back, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("%s: re-marshalling: %v", v.Slug, err)
		}
		if string(back) != string(raw) {
			t.Errorf("%s preset does not round-trip: %s became %s", v.Slug, raw, back)
		}
	}
}

// Overridden paths must name paths the gateway actually sends, or the override
// is a setting that saves and does nothing -- ADR-0127's own complaint about
// the column it replaced.
func TestVendorPresetOverridesNameRealPaths(t *testing.T) {
	valid := UpstreamPaths()
	for _, v := range Vendors() {
		for _, m := range []map[string]string{v.Transport.PathOverrides, v.Transport.StreamPathOverrides} {
			for k, dest := range m {
				if !slices.Contains(valid, k) {
					t.Errorf("%s overrides %q, which is not a path this gateway sends", v.Slug, k)
				}
				if !strings.HasPrefix(dest, "/") {
					t.Errorf("%s maps %q to %q, which is not a path", v.Slug, k, dest)
				}
			}
		}
		// An override for a protocol the vendor does not declare can never fire.
		for k := range v.Transport.PathOverrides {
			if k == PathMessages && !slices.Contains(v.Protocols, ProtocolAnthropic) {
				t.Errorf("%s overrides the Anthropic path but does not declare that protocol", v.Slug)
			}
		}
	}
}

// A vendor that declares an envelope is describing the Anthropic Messages API
// as one hosted platform cuts it. Applied to an OpenAI-shaped request the
// envelope removes the model field and adds an Anthropic version, so the
// combination cannot work -- and it fails at the upstream, where it reads as
// the upstream's fault.
func TestEnvelopePresetsAreAnthropicOnly(t *testing.T) {
	for _, v := range Vendors() {
		if v.Transport.BodyEnvelope() == EnvelopeNone {
			continue
		}
		for _, p := range v.Protocols {
			if p != ProtocolAnthropic {
				t.Errorf("%s declares envelope %q and protocol %q; an enveloped request is "+
					"an Anthropic one", v.Slug, v.Transport.Envelope, p)
			}
		}
	}
}

func TestVendorRefdataIDsExistInTheBundledDataset(t *testing.T) {
	data, err := refdata.Bundled()
	if err != nil {
		t.Fatalf("loading the bundled reference prices: %v", err)
	}
	for _, v := range Vendors() {
		if v.RefdataProvider == "" {
			continue
		}
		if got := data.Scope(v.RefdataProvider, ""); got != v.RefdataProvider {
			t.Errorf("%s points at reference-price provider %q, which the dataset does not "+
				"have (Scope answered %q); prices would silently resolve against everything",
				v.Slug, v.RefdataProvider, got)
		}
	}
}

func TestNonCustomVendorsCarryOperatorGuidance(t *testing.T) {
	for _, v := range Vendors() {
		if v.Slug == VendorCustom {
			continue
		}
		if v.ModelIDExample == "" {
			t.Errorf("%s has no example model id; the wiring form has nothing to show", v.Slug)
		}
		if v.DocsURL == "" {
			t.Errorf("%s has no documentation link", v.Slug)
		}
		switch v.KeyHint {
		case KeyHintBearer, KeyHintAWSKeypairJSON, KeyHintGCPServiceAccount:
		default:
			t.Errorf("%s has key hint %q, which the interface cannot render", v.Slug, v.KeyHint)
		}
		switch v.Kind {
		case VendorFirstParty, VendorPlatform, VendorAggregator:
		default:
			t.Errorf("%s has kind %q, which is not a group", v.Slug, v.Kind)
		}
	}
}

func TestCheckProtocols(t *testing.T) {
	deepseek, ok := LookupVendor("deepseek")
	if !ok {
		t.Fatal("deepseek is missing from the registry")
	}
	if err := deepseek.CheckProtocols([]string{ProtocolOpenAI, ProtocolAnthropic}); err != nil {
		t.Errorf("both of DeepSeek's own dialects should be allowed: %v", err)
	}
	anthropic, _ := LookupVendor("anthropic")
	if err := anthropic.CheckProtocols([]string{ProtocolOpenAI}); err == nil {
		t.Error("Anthropic does not publish the OpenAI dialect; declaring it should be refused")
	} else if !strings.Contains(err.Error(), ProtocolAnthropic) {
		t.Errorf("the refusal should say what the vendor does speak, got %q", err)
	}
	if err := anthropic.CheckProtocols(nil); err == nil {
		t.Error("a provider with no protocol should be refused")
	}
	custom, _ := LookupVendor(VendorCustom)
	for _, p := range KnownProtocols() {
		if err := custom.CheckProtocols([]string{p}); err != nil {
			t.Errorf("custom should accept %q: %v", p, err)
		}
	}
	if err := custom.CheckProtocols([]string{"gopher"}); err == nil {
		t.Error("even custom cannot speak a protocol the gateway does not serve")
	}
}

func TestVendorLabelFallsBackToTheSlug(t *testing.T) {
	if got := VendorLabel("openai"); got != "OpenAI" {
		t.Errorf("VendorLabel(openai) = %q", got)
	}
	if got := VendorLabel("nobody-has-heard-of-this"); got != "nobody-has-heard-of-this" {
		t.Errorf("an unknown vendor should render as its own slug, got %q", got)
	}
}

// Vendors() hands out copies. Without this the registry is one shared map and
// any caller that edits a preset edits it for every later reader.
func TestVendorsAreNotShared(t *testing.T) {
	first := Vendors()
	for i := range first {
		if first[i].Transport.PathOverrides != nil {
			first[i].Transport.PathOverrides[PathChat] = "/mutated"
			break
		}
	}
	for _, v := range Vendors() {
		if v.Transport.PathOverrides[PathChat] == "/mutated" {
			t.Fatalf("%s: editing one caller's copy changed the registry", v.Slug)
		}
	}
}
