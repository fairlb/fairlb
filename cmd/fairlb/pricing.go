package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fairlb/fairlb/foundation/drivers"
	"github.com/fairlb/fairlb/gateway"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
	"github.com/fairlb/fairlb/settings"
)

// `fairlb pricing import` prefills model prices from a reference dataset.
//
// It exists because of what a fresh install looks like: no models, no prices,
// and a model without a price is refused rather than served for free. Getting
// to a first request therefore means copying four numbers per model out of a
// vendor's price page and into a form, which is transcription — and a dropped
// digit in transcription is a billing error nobody notices until the invoice.
//
// This is a subcommand rather than a background job on purpose. Prices reach
// this deployment when its operator decides they should, never on their own,
// and what lands is ordinary configuration: unverified, attributed, and
// editable in the console like anything else.
//
// A file supplied with --file has to be readable from inside the container,
// since the image has no shell to fetch one with. `--file -` reads stdin, which
// is usually the easier half of that.

// suppliedDatasetName is what a price row records as its source when the data
// came from a file rather than from the copy bundled with this binary. Naming
// it after the bundled dataset would assert something nobody checked; the
// digest recorded alongside it is what actually identifies the bytes.
const suppliedDatasetName = "reference-prices"

func pricingCmd(args []string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fairlb pricing import [--file <path>|-] [--force] [--dry-run]")
	}
	if args[0] != "import" {
		return fmt.Errorf("unknown pricing command %q (want import)", args[0])
	}

	fs := flag.NewFlagSet("fairlb pricing import", flag.ContinueOnError)
	fs.SetOutput(out)
	bundled := fs.Bool("bundled", false,
		"use the reference prices bundled with this binary (the default)")
	file := fs.String("file", "",
		"read the reference prices from this file instead, or from stdin with -")
	force := fs.Bool("force", false,
		"also overwrite stored prices that nobody has verified; prices a person "+
			"has confirmed are never overwritten")
	onlyUnpriced := fs.Bool("only-unpriced", false,
		"only fill in models that have no price at all (the default)")
	dryRun := fs.Bool("dry-run", false,
		"print what would happen to every model and write nothing")
	if err := fs.Parse(args[1:]); err != nil {
		// -h is a request that was granted, not a failure. Returning it as an
		// error would print the flag list and then exit non-zero, which is the
		// sort of thing that quietly fails a deployment script.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *bundled && *file != "" {
		return errors.New("--bundled and --file name two different sources; pass one")
	}
	if *onlyUnpriced && *force {
		return errors.New("--only-unpriced and --force ask for opposite things; pass one")
	}

	data, err := loadReferenceDataset(*file, stdin)
	if err != nil {
		return err
	}

	ctx := context.Background()
	cfg, pool, err := connectRuntime(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	// A dry run keeps no rows, so there is nothing to invalidate and no reason
	// to open a second connection to Redis just to find that out. Skipping it
	// also keeps the rehearsal from complaining about a dependency the thing it
	// is rehearsing would not have needed.
	//
	// The import has to invalidate the price cache, not just write the rows.
	//
	// A running gateway keeps its own copy of the catalog for a minute, so
	// without this a --force run that *replaces* a price leaves that minute
	// billing at the price it just replaced. The default run only turns a
	// refusal into a price and is harmless either way, but the two share a code
	// path and only one of them is safe to leave alone.
	//
	// The invalidation reaches other processes as well as this one: the
	// in-process cache broadcasts deletions over the database, so the gateway
	// serving traffic drops its copy even though this command is a separate
	// process that will exit in a moment.
	var cat *catalog.Service
	if !*dryRun {
		if drv, derr := drivers.New(ctx, cfg.Config, pool); derr != nil {
			// Not fatal. The rows are the point; the cache is a one-minute
			// staleness window that expires on its own, and refusing to import
			// because Redis is unwell would be the worse trade.
			fmt.Fprintf(os.Stderr, "warning: cache invalidation is unavailable (%v); "+
				"a running gateway may serve the previous prices for up to a minute\n", derr)
		} else {
			defer func() { _ = drv.Close() }()
			cat = catalog.NewService(gwdb.New(pool), drv.Cache, settings.New(pool, drv.Cache, settings.NewRegistry(gateway.SettingSpecs()), nil))
		}
	}

	report, err := gwstaffapi.ImportReferencePrices(ctx, gwstaffapi.ReferencePriceImportConfig{
		Pool: pool, Catalog: cat,
		Options: gwstaffapi.ImportOptions{
			Data: data, Force: *force, DryRun: *dryRun, Now: time.Now().UTC(),
		},
	})
	if report != nil {
		// Printed even when the run stopped early. A partially applied import
		// that reports nothing is the worst of both: rows were written, and the
		// operator has no idea which.
		writeImportReport(out, report)
	}
	return err
}

// loadReferenceDataset picks the bundled dataset or the one the operator
// supplied.
func loadReferenceDataset(file string, stdin io.Reader) (*refdata.Dataset, error) {
	if file == "" {
		return refdata.Bundled()
	}
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, fmt.Errorf("read the reference prices: %w", err)
	}
	sum := sha256.Sum256(raw)
	// The provenance written for a supplied file states only what is actually
	// known: these bytes, on this day. Everything else the bundled record
	// carries — which upstream release, taken when — would be a guess.
	return refdata.Parse(raw, refdata.Snapshot{
		Dataset:      suppliedDatasetName,
		SnapshotDate: time.Now().UTC().Format(time.DateOnly),
		SHA256:       hex.EncodeToString(sum[:]),
	})
}

// writeImportReport prints what the run did.
//
// The counts come first because that is the question, but the lists below them
// are the point: a model the import declined to price is named, with the
// reason. Leaving those silent would reproduce exactly the situation this
// command exists to end — a model that refuses traffic and no clue why.
func writeImportReport(w io.Writer, r *gwstaffapi.ImportReport) {
	snapshot := r.Snapshot.Dataset
	if r.Snapshot.SnapshotDate != "" {
		snapshot += ", snapshot " + r.Snapshot.SnapshotDate
	}
	_, _ = fmt.Fprintf(w, "\nReference prices: %s (%d entries)\n\n", snapshot, r.DatasetEntries)
	// Said before the counts rather than after them, because the counts are
	// what a reader acts on: "priced 40 models" reads exactly the same whether
	// or not the rows survived, and by the time a footnote arrives the reader
	// has already believed the table above it.
	if r.DryRun {
		_, _ = fmt.Fprint(w, "DRY RUN — nothing was written. The counts below are what a real\n"+
			"run would do, decided by the same checks, then rolled back.\n\n")
	}

	// Every line that reports an action is written twice, in the two tenses,
	// rather than once with the dry-run notice standing in for the difference.
	// A banner is read once and a table is read line by line, so a table that
	// says "now priced" under a banner saying nothing happened is read as the
	// first of those two.
	for _, row := range []struct {
		outcome gwstaffapi.ImportOutcome
		note    string
		wouldBe string
	}{
		{gwstaffapi.ImportPriced, "had no price, now priced", "has no price, would be priced"},
		{gwstaffapi.ImportUpdated, "unverified price replaced", "unverified price would be replaced"},
		{gwstaffapi.ImportUnchanged, "already equal to the reference", ""},
		{gwstaffapi.ImportKept, "already priced; --force replaces unverified ones", ""},
		{gwstaffapi.ImportVerified, "checked by a person; never overwritten", ""},
		{gwstaffapi.ImportSkipped, "no price written", "no price would be written"},
	} {
		note := row.note
		// An empty second form means the line already reads correctly either
		// way: "already equal to the reference" describes a state the run found,
		// not something it did.
		if r.DryRun && row.wouldBe != "" {
			note = row.wouldBe
		}
		_, _ = fmt.Fprintf(w, "  %-11s %4d   %s\n", row.outcome, r.Count(row.outcome), note)
	}

	priced, replaced := "Priced", "Replaced"
	if r.DryRun {
		priced, replaced = "Would price", "Would replace"
	}
	writeImportSection(w, priced, r.Of(gwstaffapi.ImportPriced))
	writeImportSection(w, replaced, r.Of(gwstaffapi.ImportUpdated))
	writeImportSection(w, "Not priced", r.Of(gwstaffapi.ImportSkipped))

	if len(r.Acknowledged) > 0 {
		_, _ = fmt.Fprintf(w, "\nConfirmed on your behalf (the console would have asked): %s\n",
			strings.Join(r.Acknowledged, ", "))
	}
	if r.DryRun {
		_, _ = fmt.Fprint(w, "\nNothing above was stored. Run the same command without --dry-run "+
			"to apply it.\n")
		return
	}
	if r.Count(gwstaffapi.ImportPriced)+r.Count(gwstaffapi.ImportUpdated) > 0 {
		_, _ = fmt.Fprint(w, "\nNone of these prices is marked as checked. Compare each one against the "+
			"vendor's own\nprice list and confirm it in the console; until then it is a reference, "+
			"not a rate\nyou have agreed to charge against.\n")
	}
}

func writeImportSection(w io.Writer, title string, rows []gwstaffapi.ImportResult) {
	if len(rows) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n%s:\n", title)
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "  %-32s %s\n", row.ModelSlug, row.Detail)
	}
}
