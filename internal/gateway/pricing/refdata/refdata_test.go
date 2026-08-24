package refdata

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

var day = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

// synthetic builds a dataset from a literal, so that the rules below are
// pinned to values that stay put. Asserting rates against the bundled snapshot
// instead would turn "somebody refreshed the dataset" into a test failure,
// which teaches people to edit the expectation rather than read it.
func synthetic(t *testing.T, body string) *Dataset {
	t.Helper()
	d, err := Parse([]byte(body), Snapshot{Dataset: "test", SnapshotDate: "2026-08-17"})
	if err != nil {
		t.Fatalf("parse the test dataset: %v", err)
	}
	return d
}

func TestBundledLoadsAndMatchesItsRecord(t *testing.T) {
	d, err := Bundled()
	if err != nil {
		t.Fatalf("load the bundled dataset: %v", err)
	}
	if d.Entries < 100 {
		t.Fatalf("the bundled dataset holds %d entries, which is too few to be the real one", d.Entries)
	}
	for name, v := range map[string]string{
		"dataset": d.Snapshot.Dataset, "source_url": d.Snapshot.SourceURL,
		"snapshot_date": d.Snapshot.SnapshotDate, "license": d.Snapshot.License,
		"sha256": d.Snapshot.SHA256,
	} {
		if v == "" {
			t.Errorf("the bundled snapshot record leaves %s empty; every imported row is stamped with it", name)
		}
	}
	if _, err := time.Parse(time.DateOnly, d.Snapshot.SnapshotDate); err != nil {
		t.Errorf("snapshot_date %q is not a date: %v", d.Snapshot.SnapshotDate, err)
	}
}

// The digest check is the only thing standing between "the data was refreshed"
// and "every row now carries a provenance that describes the previous numbers",
// so it gets a test that makes it fail.
func TestBundledDigestRefusesAMismatch(t *testing.T) {
	if _, err := loadBundled(bundledData, bundledSnapshot); err != nil {
		t.Fatalf("the shipped pair must load: %v", err)
	}
	tampered := append(append([]byte{}, bundledData...), ' ')
	_, err := loadBundled(tampered, bundledSnapshot)
	if err == nil {
		t.Fatal("data that does not hash to its record was accepted")
	}
	if !strings.Contains(err.Error(), "does not match its snapshot record") {
		t.Errorf("the failure does not say what is wrong: %v", err)
	}
}

func TestBundledRecognisesWellKnownUpstreamIDs(t *testing.T) {
	d, err := Bundled()
	if err != nil {
		t.Fatalf("load the bundled dataset: %v", err)
	}
	// Deliberately no rate assertions: this asks whether the matching reaches
	// the entries at all, which is what a bundled dataset is for.
	for _, tc := range []struct{ scope, model string }{
		{"openai", "gpt-4o"},
		{"openai", "text-embedding-3-small"},
		{"anthropic", "claude-sonnet-4-5"},
	} {
		res := d.Lookup(tc.scope, tc.model, day)
		if res.Outcome != Matched {
			t.Errorf("%s/%s: %s (%s)", tc.scope, tc.model, res.Outcome, res.Detail)
		}
	}
}

func TestScopeUsesNameThenEndpoint(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"openai","api_pattern":"https://api\\.openai\\.com","models":[
	    {"id":"gpt-x","match":{"equals":"gpt-x"},"prices":{"input_mtok":1}}]},
	  {"id":"acme","api_pattern":"https://api\\.acme\\.test","models":[
	    {"id":"acme-1","match":{"equals":"acme-1"},"prices":{"input_mtok":2}}]}
	]`)
	if got := d.Scope("OpenAI", "https://example.invalid"); got != "openai" {
		t.Errorf("name match: got %q, want openai", got)
	}
	// The name is unknown, so the endpoint decides.
	if got := d.Scope("my-relay", "https://api.acme.test/v1"); got != "acme" {
		t.Errorf("endpoint match: got %q, want acme", got)
	}
	// Neither signal fires: no scope, rather than a guess.
	if got := d.Scope("my-relay", "https://api.elsewhere.test/v1"); got != "" {
		t.Errorf("unknown provider: got %q, want no scope", got)
	}
}

func TestLookupPrefersAnExactName(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[
	    {"id":"broad","match":{"starts_with":"m-"},"prices":{"input_mtok":9,"output_mtok":9}},
	    {"id":"m-1","match":{"equals":"m-1"},"prices":{"input_mtok":1,"output_mtok":2}}]}
	]`)
	res := d.Lookup("acme", "m-1", day)
	if res.Outcome != Matched {
		t.Fatalf("outcome %s (%s)", res.Outcome, res.Detail)
	}
	if res.Ref.ModelKey != "m-1" || res.Ref.MatchedBy != "equals" {
		t.Errorf("got %s via %s, want m-1 via equals", res.Ref.ModelKey, res.Ref.MatchedBy)
	}
	if res.Ref.Rates.Input != "1" || res.Ref.Rates.Output != "2" {
		t.Errorf("rates %+v came from the catch-all", res.Ref.Rates)
	}
	// Nothing names m-2, so the catch-all is what is left.
	res = d.Lookup("acme", "m-2", day)
	if res.Outcome != Matched || res.Ref.MatchedBy != "pattern" {
		t.Errorf("catch-all: outcome %s via %q", res.Outcome, res.Ref.MatchedBy)
	}
}

func TestLookupReportsDisagreementInsteadOfPicking(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"a","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]},
	  {"id":"b","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":2}}]}
	]`)
	res := d.Lookup("", "m", day)
	if res.Outcome != Ambiguous {
		t.Fatalf("outcome %s, want %s", res.Outcome, Ambiguous)
	}
	if !strings.Contains(res.Detail, "a/m") || !strings.Contains(res.Detail, "b/m") {
		t.Errorf("the report does not name both candidates: %q", res.Detail)
	}
	// Agreement is not ambiguity: two entries stating the same rate resolve.
	same := synthetic(t, `[
	  {"id":"a","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]},
	  {"id":"b","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]}
	]`)
	if res := same.Lookup("", "m", day); res.Outcome != Matched {
		t.Errorf("agreeing entries: outcome %s (%s)", res.Outcome, res.Detail)
	}
}

func TestLookupWritesAnExplicitZeroForUnpricedBuckets(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[
	    {"id":"embed","match":{"equals":"embed"},"prices":{"input_mtok":0.02}}]}
	]`)
	res := d.Lookup("acme", "embed", day)
	if res.Outcome != Matched {
		t.Fatalf("outcome %s (%s)", res.Outcome, res.Detail)
	}
	want := Rates{Input: "0.02", Output: "0", CacheRead: "0", CacheWrite: "0"}
	if res.Ref.Rates != want {
		t.Errorf("rates %+v, want %+v", res.Ref.Rates, want)
	}
	// Which buckets got a zero this way has to stay readable afterwards:
	// "the dataset says free" and "the dataset says nothing" are not the same.
	if !slices.Equal(res.Ref.Defaulted, []string{"cache_read_mtok", "cache_write_mtok", "output_mtok"}) {
		t.Errorf("defaulted buckets %v", res.Ref.Defaulted)
	}
}

func TestLookupRefusesAnEntryWithNoInputRate(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[
	    {"id":"odd","match":{"equals":"odd"},"prices":{"output_mtok":1}}]}
	]`)
	res := d.Lookup("acme", "odd", day)
	if res.Outcome != Unusable {
		t.Fatalf("outcome %s, want %s", res.Outcome, Unusable)
	}
	if !strings.Contains(res.Detail, "no input rate") {
		t.Errorf("detail %q", res.Detail)
	}
}

func TestLookupRefusesTimeOfDayPricing(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[{"id":"offpeak","match":{"equals":"offpeak"},"prices":[
	    {"prices":{"input_mtok":1,"output_mtok":2}},
	    {"constraint":{"start_time":"16:30:00","end_time":"00:30:00"},
	     "prices":{"input_mtok":0.5,"output_mtok":1}}]}]}
	]`)
	res := d.Lookup("acme", "offpeak", day)
	if res.Outcome != TimeOfDay {
		t.Fatalf("outcome %s, want %s -- one stored rate cannot hold two prices for the same tokens",
			res.Outcome, TimeOfDay)
	}
}

func TestLookupTakesThePriceInForceOnTheGivenDay(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[{"id":"m","match":{"equals":"m"},"prices":[
	    {"prices":{"input_mtok":10,"output_mtok":20}},
	    {"constraint":{"start_date":"2026-06-01"},"prices":{"input_mtok":8,"output_mtok":16}}]}]}
	]`)
	before := d.Lookup("acme", "m", time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC))
	if before.Outcome != Matched || before.Ref.Rates.Input != "10" {
		t.Errorf("before the change: %s %+v", before.Outcome, before.Ref)
	}
	after := d.Lookup("acme", "m", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if after.Outcome != Matched || after.Ref.Rates.Input != "8" {
		t.Errorf("on the day it takes effect: %s %+v", after.Outcome, after.Ref)
	}
}

func TestLookupCarriesTheBaseStepAndSaysSo(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[{"id":"m","match":{"equals":"m"},"prices":{
	    "input_mtok":{"base":3,"tiers":[{"start":200000,"price":6}]},
	    "output_mtok":15,"cache_read_mtok":0.3,"cache_write_mtok":3.75}}]}
	]`)
	res := d.Lookup("acme", "m", day)
	if res.Outcome != Matched {
		t.Fatalf("outcome %s (%s)", res.Outcome, res.Detail)
	}
	if res.Ref.Rates.Input != "3" {
		t.Errorf("input rate %q, want the base step 3", res.Ref.Rates.Input)
	}
	if !res.Ref.ContextTiered {
		t.Error("the higher steps were dropped without saying so")
	}
}

func TestLookupIsCaseSensitive(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[
	    {"id":"M","match":{"equals":"Meta-Llama"},"prices":{"input_mtok":1}}]}
	]`)
	if res := d.Lookup("acme", "meta-llama", day); res.Outcome != NoEntry {
		t.Errorf("outcome %s: the id is the exact string sent upstream, and vendors do ship names differing only in case", res.Outcome)
	}
	if res := d.Lookup("acme", "Meta-Llama", day); res.Outcome != Matched {
		t.Errorf("outcome %s (%s)", res.Outcome, res.Detail)
	}
}

func TestLookupNamesTheScopeItSearched(t *testing.T) {
	d := synthetic(t, `[
	  {"id":"acme","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]}
	]`)
	res := d.Lookup("acme", "nope", day)
	if res.Outcome != NoEntry || !strings.Contains(res.Detail, "acme") {
		t.Errorf("outcome %s detail %q", res.Outcome, res.Detail)
	}
}

// An unknown operator has to be a parse failure. A clause that silently matches
// nothing looks exactly like a model the dataset does not carry, and the
// difference only surfaces as "half of them stopped being priced".
func TestParseRefusesAnUnreadableMatchRule(t *testing.T) {
	for name, body := range map[string]string{
		"unknown operator": `[{"id":"a","models":[{"id":"m","match":{"sounds_like":"m"},"prices":{"input_mtok":1}}]}]`,
		"two operators":    `[{"id":"a","models":[{"id":"m","match":{"equals":"m","contains":"m"},"prices":{"input_mtok":1}}]}]`,
		"bad expression":   `[{"id":"a","models":[{"id":"m","match":{"regex":"["},"prices":{"input_mtok":1}}]}]`,
		"bad endpoint":     `[{"id":"a","api_pattern":"[","models":[{"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]}]`,
		"no entries":       `[]`,
	} {
		if _, err := Parse([]byte(body), Snapshot{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestClauseFormsAllEvaluate(t *testing.T) {
	for _, tc := range []struct {
		rule string
		in   string
		want bool
	}{
		{`{"equals":"a"}`, "a", true},
		{`{"equals":"a"}`, "ab", false},
		{`{"contains":"b"}`, "abc", true},
		{`{"starts_with":"ab"}`, "abc", true},
		{`{"ends_with":"bc"}`, "abc", true},
		{`{"regex":"^o[134]"}`, "o3-mini", true},
		{`{"or":[{"equals":"x"},{"equals":"y"}]}`, "y", true},
		{`{"and":[{"starts_with":"a"},{"ends_with":"z"}]}`, "abz", true},
		{`{"and":[{"starts_with":"a"},{"ends_with":"z"}]}`, "abc", false},
	} {
		c, err := parseClause(json.RawMessage(tc.rule))
		if err != nil {
			t.Fatalf("%s: %v", tc.rule, err)
		}
		if got := c.matches(tc.in); got != tc.want {
			t.Errorf("%s against %q: got %v, want %v", tc.rule, tc.in, got, tc.want)
		}
	}
}

func TestQuantizeKeepsWhatFitsAndRoundsWhatDoesNot(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		rounded bool
	}{
		{"8", "8", false},
		{"0.5", "0.5", false},
		{"0.000000001", "0.000000001", false},
		// A float artefact of one twelfth, straight out of the dataset. This is
		// the value that makes "just use float64" look harmless.
		{"0.08333333333333334", "0.083333333", true},
		{"0.18000000000000002", "0.18", true},
		{"0.9999999999", "1", true},
		{"0.0000000004", "0", true},
		// The two cases that exercise reassembling a fraction shorter than the
		// stored precision: the leading zeros have to come back as zeros. Pad
		// with spaces there and the result is not a wrong price but an
		// unparseable one, which nothing downstream would accept.
		{"0.0000000014", "0.000000001", true},
		{"0.0000000016", "0.000000002", true},
	} {
		got, rounded, err := quantize(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got != tc.want || rounded != tc.rounded {
			t.Errorf("%s -> %q (rounded %v), want %q (rounded %v)", tc.in, got, rounded, tc.want, tc.rounded)
		}
	}
	// The criterion has to be able to say no, or its silence proves nothing.
	for _, bad := range []string{"1e-07", "-1", ".5", "1.", "", "abc"} {
		if _, _, err := quantize(bad); err == nil {
			t.Errorf("%q was accepted as a rate", bad)
		}
	}
}
