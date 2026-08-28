package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// A streamed image generation is billed on the tokens the upstream reports,
// exactly as the unstreamed one is.
//
// `stream` is read off the body at the one boundary that chooses the entry
// point, with no condition on the surface, and gpt-image supports it: the
// events are partial images followed by `image_generation.completed`. That
// terminal frame carries a usage object -- in the input_tokens/output_tokens
// spelling, which no other openai-protocol surface uses -- so the chat arm
// found an object, read the two names it knows as *zero*, and reported it as
// present. Present short-circuits the estimate, so the request settled a real
// generation at nothing, with usage_estimated false and a row that looked like
// an ordinary cheap request.
//
// The unstreamed sibling of this test has existed since the buffered parser was
// given its own arm; the streaming switch owed the same sentence and nobody was
// holding it to it.
func TestStreamedImageGenerationBillsTheReportedTokens(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"abc\"}\n\n" +
				"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"abc\"," +
				"\"usage\":{\"input_tokens\":100,\"output_tokens\":1000}}\n\n" +
				"data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-image-2", "gpt-image-2", []string{"images"})

	rec := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body: []byte(`{"model":"openai/gpt-image-2","prompt":"a blue square",` +
			`"stream":true,"partial_images":1}`),
		Credential: plaintext, Stream: true,
	}, proxy.SurfaceOpenAI); gerr != nil {
		t.Fatalf("the streamed image generation should be served: %v", gerr)
	}

	// The same arithmetic as the unstreamed test, deliberately: 100 in at
	// $3/Mtok plus 1000 out at $15/Mtok is 15.3e6, and with the 20% default
	// markup 18.36e6. Delivery mode must not change the bill.
	const wantCharged = 18_360_000
	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("a delivered stream must settle")
	}
	if st.ActualNano == 0 {
		t.Fatalf("an image was generated and charged nothing: %+v", st)
	}
	if st.ActualNano != wantCharged {
		t.Fatalf("streamed image charge %d, want %d (the unstreamed number): %+v",
			st.ActualNano, wantCharged, st)
	}

	var tokensIn, tokensOut int32
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tokens_in, tokens_out, usage_estimated FROM usage_logs WHERE org_id = $1`, org).
		Scan(&tokensIn, &tokensOut, &estimated); err != nil {
		t.Fatal(err)
	}
	if tokensIn != 100 || tokensOut != 1000 {
		t.Errorf("streamed image usage row: in=%d out=%d, want 100/1000", tokensIn, tokensOut)
	}
	if estimated {
		t.Error("the upstream reported usage; the row must not be marked estimated")
	}
}

// A streamed image whose upstream reports no usage settles at the held amount.
//
// This is where streaming is worse than buffering rather than merely equal to
// it. The estimation fallback works from produced text, and an image stream has
// none at all -- its frames are partial images and then a result -- so the
// estimate is not an approximation of the bill, it is zero. The buffered path
// had already reasoned this out and settled at the hold instead; the streaming
// path was never given that branch.
func TestStreamedImageGenerationWithoutUsageSettlesAtTheHold(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"abc\"}\n\n" +
				"data: [DONE]\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-image-2", "gpt-image-2", []string{"images"})

	rec := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceImages, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/images/generations",
		Body:         []byte(`{"model":"openai/gpt-image-2","prompt":"a blue square","stream":true}`),
		Credential:   plaintext, Stream: true,
	}, proxy.SurfaceOpenAI); gerr != nil {
		t.Fatalf("the streamed image generation should be served: %v", gerr)
	}

	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("a delivered stream must settle")
	}
	if len(f.settler.Holds) != 1 {
		t.Fatalf("exactly one hold expected: %d", len(f.settler.Holds))
	}
	held := f.settler.Holds[0].AmountNano
	// Equality with the hold rather than "greater than zero": the input side
	// alone is greater than zero too, so the weaker assertion passes while the
	// endpoint is still nearly free.
	if st.ActualNano != held {
		t.Errorf("with no reported usage a streamed image must follow the hold: settled %d, held %d",
			st.ActualNano, held)
	}
}

// A streamed Gemini image keeps its image-token breakdown.
//
// Gemini's image models are reached on the same surface as its text ones, and
// its usage object reports the generated image as output tokens under an IMAGE
// modality detail. The buffered parser splits that out; the accumulator behind
// the streaming path copied five fields across and dropped the two image ones,
// so a streamed generation arrived at settlement with its image tokens folded
// back into plain text output -- which is priced several times lower.
//
// Asserted on the usage row rather than on the charge, so the test states the
// defect (the breakdown was lost) independently of how any deployment has
// priced that bucket.
func TestStreamedGeminiImageKeepsTheImageBreakdown(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"here\"}]}}]," +
				"\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":1290," +
				"\"candidatesTokensDetails\":[{\"modality\":\"IMAGE\",\"tokenCount\":1290}]}}\n\n"))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "gemini", "google/gemini-3.1-flash-image", "gemini-3.1-flash-image",
		[]string{"generate_content"})

	rec := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(ctx, rec, proxy.Request{
		Surface: catalog.SurfaceGenerateContent, Protocol: proxy.ProtocolGemini,
		UpstreamPath: "/v1beta/models/{model}:streamGenerateContent",
		Model:        "google/gemini-3.1-flash-image",
		Body:         []byte(`{"contents":[{"parts":[{"text":"a blue square"}]}]}`),
		Credential:   plaintext, Stream: true,
	}, proxy.SurfaceGemini); gerr != nil {
		t.Fatalf("the streamed generation should be served: %v", gerr)
	}

	var imageOut, tokensOut int32
	if err := f.pool.QueryRow(ctx,
		`SELECT COALESCE(tokens_image_out, 0), tokens_out FROM usage_logs WHERE org_id = $1`, org).
		Scan(&imageOut, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if imageOut != 1290 {
		t.Errorf("streamed image output tokens: %d, want 1290. Zero means the breakdown was "+
			"dropped and all 1290 will be priced at the text output rate.", imageOut)
	}
	if tokensOut != 1290 {
		t.Errorf("output total: %d, want 1290", tokensOut)
	}
}

// An image produced on a text surface is still an image, and its tokens are
// still image tokens.
//
// A modality is an axis of its own (ADR-0226): nothing ties an image model to
// the images endpoint, and an OpenAI-compatible relay can put one behind
// chat/completions -- Google's own compatible endpoint and Alibaba's
// compatible-mode both do. The breakdown had nowhere to go in that surface's
// usage object, so every generated image was folded into plain completion
// tokens and billed at the text output rate, which for these models is a third
// of the image one.
func TestChatCompletionsCarriesTheImageTokenBreakdown(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"here"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":1290,
			"completion_tokens_details":{"image_tokens":1290}}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "google/gemini-3.1-flash-image", "gemini-3.1-flash-image",
		[]string{"chat"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"google/gemini-3.1-flash-image","messages":[{"role":"user","content":"a cat"}]}`),
		Credential:   plaintext,
	}); gerr != nil {
		t.Fatalf("the request should serve: %v", gerr)
	}

	var imageOut int32
	if err := f.pool.QueryRow(ctx,
		`SELECT COALESCE(tokens_image_out, 0) FROM usage_logs WHERE org_id = $1`, org).
		Scan(&imageOut); err != nil {
		t.Fatal(err)
	}
	if imageOut != 1290 {
		t.Errorf("image output tokens on chat/completions: %d, want 1290. Zero means the "+
			"breakdown had nowhere to go and all of it will price as text.", imageOut)
	}
}

// The Responses image generation tool produces an image the upstream reports
// nothing about, so it is counted from the output items.
//
// This surface is the one place where an image is generated and the usage
// object says neither how many tokens it cost nor that the tool ran: the model
// billed is a text model, and its usage covers the prose. Without a count taken
// from somewhere, a caller generates images through this gateway and pays only
// for the words around them.
func TestResponsesCountsGeneratedImagesFromTheOutputItems(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":[
			{"type":"message"},
			{"type":"image_generation_call","result":"aaa"},
			{"type":"image_generation_call","result":"bbb"}],
			"usage":{"input_tokens":100,"output_tokens":50}}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "gpt-5.4", []string{"responses"})
	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/gpt-5.4'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	// The tool has to be priced, and that it *has* to be is half of what this
	// change buys: an image generated through a model with no rate for it lands
	// in the operator's unpriced queue instead of being served for the price of
	// the prose around it.
	const nanoPerImage = 40_000_000
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model_price_tool_rates (model_id, tool, nano_per_call) VALUES ($1, 'image_generation', $2)`,
		modelID, nanoPerImage); err != nil {
		t.Fatal(err)
	}

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/responses",
		Body: []byte(`{"model":"openai/gpt-5.4","input":"draw two cats",` +
			`"tools":[{"type":"image_generation"}]}`),
		Credential: plaintext,
	}); gerr != nil {
		t.Fatalf("the request should serve: %v", gerr)
	}

	var tools []byte
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT tool_calls, charged_nano FROM usage_logs WHERE org_id = $1`, org).
		Scan(&tools, &charged); err != nil {
		t.Fatal(err)
	}
	var counts map[string]int64
	if err := json.Unmarshal(tools, &counts); err != nil {
		t.Fatalf("tool usage %s is not a count map: %v", tools, err)
	}
	if counts["image_generation"] != 2 {
		t.Errorf("tool usage %s does not record the two generated images; they are billable "+
			"and the upstream reports them nowhere else", tools)
	}
	// The two images have to reach the charge, not merely the row. Before this
	// the whole request cost was the text tokens around them.
	if charged < 2*nanoPerImage {
		t.Errorf("charged %d, which does not cover two generated images at %d each",
			charged, nanoPerImage)
	}
}

// Images produced by the Responses tool are billed even when the answer
// carries no usage object at all.
//
// The usage object is what the caller estimates from when it is missing, and an
// image is not estimable -- it is not in that object even when one is sent. So
// "no usage" must not mean "no images": the count still comes from the output
// items, and the estimate path has to carry an observed tool call through
// rather than replace it. Returning early on a nil usage billed those images at
// nothing, which is the same leak this arm was added to close.
func TestResponsesCountsGeneratedImagesWithNoUsageObject(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"output":[
			{"type":"message"},
			{"type":"image_generation_call","result":"aaa"}]}`))
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedCatalog(t, "openai", "openai/gpt-5.4", "gpt-5.4", []string{"responses"})
	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM models WHERE slug = 'openai/gpt-5.4'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	const nanoPerImage = 40_000_000
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model_price_tool_rates (model_id, tool, nano_per_call) VALUES ($1, 'image_generation', $2)`,
		modelID, nanoPerImage); err != nil {
		t.Fatal(err)
	}

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/responses",
		Body: []byte(`{"model":"openai/gpt-5.4","input":"draw a cat",` +
			`"tools":[{"type":"image_generation"}]}`),
		Credential: plaintext,
	}); gerr != nil {
		t.Fatalf("the request should serve: %v", gerr)
	}

	var tools []byte
	var charged int64
	var estimated bool
	if err := f.pool.QueryRow(ctx,
		`SELECT tool_calls, charged_nano, usage_estimated FROM usage_logs WHERE org_id = $1`, org).
		Scan(&tools, &charged, &estimated); err != nil {
		t.Fatal(err)
	}
	var counts map[string]int64
	if err := json.Unmarshal(tools, &counts); err != nil {
		t.Fatalf("tool usage %s is not a count map: %v", tools, err)
	}
	if counts["image_generation"] != 1 {
		t.Errorf("tool usage %s lost the generated image on the estimate path", tools)
	}
	// The text side is still an estimate -- that half is genuinely unknown --
	// but the image is not, and the charge has to cover it.
	if !estimated {
		t.Error("with no usage object the token side must be marked estimated")
	}
	if charged < nanoPerImage {
		t.Errorf("charged %d, which does not cover the generated image at %d", charged, nanoPerImage)
	}
}
