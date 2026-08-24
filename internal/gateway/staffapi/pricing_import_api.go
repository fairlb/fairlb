package gwstaffapi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/internal/gateway/pricing/refdata"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// The reference-price import, offered to somebody signed in.
//
// The same run is reachable from the command line, and the two differ in one
// way that matters: this one has an identity. That is why it exists at all.
// A command line inside a container has nobody to record, so its runs are
// traceable only through what they write on each row; a request arrives with a
// signed-in operator behind it, so it leaves an audit record like every other
// write on this plane and names that operator on every row it stores.
//
// What does not differ: no row it writes is marked as verified, a price a
// person has checked is never replaced, and every model it declined to price
// comes back named with the reason.

// ReferencePriceImportService is an optional capability of the pricing service.
//
// importOutcomeOut maps a run's outcome onto the contract's.
//
// A table rather than a cast between the two string types. They are declared in
// two documents that are edited at different times, and a cast is a promise
// that nothing checks: rename one enum's member and the wire keeps carrying the
// old spelling, which every reader downstream then treats as an unknown state.
// Totality is asserted in the tests.
var importOutcomeOut = map[ImportOutcome]ReferencePriceImportResultOutcome{
	ImportPriced:    Priced,
	ImportUpdated:   Updated,
	ImportUnchanged: Unchanged,
	ImportKept:      Kept,
	ImportVerified:  Verified,
	ImportSkipped:   Skipped,
}

// ImportGatewayReferencePrices fills empty model prices in from the dataset
// bundled with this build.
func (s *Server) ImportGatewayReferencePrices(
	ctx context.Context, req ImportGatewayReferencePricesRequestObject,
) (ImportGatewayReferencePricesResponseObject, error) {
	// The same bar as saving one price by hand. Writing a price is publishing
	// it, and writing several hundred in one request does not lower that bar --
	// the argument for the highest privilege gets stronger with the count, not
	// weaker.
	actor, err := httpx.RequireSuperadmin(ctx)
	if err != nil {
		return nil, err
	}
	// The dataset is compiled in and its digest is checked as it loads, so a
	// failure here means the build itself carries a dataset that does not match
	// its own record -- which is worth refusing rather than importing.
	data, err := refdata.Bundled()
	if err != nil {
		return nil, fmt.Errorf("gwadmin: read the bundled reference prices: %w", err)
	}

	opts := ImportOptions{Data: data, Now: time.Now().UTC(), Actor: actor}
	if b := req.Body; b != nil {
		opts.Force = b.Force != nil && *b.Force
		opts.DryRun = b.DryRun != nil && *b.DryRun
		if b.ModelIds != nil {
			opts.Models = make([]uuid.UUID, 0, len(*b.ModelIds))
			opts.Models = append(opts.Models, *b.ModelIds...)
		}
	}

	report, err := s.pricingAdmin.ImportReferencePrices(ctx, opts)
	if err != nil {
		// A run can stop after writing some of its rows. The report cannot
		// travel with a failure, so the one fact that must not be lost -- that
		// rows were stored -- is logged and named in the error: an operator who
		// reads "the import failed" and assumes nothing happened will go on to
		// do the wrong thing next.
		written := 0
		if report != nil {
			written = report.Count(ImportPriced) + report.Count(ImportUpdated)
		}
		slog.ErrorContext(ctx, "reference price import stopped early",
			"prices_written", written, "error", err)
		return nil, fmt.Errorf(
			"gwadmin: the reference price import stopped after writing %d prices: %w", written, err)
	}
	return ImportGatewayReferencePrices200JSONResponse(importReportOut(report)), nil
}

// importReportOut maps the run onto the wire.
func importReportOut(r *ImportReport) ReferencePriceImportReport {
	entries := r.DatasetEntries
	out := ReferencePriceImportReport{
		Dataset: r.Snapshot.Dataset,
		Entries: &entries,
		DryRun:  r.DryRun,
		Results: make([]ReferencePriceImportResult, 0, len(r.Results)),
	}
	if r.Snapshot.SnapshotDate != "" {
		date := r.Snapshot.SnapshotDate
		out.SnapshotDate = &date
	}
	if len(r.Acknowledged) > 0 {
		acked := r.Acknowledged
		out.Acknowledged = &acked
	}
	for _, res := range r.Results {
		out.Results = append(out.Results, ReferencePriceImportResult{
			Model:   res.ModelSlug,
			Outcome: importOutcomeOut[res.Outcome],
			Detail:  res.Detail,
		})
	}
	return out
}
