package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The bundled dataset has to be usable from the binary with no arguments at
// all: the whole point of compiling it in is that an operator inside a
// container with no shell can still run this.
func TestBundledDatasetIsReadableWithNoArguments(t *testing.T) {
	d, err := loadReferenceDataset("", nil)
	if err != nil {
		t.Fatalf("load the bundled dataset: %v", err)
	}
	if d.Entries == 0 {
		t.Fatal("the bundled dataset is empty")
	}
	if d.Snapshot.Dataset == "" || d.Snapshot.SnapshotDate == "" {
		t.Errorf("the bundled snapshot record is incomplete: %+v", d.Snapshot)
	}
}

// A dataset handed in on stdin is recorded as what it is. Stamping it with the
// bundled dataset's name and date would put a provenance on those rows that
// nobody checked.
func TestASuppliedDatasetIsRecordedAsSupplied(t *testing.T) {
	body := `[{"id":"acme","models":[
	  {"id":"m","match":{"equals":"m"},"prices":{"input_mtok":1}}]}]`
	d, err := loadReferenceDataset("-", strings.NewReader(body))
	if err != nil {
		t.Fatalf("load from stdin: %v", err)
	}
	if d.Snapshot.Dataset != suppliedDatasetName {
		t.Errorf("dataset name %q, want %q", d.Snapshot.Dataset, suppliedDatasetName)
	}
	if d.Snapshot.UpstreamCommit != "" {
		t.Error("a supplied file was stamped with an upstream revision nobody can check")
	}
	if len(d.Snapshot.SHA256) != 64 {
		t.Errorf("no digest was recorded for the supplied bytes: %q", d.Snapshot.SHA256)
	}
}

func TestPricingCommandRefusesContradictorySources(t *testing.T) {
	var out bytes.Buffer
	for name, args := range map[string][]string{
		"two sources": {"import", "--bundled", "--file", "/tmp/x.json"},
		"two modes":   {"import", "--only-unpriced", "--force"},
		"no verb":     {},
		"wrong verb":  {"export"},
	} {
		if err := pricingCmd(args, nil, &out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The report is the only record an operator gets of a run, so the models it
// declined to price have to be in it, by name and with the reason.
func TestReportNamesWhatWasNotPriced(t *testing.T) {
	rep := &gwstaffapi.ImportReport{
		Snapshot:       refdata.Snapshot{Dataset: "test-prices", SnapshotDate: "2026-08-17"},
		DatasetEntries: 3,
		Acknowledged:   []string{"customer_price_drop"},
		Results: []gwstaffapi.ImportResult{
			{ModelSlug: "acme/large", Outcome: gwstaffapi.ImportPriced, Detail: "acme/acme-large matched by equals"},
			{ModelSlug: "acme/odd", Outcome: gwstaffapi.ImportSkipped, Detail: "route acme-odd on acme: no reference entry"},
			{ModelSlug: "acme/checked", Outcome: gwstaffapi.ImportVerified, Detail: "checked by a human on 2026-01-02"},
		},
	}
	var out bytes.Buffer
	writeImportReport(&out, rep)
	got := out.String()

	for _, want := range []string{
		"test-prices, snapshot 2026-08-17",
		"acme/large", "acme/acme-large matched by equals",
		"acme/odd", "no reference entry",
		"customer_price_drop",
		// Nothing written here has been checked by anyone, and the operator has
		// to be told so in the same breath as the counts.
		"None of these prices is marked as checked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not mention %q:\n%s", want, got)
		}
	}
	// The counts have to be right, not merely present. Read back by splitting
	// on whitespace rather than by matching the padding, so that adjusting the
	// column widths does not read as a wrong number.
	counts := map[string]string{}
	for _, line := range strings.Split(got, "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			counts[f[0]] = f[1]
		}
	}
	for label, want := range map[string]string{
		"priced": "1", "skipped": "1", "verified": "1", "kept": "0", "updated": "0",
	} {
		if counts[label] != want {
			t.Errorf("%s counted %q, want %q:\n%s", label, counts[label], want, got)
		}
	}
}

// A run that wrote nothing must not end with a line telling the operator to go
// and verify prices that do not exist.
func TestReportStaysQuietWhenNothingWasWritten(t *testing.T) {
	var out bytes.Buffer
	writeImportReport(&out, &gwstaffapi.ImportReport{
		Snapshot: refdata.Snapshot{Dataset: "test-prices"},
		Results: []gwstaffapi.ImportResult{
			{ModelSlug: "acme/odd", Outcome: gwstaffapi.ImportSkipped, Detail: "no reference entry"},
		},
	})
	if strings.Contains(out.String(), "marked as checked") {
		t.Errorf("a run that wrote nothing still asks for prices to be verified:\n%s", out.String())
	}
}

// A dry run's report has to read as a dry run line by line, not only in a
// banner at the top. The counts are what a reader acts on, and "priced 1 --
// had no price, now priced" is a completed action however the header was
// worded; a reader who skims the table has been told something untrue.
func TestDryRunReportSpeaksInTheConditional(t *testing.T) {
	rep := &gwstaffapi.ImportReport{
		Snapshot:       refdata.Snapshot{Dataset: "test-prices", SnapshotDate: "2026-08-17"},
		DatasetEntries: 3,
		DryRun:         true,
		Results: []gwstaffapi.ImportResult{
			{ModelSlug: "acme/large", Outcome: gwstaffapi.ImportPriced, Detail: "acme/acme-large matched by equals"},
		},
	}
	var out bytes.Buffer
	writeImportReport(&out, rep)
	got := out.String()

	wanted := []string{
		"DRY RUN", "would be priced", "Would price", "acme/large", "Nothing above was stored",
	}
	for _, want := range wanted {
		if !strings.Contains(got, want) {
			t.Errorf("the dry-run report does not say %q:\n%s", want, got)
		}
	}
	// The completed tense must be gone, not merely accompanied by a warning.
	for _, unwanted := range []string{"now priced", "None of these prices is marked as checked"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the dry-run report still claims %q happened:\n%s", unwanted, got)
		}
	}

	// The reverse reading: the same results without the flag do say it happened.
	// Without this, the assertions above would also pass on a report that said
	// nothing at all.
	rep.DryRun = false
	out.Reset()
	writeImportReport(&out, rep)
	if applied := out.String(); !strings.Contains(applied, "now priced") ||
		strings.Contains(applied, "DRY RUN") {
		t.Errorf("a real run's report does not read as a completed action:\n%s", applied)
	}
}
