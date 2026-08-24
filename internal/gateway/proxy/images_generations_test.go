package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// Image generation must be billed on the tokens the upstream reports.
//
// This is the same assertion TestPipelineImageEditBillsCorrectly makes for the
// edits endpoint, against the same upstream body and the same seeded prices --
// deliberately, because the two endpoints share a surface, a price row and a
// settlement function, and the only thing that differed was which usage parser
// each one reached.
//
// The failure this guards is silent in every way that matters: the upstream
// answers 200, the client gets its image, a usage row is written, and the
// charge is zero. Nothing is logged, no error code is produced, and the row
// looks like an ordinary cheap request rather than a fault.
func TestPipelineImageGenerationBillsCorrectly(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"abc"}],
			"usage":{"input_tokens":100,"output_tokens":1000}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-image-2", "gpt-image-2", []string{"images"})

	res, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body:         []byte(`{"model":"openai/gpt-image-2","prompt":"a blue square"}`),
		Credential:   plaintext,
	})
	if gerr != nil {
		t.Fatalf("the image generation should succeed: %v", gerr)
	}
	if res.Status != 200 {
		t.Fatalf("status code: %d", res.Status)
	}

	// Identical arithmetic to the edits test: 100 in at $3/Mtok plus 1000 out
	// at $15/Mtok is 0.3e6 + 15e6 = 15.3e6, and with the 20% default markup
	// 18.36e6.
	const wantCharged = 18_360_000
	st, ok := f.settler.LastSettle()
	if !ok || st.ActualNano != wantCharged {
		t.Errorf("the image charge should be %d: settlement %+v", wantCharged, st)
	}

	// The usage row has to carry the token counts too. Asserting only the
	// charge would let a future change that zeroes the row but keeps the
	// arithmetic pass, and the row is what usage reports are built from.
	var tokensIn, tokensOut int32
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_in, tokens_out, usage_estimated FROM usage_logs WHERE org_id = $1`, org).
		Scan(&tokensIn, &tokensOut, &estimated); err != nil {
		t.Fatal(err)
	}
	if tokensIn != 100 || tokensOut != 1000 {
		t.Errorf("image usage row: in=%d out=%d, want 100/1000", tokensIn, tokensOut)
	}
	// The upstream did report usage, so this must not be marked as estimated.
	// A true here would mean the parser missed the block and the estimate
	// happened to produce a number -- the failure that hides behind a
	// plausible charge.
	if estimated {
		t.Error("usage was reported by the upstream; the row must not be marked estimated")
	}
}

// An image generation whose upstream really does omit usage falls back to the
// held amount rather than being served for nothing.
//
// This is the companion of the case above and the reason the two cannot be
// covered by one test: "the parser read zero" and "there was nothing to read"
// produce the same token counts and are distinguished only by which path the
// charge came from.
func TestPipelineImageGenerationWithoutUsageFallsBackToHold(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"abc"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-image-2", "gpt-image-2", []string{"images"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body:         []byte(`{"model":"openai/gpt-image-2","prompt":"a blue square"}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the image generation should succeed: %v", gerr)
	}

	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("the request should have settled")
	}
	if len(f.settler.Holds) != 1 {
		t.Fatalf("exactly one hold expected: %d", len(f.settler.Holds))
	}
	held := f.settler.Holds[0].AmountNano
	// An image's cost sits on the *output* side, which no estimate can reach:
	// the response body is base64 image data, so running the text heuristic
	// over it measures the encoding rather than the work. Charging the input
	// side alone therefore gives the image away for almost nothing -- which is
	// exactly the reasoning already written down on the edits path, where the
	// fallback settles at the held amount instead.
	//
	// Asserting equality with the hold rather than "greater than zero" is the
	// point: the input side alone is greater than zero too, so the weaker
	// assertion passes while the endpoint is still nearly free.
	if st.ActualNano != held {
		t.Errorf("with no reported usage the charge must follow the hold: settled %d, held %d", st.ActualNano, held)
	}
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT usage_estimated FROM usage_logs WHERE org_id = $1`, org).Scan(&estimated); err != nil {
		t.Fatal(err)
	}
	if !estimated {
		t.Error("the row must be marked estimated when the upstream reported no usage")
	}
}
