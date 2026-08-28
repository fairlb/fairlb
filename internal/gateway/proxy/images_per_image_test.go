package proxy_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// seedPerImageModel configures a model billed by produced image on the images
// surface, on a provider that speaks OpenAI.
//
// The combination is the point: this is a per-unit price row on a surface that
// also carries token-billed models, which is the configuration that answered
// 404 to every request before the billing family moved to the price row
// (ADR-0227). The fixture is itself part of what is under test.
func (f *pipeFixture) seedPerImageModel(t *testing.T, slug string, nanoPerImage int64) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var provID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('p-ark', 'volcengine', ARRAY['openai'], $1) RETURNING id`,
		f.upstream.URL).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.box.Seal([]byte("sk-upstream-secret"), provID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', $2) RETURNING id`,
		provID, sealed).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	resealed, err := f.box.Seal([]byte("sk-upstream-secret"), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE provider_keys SET secret_enc = $2 WHERE id = $1`, keyID, resealed); err != nil {
		t.Fatal(err)
	}

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug, output_modalities) VALUES ($1, ARRAY['image']) RETURNING id`,
		slug).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	// Four explicit zero token buckets and pricing_family = units: this model
	// has no token price, and NULL would mean "unknown" rather than "absent".
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode, pricing_family,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			multiplier_bps, source_name)
		VALUES ($1, 'paid', 'units', 0, 0, 0, 0, 10000, 'test-fixture')`, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model_price_unit_rates (model_id, unit, resolution, nano_per_unit)
		 VALUES ($1, 'image', '1024x1024', $2)`, modelID, nanoPerImage); err != nil {
		t.Fatal(err)
	}
	// The images endpoint is never probed automatically, so a route is a
	// candidate only once a verdict says ok -- which is what an operator does
	// by hand today. Without this the model resolves to nothing.
	routeID := catalogtest.SeedRoute(t, f.pool, modelID, provID, "seedream-4-0", "images")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status)
		VALUES ($1, 'images', 'openai', 'manual', 'ok')
		ON CONFLICT (route_id, endpoint) DO UPDATE SET status = 'ok'`, routeID); err != nil {
		t.Fatal(err)
	}
	// This vendor's real ceiling: reference images plus generated images may
	// not exceed fifteen. It is what the hold is taken against, because how
	// many images a request comes back with is not knowable until it does.
	if _, err := f.pool.Exec(ctx,
		`UPDATE model_routes SET max_images = 15 WHERE id = $1`, routeID); err != nil {
		t.Fatal(err)
	}
	return modelID
}

// A per-image model is charged for the images it produced, and the reservation
// covers the most it could have produced.
//
// Four properties in one request, each broken in its own way at some point: the
// model resolved at all (it answered 404), a hold was taken (there was none),
// the usage row recorded the quantity (nothing wrote billed_units on this
// path), and the charge follows the response rather than the request.
//
// Held and charged are deliberately *different* numbers here. The hold is the
// route's declared ceiling of fifteen; the charge is the three images that came
// back. Holding the charge instead would mean knowing the count before the
// request was made, and on this vendor nothing in the request says it.
func TestPerImageModelChargesForWhatWasProduced(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately carrying a usage object. A per-image model is not billed
		// from it, and pricing it as well would charge twice.
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"a"},{"b64_json":"b"},{"b64_json":"c"}],
			"usage":{"input_tokens":100,"output_tokens":9999}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000 // $0.04
	f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"a blue square",` +
			`"n":3,"size":"1024x1024"}`),
		Credential: plaintext,
	})
	if gerr != nil {
		t.Fatalf("a per-image model on the images surface should serve: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}

	const want = 3 * nanoPerImage
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != want {
		t.Fatalf("three images at $0.04 should settle %d: %+v", want, st)
	}
	// The reservation covers the ceiling, not the count. A hold of one image
	// would let this organization pass a budget check on the strength of one
	// image and then be charged for fifteen.
	const wantHeld = 15 * nanoPerImage
	held, ok := f.settler.LastHold()
	if !ok || held.AmountNano != wantHeld {
		t.Fatalf("the hold should cover the route's ceiling %d: %+v", wantHeld, held)
	}

	var billedUnits pgtype.Int4
	var billedUnit string
	var tokensOut int32
	if err := f.pool.QueryRow(ctx,
		`SELECT billed_units, billed_unit, tokens_out FROM usage_logs WHERE org_id = $1`, org).
		Scan(&billedUnits, &billedUnit, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if !billedUnits.Valid || billedUnits.Int32 != 3 || billedUnit != "image" {
		t.Errorf("usage row should record 3 images: units=%+v unit=%q", billedUnits, billedUnit)
	}
	// The upstream's token counts are not this model's bill and must not be
	// recorded as though they were: 9999 output tokens against a per-image
	// model is a number that denotes nothing.
	if tokensOut != 0 {
		t.Errorf("tokens_out=%d on a per-image request; the upstream's token report "+
			"is not what this model is billed on", tokensOut)
	}
}

// A size the rate card does not price is refused before anything is reserved.
//
// The card *is* the envelope on this surface: pricing already has to read the
// size to look up a rate, and "no rate for this value" and "the upstream does
// not support this value" are the same fact from here.
func TestPerImageModelRefusesASizeItCannotPrice(t *testing.T) {
	var upstreamCalls int
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"a"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedPerImageModel(t, "bytedance/seedream-4-0", 40_000_000)

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"x",` +
			`"size":"4096x4096"}`),
		Credential: plaintext,
	})
	if gerr == nil {
		t.Fatal("a size with no rate must be refused, not billed at some other row's price")
	}
	if upstreamCalls != 0 {
		t.Errorf("the upstream was called %d times; the refusal must come before any "+
			"money or any generation", upstreamCalls)
	}
}

// `"stream": true` on a per-image model is refused, before a hold and before a
// generation.
//
// It used to be served, and served wrongly. The charge on this family is the
// number of images produced, and a stream has no place that number can be read
// from: this vendor emits one `image_generation.partial_succeeded` per finished
// image and a terminal `completed`, while the other emits `completed` per image
// with up to three `partial_image` renders before each -- and both spell the
// payload `b64_json`, so the frames cannot be told apart by shape either.
// Counting would overcharge on one vendor and undercharge on the other, and the
// old arithmetic did neither: it billed whatever `n` said, which on this vendor
// is not a parameter at all.
//
// So the answer is the one the Gemini array-streaming form gets, for the same
// reason: a shape this gateway cannot meter is not served. Dropping `stream`
// returns the identical images.
func TestPerImageModelRefusesToStream(t *testing.T) {
	upstreamCalls := 0
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedPerImageModel(t, "bytedance/seedream-4-0", 40_000_000)

	rec := httptest.NewRecorder()
	gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"a blue square",` +
			`"size":"1024x1024","stream":true}`),
		Credential: plaintext, Stream: true,
	}, proxy.SurfaceOpenAI)
	if gerr == nil {
		t.Fatal("a streamed per-image request must be refused, not billed on a count nobody can read")
	}
	// Before any money and before any generation: a refusal that arrived after
	// the upstream had produced the images would leave the customer's images
	// paid for by us.
	if upstreamCalls != 0 {
		t.Errorf("the upstream was called %d times; the refusal must come first", upstreamCalls)
	}
	if len(f.settler.Holds) != 0 {
		t.Errorf("a refused request must not take a hold: %+v", f.settler.Holds)
	}
	// A refusal does get a usage row -- that is how an operator sees refusals
	// at all -- but it carries nothing billable. Asserting the row exists *and*
	// bills zero is the pair that matters: a missing row hides the refusal, and
	// a billed one charges for images nobody generated.
	var billedUnits pgtype.Int4
	var chargedNano int64
	if err := f.pool.QueryRow(ctx,
		`SELECT billed_units, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&billedUnits, &chargedNano); err != nil {
		t.Fatalf("a refusal should still be visible in the usage log: %v", err)
	}
	if billedUnits.Valid && billedUnits.Int32 != 0 {
		t.Errorf("a refused request billed %+v units", billedUnits)
	}
	if chargedNano != 0 {
		t.Errorf("a refused request was charged %d", chargedNano)
	}
}

// A per-image model on the organization's own credential owes the service fee,
// not the list price.
//
// The sibling of TestPipelineBYOKChargesServiceFeeNotListPrice, on the family
// beside it. Both live on the images surface, so a caller can watch the two
// models disagree: without this the token-billed one charges 5% of the vendor
// rate on a BYOK request while the per-image one charges the vendor rate in
// full, twenty times as much, on the same endpoint and the same credential.
func TestPerImageModelOnItsOwnCredentialPaysTheServiceFee(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"a"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000 // $0.04
	f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)
	f.seedBYOK(t, org, "volcengine")

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"x",` +
			`"size":"1024x1024"}`),
		Credential: plaintext, RequestID: "byok-image-fee",
	}); gerr != nil {
		t.Fatal(gerr)
	}

	var charged, cost int64
	var byok bool
	if err := f.pool.QueryRow(ctx, `
		SELECT charged_nano, upstream_cost_usd_nano, byok
		  FROM usage_logs WHERE request_id = 'byok-image-fee'`).
		Scan(&charged, &cost, &byok); err != nil {
		t.Fatal(err)
	}
	// The positive control: without it the assertions below could hold on an
	// implementation that never reached the organization's credential at all.
	if !byok {
		t.Fatal("this hop should be recorded as using a organization credential")
	}
	// An independent expected value, not a reading of the code: one image at
	// $0.04 with the default 500 bps fee is 2_000_000 nano. The sales
	// multiplier does not apply -- the fee is itself the rate.
	const wantFee = nanoPerImage * 500 / 10_000
	if charged != wantFee {
		t.Errorf("a per-image model on a organization credential should be charged the %d nano "+
			"service fee, not the %d nano list price: charged=%d", wantFee, nanoPerImage, charged)
	}
	// Nothing was bought from an upstream, so recording a cost would skew the
	// margin column.
	if cost != 0 {
		t.Errorf("a organization credential must record no upstream cost: %d", cost)
	}
}

// The batch this whole change exists for: fifteen images from a request that
// names no number.
//
// Seedream has no `n` parameter. Its batch mode is
// sequential_image_generation with max_images, and it can return fifteen
// images. Billing from the request read no `n`, defaulted to one, and charged
// one image for fifteen -- a fifteenfold undercharge on a model whose whole
// selling point is that batch.
func TestPerImageModelChargesForABatchTheRequestNeverNamed(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		var data string
		for i := range 15 {
			if i > 0 {
				data += ","
			}
			data += `{"url":"https://example.invalid/i.png"}`
		}
		_, _ = w.Write([]byte(`{"created":1,"data":[` + data + `]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000 // $0.04
	f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		// The vendor's own batch spelling, and not one field of it is `n`.
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"a set of icons",` +
			`"size":"1024x1024","sequential_image_generation":"auto",` +
			`"sequential_image_generation_options":{"max_images":15}}`),
		Credential: plaintext,
	}); gerr != nil {
		t.Fatalf("the batch should serve: %v", gerr)
	}

	const want = 15 * nanoPerImage
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != want {
		t.Fatalf("fifteen images at $0.04 should settle %d, not %d: %+v", want, st.ActualNano, st)
	}
	var billedUnits pgtype.Int4
	if err := f.pool.QueryRow(ctx,
		`SELECT billed_units FROM usage_logs WHERE org_id = $1`, org).Scan(&billedUnits); err != nil {
		t.Fatal(err)
	}
	if !billedUnits.Valid || billedUnits.Int32 != 15 {
		t.Errorf("usage row should record 15 images: %+v", billedUnits)
	}
}

// The other direction of the same defect: an `n` the upstream ignored.
//
// `n` is OpenAI's word, and a vendor that does not have it answers with one
// image regardless. Billing from the request charged four; the response says
// one, and one is what was delivered.
func TestPerImageModelDoesNotChargeForImagesTheUpstreamDidNotReturn(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"only-one"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000
	f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"a blue square",` +
			`"n":4,"size":"1024x1024"}`),
		Credential: plaintext,
	}); gerr != nil {
		t.Fatalf("the request should serve: %v", gerr)
	}

	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != nanoPerImage {
		t.Fatalf("one image came back; the charge should be %d, not %d: %+v",
			nanoPerImage, st.ActualNano, st)
	}
}

// A response nothing can be counted out of settles at the hold, not at zero.
//
// The images were generated and the upstream has charged for them, so an
// uncountable body must fall the conservative way. Zero would give a generation
// away; the hold never exceeds what the organization was already checked
// against.
func TestPerImageModelWithAnUncountableResponseSettlesAtTheHold(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		// 200, but in a shape this gateway has never seen.
		_, _ = w.Write([]byte(`{"images":["a","b"]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000
	f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"bytedance/seedream-4-0","prompt":"a blue square",` +
			`"size":"1024x1024"}`),
		Credential: plaintext,
	}); gerr != nil {
		t.Fatalf("the request should serve: %v", gerr)
	}

	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("the request should have settled")
	}
	held, ok := f.settler.LastHold()
	if !ok {
		t.Fatal("the request should have held")
	}
	if st.ActualNano != held.AmountNano {
		t.Errorf("an uncountable response must settle at the hold %d, not %d",
			held.AmountNano, st.ActualNano)
	}
}

// A route verified on generations is not thereby a candidate for edits.
//
// The two endpoints shared one capability key, and only generations was ever
// probed. So a vendor with no edits endpoint -- several have none, taking an
// input image on the generations call instead -- became a candidate for one the
// moment its generations probe went green, and answered the upstream's 404 to
// every edit request. The symptom was the worst kind: configuration entirely
// correct, and every request to that path failing.
func TestGenerationsVerdictDoesNotMakeARouteAnEditsCandidate(t *testing.T) {
	upstreamCalls := 0
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"a"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	// seedPerImageModel marks `images` ok and says nothing about `images_edits`,
	// which is exactly the state an operator leaves behind after probing the
	// one endpoint that can be probed cheaply.
	f.seedPerImageModel(t, "bytedance/seedream-4-0", 40_000_000)

	// Generations serves.
	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body:         []byte(`{"model":"bytedance/seedream-4-0","prompt":"x","size":"1024x1024"}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("generations should serve: %v", gerr)
	}

	// Edits does not, and is refused here rather than at the upstream.
	before := upstreamCalls
	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImagesEdit, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/edits",
		Body:         []byte(`{"model":"bytedance/seedream-4-0","prompt":"x","size":"1024x1024"}`),
		Credential:   plaintext,
	}); gerr == nil {
		t.Fatal("an unverified edits endpoint must not be a candidate")
	}
	if upstreamCalls != before {
		t.Errorf("the upstream was called for edits; the refusal must come from the catalogue")
	}
}

// A per-image model on the *edits* endpoint is charged at the row it asked for.
//
// Every other per-image test drives generations, and that is how the defect
// this covers survived a review: on edits the request body is a multipart
// stream, so admission never sees `in.Body` at all -- it is handed the small
// fields peeked out of the stream instead. Settlement that re-read the request
// body therefore found nothing, resolved the card's widest row instead of the
// one that was held, and either charged a different price or missed the card
// entirely and refused a request whose image the upstream had already made.
func TestPerImageEditChargesAtTheRowItWasAdmittedOn(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"a"},{"b64_json":"b"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	const nanoPerImage = 40_000_000
	modelID := f.seedPerImageModel(t, "bytedance/seedream-4-0", nanoPerImage)
	// A second, dearer row and no catch-all: the card prices 1024x1024 and
	// 2048x2048 and nothing else, so a lookup that lost the size misses the
	// card altogether rather than quietly landing on a neighbour.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model_price_unit_rates (model_id, unit, resolution, nano_per_unit)
		 VALUES ($1, 'image', '2048x2048', $2)`, modelID, 4*nanoPerImage); err != nil {
		t.Fatal(err)
	}
	// Edits is its own capability key, so it needs its own verdict: the
	// generations one deliberately does not carry over.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status)
		SELECT id, 'images_edits', 'openai', 'manual', 'ok' FROM model_routes WHERE model_id = $1
		ON CONFLICT (route_id, endpoint) DO UPDATE SET status = 'ok'`, modelID); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "bytedance/seedream-4-0")
	_ = mw.WriteField("size", "2048x2048")
	_ = mw.WriteField("prompt", "repaint the walls")
	part, _ := mw.CreateFormFile("image", "room.png")
	_, _ = part.Write(bytes.Repeat([]byte{0xFF}, 4<<10))
	_ = mw.Close()

	if _, gerr := f.pipeline.RunImageEdit(ctx, proxy.Request{
		Surface: catalog.SurfaceImagesEdit, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/edits",
		Credential:   plaintext,
	}, mw.FormDataContentType(), bytes.NewReader(buf.Bytes())); gerr != nil {
		t.Fatalf("the edit should serve: %v", gerr)
	}

	// Two images at the 2048 rate, not at the 1024 one and not a refusal.
	const want = 2 * 4 * nanoPerImage
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != want {
		t.Fatalf("two 2048x2048 edits should settle %d: %+v", want, st)
	}
}
