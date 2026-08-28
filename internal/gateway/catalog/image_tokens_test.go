package catalog_test

import (
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// $5 per million input, $30 per million output -- gpt-image-2's text rates.
var imgBase = catalog.Price{InNanoPerMTok: 5_000_000_000, OutNanoPerMTok: 30_000_000_000}

// The whole point of the image bucket, stated as money.
//
// The upstream has always reported image_tokens; nothing here had anywhere to
// put the number, so image input was billed at the text rate. For gpt-image-2
// that is 5 against a real 8 per million, on the bucket an edit request is
// mostly made of.
func TestImageInputBillsAtItsOwnRateWhenOneIsConfigured(t *testing.T) {
	priced := catalog.NewPriceTable(imgBase, []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketImageIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 8_000_000_000},
	})
	// 1000 input tokens of which 800 are image, and no output.
	//   200 text  * 5/M = 1_000_000 nano
	//   800 image * 8/M = 6_400_000 nano
	tokens := catalog.Tokens{In: 1000, ImageIn: 800}
	q, err := catalog.Compute(priced, priced, tokens, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	const want = 1_000_000 + 6_400_000
	if q.ChargedNano != want {
		t.Fatalf("charged %d, want %d", q.ChargedNano, want)
	}
	// And what it used to be, so the size of the leak is on the record: the
	// same request priced entirely at the text rate.
	const allAtTextRate = 5_000_000
	if q.ChargedNano <= allAtTextRate {
		t.Fatalf("charging %d is no more than the text-rate total %d, so the image "+
			"rate did not apply", q.ChargedNano, allAtTextRate)
	}
}

// The regression that matters more than the feature.
//
// Adding a bucket must not move a single existing charge. A model with no image
// rate configured -- which is every model in the catalog today -- has to bill
// exactly as it did before the bucket existed, because BucketImageIn falls back
// to the text input rate. Asserted per bucket rather than on the total: a total
// can come out right with two buckets wrong in opposite directions.
func TestImageTokensChangeNothingWithoutAnImageRate(t *testing.T) {
	plain := catalog.NewPriceTable(imgBase, nil)
	rates := catalog.Rates{FXRate: "1"}

	// The same 1000 input tokens, described two ways: as plain input, and as
	// input of which 800 are image. Both must cost the same.
	asPlainInput, err := catalog.Compute(plain, plain, catalog.Tokens{In: 1000, Out: 100}, rates)
	if err != nil {
		t.Fatal(err)
	}
	withImageBreakdown, err := catalog.Compute(
		plain, plain, catalog.Tokens{In: 1000, Out: 100, ImageIn: 800}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if asPlainInput.ChargedNano != withImageBreakdown.ChargedNano {
		t.Errorf("reporting the image breakdown changed the charge: %d became %d",
			asPlainInput.ChargedNano, withImageBreakdown.ChargedNano)
	}
	if asPlainInput.UpstreamUSDNano != withImageBreakdown.UpstreamUSDNano {
		t.Errorf("reporting the image breakdown changed the cost: %d became %d",
			asPlainInput.UpstreamUSDNano, withImageBreakdown.UpstreamUSDNano)
	}
	// Per bucket as well as in total: a total can come out right with two
	// buckets wrong in opposite directions, which is exactly what a botched
	// subtraction looks like.
	plainParts, err := catalog.ComputeExactContributions(
		plain, plain, catalog.Tokens{In: 1000, Out: 100}, rates)
	if err != nil {
		t.Fatal(err)
	}
	imageParts, err := catalog.ComputeExactContributions(
		plain, plain, catalog.Tokens{In: 1000, Out: 100, ImageIn: 800}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if plainParts != imageParts {
		t.Errorf("per-bucket contributions moved:\n%+v\nbecame\n%+v", plainParts, imageParts)
	}
}

// Image and audio are both subsets of the input count and are subtracted
// together, so a request carrying both must not have its text remainder
// counted twice over.
func TestImageAndAudioAreBothSubtractedFromTheTextRemainder(t *testing.T) {
	table := catalog.NewPriceTable(imgBase, []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketImageIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 8_000_000_000},
		{Bucket: catalog.BucketAudioIn, ServiceTier: catalog.TierStandard, NanoPerMtok: 10_000_000_000},
	})
	// 1000 input: 500 image, 300 audio, 200 text.
	//   200 text  *  5/M = 1_000_000
	//   500 image *  8/M = 4_000_000
	//   300 audio * 10/M = 3_000_000
	q, err := catalog.Compute(
		table, table,
		catalog.Tokens{In: 1000, ImageIn: 500, AudioIn: 300},
		catalog.Rates{FXRate: "1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = 1_000_000 + 4_000_000 + 3_000_000
	if q.ChargedNano != want {
		t.Fatalf("charged %d, want %d", q.ChargedNano, want)
	}
}

// The subset relation is what makes the subtraction correct, so a report that
// contradicts it is refused rather than billed.
func TestImageTokensMustBeASubsetOfInput(t *testing.T) {
	if err := catalog.ValidateTokens(catalog.Tokens{In: 100, ImageIn: 101}); err == nil {
		t.Fatal("more image tokens than input tokens should not validate")
	}
	if err := catalog.ValidateTokens(catalog.Tokens{In: 100, ImageIn: 100}); err != nil {
		t.Fatalf("all-image input is a real shape for an image request: %v", err)
	}
}

// The output side, which is the larger of the two leaks.
//
// A generated image is reported as output tokens, and both upstreams that
// generate them charge those far above their text output: the reference dataset
// states 30 USD/Mtok against a text output of 2.5 for one Gemini image model,
// and 32 against 10 for gpt-image. Folded into Out, every image generated was
// billed at somewhere between a third and a twelfth of its cost.
func TestImageOutputBillsAtItsOwnRateWhenOneIsConfigured(t *testing.T) {
	priced := catalog.NewPriceTable(imgBase, []gwdb.ModelPriceDimensionRate{
		{Bucket: catalog.BucketImageOut, ServiceTier: catalog.TierStandard, NanoPerMtok: 96_000_000_000},
	})
	// 1000 output tokens of which 900 are the image itself, and no input.
	//   100 text  * 30/M = 3_000_000 nano
	//   900 image * 96/M = 86_400_000 nano
	tokens := catalog.Tokens{Out: 1000, ImageOut: 900}
	q, err := catalog.Compute(priced, priced, tokens, catalog.Rates{FXRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	const want = 3_000_000 + 86_400_000
	if q.ChargedNano != want {
		t.Fatalf("charged %d, want %d", q.ChargedNano, want)
	}
	const allAtTextRate = 30_000_000
	if q.ChargedNano <= allAtTextRate {
		t.Fatalf("charging %d is no more than the text-rate total %d, so the image "+
			"output rate did not apply", q.ChargedNano, allAtTextRate)
	}
}

// image_out falls back to *output*, not input.
//
// This is the arm that is easy to get wrong: the two image buckets sit next to
// each other and the other one belongs to input, so putting both in the input
// arm looks tidy and bills every generated image at the price of reading one.
// With gpt-image's 5-in / 30-out that is a sixth of the correct charge, and
// nothing about the number says so.
func TestImageOutputFallsBackToTheOutputRateNotTheInputRate(t *testing.T) {
	plain := catalog.NewPriceTable(imgBase, nil)
	rates := catalog.Rates{FXRate: "1"}

	withImage, err := catalog.Compute(plain, plain, catalog.Tokens{Out: 1000, ImageOut: 1000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	asPlainOutput, err := catalog.Compute(plain, plain, catalog.Tokens{Out: 1000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if withImage.ChargedNano != asPlainOutput.ChargedNano {
		t.Fatalf("1000 image output tokens charged %d, plain output charged %d -- "+
			"an unpriced image_out bucket must fall back to the output rate",
			withImage.ChargedNano, asPlainOutput.ChargedNano)
	}
	// And it must not have landed on the input side, which is the actual slip.
	asPlainInput, err := catalog.Compute(plain, plain, catalog.Tokens{In: 1000}, rates)
	if err != nil {
		t.Fatal(err)
	}
	if withImage.ChargedNano == asPlainInput.ChargedNano {
		t.Fatalf("1000 image output tokens charged %d, the same as 1000 *input* tokens -- "+
			"image_out fell back to the input rate", withImage.ChargedNano)
	}
}
