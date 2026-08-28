package proxy_test

import (
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// On the per-image unit every image in the response is billed.
//
// The count comes from the response rather than from `n`, and that is the whole
// point of this family's arithmetic: the two readings differ by as much as
// fifteen and neither announces itself in a bill.
func TestImageUnitsCountEveryImageProduced(t *testing.T) {
	units := proxy.ExportImageUnits([]byte(`{"model":"m","size":"1024x1024"}`), catalog.UnitImage, 4)
	got := units.Quantities[catalog.UnitKey{Unit: catalog.UnitImage, Resolution: "1024x1024"}]
	if got != 4 {
		t.Fatalf("four images produced %d units, want 4", got)
	}
}

// The same four images on a model sold by the generation are one unit, not
// four. That difference is why `call` and `image` are separate units.
func TestImageUnitsCountOneWhenTheModelSellsGenerations(t *testing.T) {
	units := proxy.ExportImageUnits([]byte(`{"model":"m"}`), catalog.UnitCall, 4)
	got := units.Quantities[catalog.UnitKey{Unit: catalog.UnitCall}]
	if got != 1 {
		t.Fatalf("four images on a per-generation model produced %d units, want 1", got)
	}
}

// A count of zero still bills one. Zero reaches here only from a response
// nothing could be read out of, and the caller has already been given the
// images; reading the absence as zero would serve a generation for free.
func TestImageUnitsNeverFallToZero(t *testing.T) {
	units := proxy.ExportImageUnits([]byte(`{"model":"m"}`), catalog.UnitImage, 0)
	got := units.Quantities[catalog.UnitKey{Unit: catalog.UnitImage}]
	if got != 1 {
		t.Fatalf("a count of zero produced %d units, want 1", got)
	}
}

// size and quality select the rate row: they become the resolution and variant
// axes. A body carrying neither matches the card's widest row, which is what an
// upstream default means.
func TestImageUnitsCarryTheRateAxes(t *testing.T) {
	units := proxy.ExportImageUnits([]byte(`{"model":"m","size":"2048x2048","quality":"high"}`), catalog.UnitImage, 1)
	want := catalog.UnitKey{Unit: catalog.UnitImage, Resolution: "2048x2048", Variant: "high"}
	if _, ok := units.Quantities[want]; !ok {
		t.Fatalf("size and quality did not reach the rate key; got %v", units.Quantities)
	}
}

// `n` is not a rate axis and must not become one. It was the quantity once, and
// a body that still carries it has to resolve to the same rate row as one that
// does not -- otherwise the two spellings price differently.
func TestImageUnitsIgnoreN(t *testing.T) {
	withN := proxy.ExportImageUnits([]byte(`{"model":"m","n":4,"size":"1024x1024"}`), catalog.UnitImage, 1)
	withoutN := proxy.ExportImageUnits([]byte(`{"model":"m","size":"1024x1024"}`), catalog.UnitImage, 1)
	key := catalog.UnitKey{Unit: catalog.UnitImage, Resolution: "1024x1024"}
	if withN.Quantities[key] != 1 || withoutN.Quantities[key] != 1 {
		t.Fatalf("`n` changed the quantity: with=%v without=%v", withN.Quantities, withoutN.Quantities)
	}
}

// A malformed body must not become an error here. The body is forwarded either
// way and the upstream is the authority on whether it is valid; refusing here
// would reject requests the vendor accepts.
func TestImageUnitsTolerateAMalformedBody(t *testing.T) {
	units := proxy.ExportImageUnits([]byte(`not json`), catalog.UnitImage, 1)
	if got := units.Quantities[catalog.UnitKey{Unit: catalog.UnitImage}]; got != 1 {
		t.Fatalf("a malformed body produced %d units, want 1", got)
	}
}

// The count is the length of `data`, which is the one thing every OpenAI-shaped
// image API agrees on.
//
// The vendor that made per-image billing worth having has no `n` parameter at
// all: Seedream batches with sequential_image_generation and can return fifteen
// images from a request that names no number. Billing that request from its
// body charged for one.
func TestImagesInResponseCountsTheDataArray(t *testing.T) {
	body := []byte(`{"created":1,"data":[{"b64_json":"a"},{"url":"u"},{"b64_json":"c"}],` +
		`"usage":{"output_tokens":9}}`)
	got, ok := proxy.ExportImagesInResponse(body)
	if !ok || got != 3 {
		t.Fatalf("counted %d (ok=%v), want 3", got, ok)
	}
}

// A response with no `data` array reports that it could not be counted, rather
// than reporting zero.
//
// The difference decides money: the caller has been given whatever the upstream
// produced, so "I could not count" must fall back to the amount held, while a
// zero would settle a served generation at nothing.
func TestImagesInResponseSeparatesAbsentFromEmpty(t *testing.T) {
	if _, ok := proxy.ExportImagesInResponse([]byte(`{"error":{"message":"nope"}}`)); ok {
		t.Error("a body with no data array must report that it could not be counted")
	}
	if n, ok := proxy.ExportImagesInResponse([]byte(`{"data":[]}`)); !ok || n != 0 {
		t.Errorf("an empty data array is a count of zero, not an absence: %d %v", n, ok)
	}
}

// The hold is taken against the most any candidate route could return.
//
// It is taken before a route is picked, so the conservative candidate is the
// only safe one -- the same reasoning catalog.HoldCap uses for token caps. An
// undeclared route counts as one.
func TestMaxImagesTakesTheWidestCandidate(t *testing.T) {
	if got := proxy.ExportMaxImagesOf([]catalog.Route{{MaxImages: 4}, {MaxImages: 15}}); got != 15 {
		t.Errorf("widest candidate: %d, want 15", got)
	}
	if got := proxy.ExportMaxImagesOf([]catalog.Route{{}, {}}); got != 1 {
		t.Errorf("undeclared routes: %d, want 1", got)
	}
	if got := proxy.ExportMaxImagesOf(nil); got != 1 {
		t.Errorf("no routes: %d, want 1", got)
	}
}

// The multipart edits endpoint reaches admission through a rendered probe body,
// because its real body is a stream with the image in it. The rate axes have to
// survive that hop or an edit on a per-image model is quoted at the card's
// widest row while the caller asked for a narrower one.
func TestPeekedProbeBodyCarriesTheBillingFields(t *testing.T) {
	body := string(proxy.ExportPeekedProbeBody(proxy.PeekedMultipart{
		Model: "openai/gpt-image-2", Size: "1024x1024", Quality: "high",
	}))
	for _, want := range []string{`"model":"openai/gpt-image-2"`, `"size":"1024x1024"`, `"quality":"high"`} {
		if !strings.Contains(body, want) {
			t.Errorf("probe body %s is missing %s", body, want)
		}
	}
	// `n` is not a billing field any more, and carrying it here would put a
	// number nothing reads in front of the next reader.
	if strings.Contains(body, `"n"`) {
		t.Errorf("probe body %s still carries n", body)
	}
}

// A field the client did not send stays absent rather than becoming an empty
// string: an empty axis matches any row of the card, and "" written out would
// narrow the lookup to a row spelled with an empty axis instead.
func TestPeekedProbeBodyOmitsWhatWasNotSent(t *testing.T) {
	body := string(proxy.ExportPeekedProbeBody(proxy.PeekedMultipart{Model: "m"}))
	if body != `{"model":"m"}` {
		t.Fatalf("probe body is %s, want only the model", body)
	}
}
