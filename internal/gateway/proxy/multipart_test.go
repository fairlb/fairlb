package proxy_test

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// buildMultipart assembles a multipart body, writing the fields in the order
// given. That order is the variable these tests turn on: the model before the
// file, or after it.
func buildMultipart(t *testing.T, fields []field) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if f.filename != "" {
			part, err := w.CreateFormFile(f.name, f.filename)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write(f.content)
			continue
		}
		if err := w.WriteField(f.name, string(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

type field struct {
	name     string
	filename string
	content  []byte
}

// The common shape: small fields first, the file last, which is what the
// official SDKs produce.
func TestPeekModelBeforeFile(t *testing.T) {
	body, ct := buildMultipart(t, []field{
		{name: "model", content: []byte("gpt-image-2")},
		{name: "prompt", content: []byte("make it blue")},
		{name: "image", filename: "a.png", content: bytes.Repeat([]byte{0xFF}, 200<<10)},
	})

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), ct)
	if err != nil {
		t.Fatalf("the model should have been read ahead: %v", err)
	}
	if peeked.Model != "gpt-image-2" {
		t.Fatalf("model = %q", peeked.Model)
	}

	// The point: reading ahead must not consume the original stream. Rejoined,
	// it has to be byte-identical to the original, or the upstream receives a
	// truncated multipart body.
	got, err := io.ReadAll(mustBody(t, &peeked, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the rejoined body differs from the original: %d vs %d bytes", len(got), len(body))
	}
}

// The model sits after the file: as long as the file stays inside the
// read-ahead cap this still succeeds, and the rejoined body is still intact.
func TestPeekModelAfterSmallFile(t *testing.T) {
	body, ct := buildMultipart(t, []field{
		{name: "image", filename: "a.png", content: bytes.Repeat([]byte{0xAB}, 4<<10)},
		{name: "model", content: []byte("gpt-image-2")},
	})

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), ct)
	if err != nil {
		t.Fatalf("with a small file first the model should still be read ahead: %v", err)
	}
	if peeked.Model != "gpt-image-2" {
		t.Fatalf("model = %q", peeked.Model)
	}
	got, _ := io.ReadAll(mustBody(t, &peeked, ""))
	if !bytes.Equal(got, body) {
		t.Fatal("the rejoined body should equal the original")
	}
}

// The model sits behind a very large file: the read-ahead stops at the cap and
// errors, rather than buffering without bound until memory runs out.
func TestPeekModelAfterHugeFileFails(t *testing.T) {
	body, ct := buildMultipart(t, []field{
		{name: "image", filename: "big.png", content: bytes.Repeat([]byte{0xCD}, 2<<20)}, // 2 MiB, past the cap
		{name: "model", content: []byte("gpt-image-2")},
	})

	_, err := proxy.PeekMultipartModel(bytes.NewReader(body), ct)
	if err == nil {
		t.Fatal("passing the read-ahead cap should error rather than buffer without bound")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("the error should say the model is missing: %v", err)
	}
}

// No model field: error explicitly rather than letting a request reach routing
// with an empty model name.
func TestPeekMissingModel(t *testing.T) {
	body, ct := buildMultipart(t, []field{
		{name: "prompt", content: []byte("x")},
	})
	if _, err := proxy.PeekMultipartModel(bytes.NewReader(body), ct); err == nil {
		t.Fatal("a missing model should error")
	}
}

// A malformed Content-Type or a missing boundary fails early, rather than
// waiting for the upstream to fail to parse it.
func TestPeekRejectsBadContentType(t *testing.T) {
	for _, ct := range []string{
		"", "application/json", "multipart/form-data", "not a media type",
	} {
		if _, err := proxy.PeekMultipartModel(strings.NewReader("x"), ct); err == nil {
			t.Errorf("Content-Type %q should be rejected", ct)
		}
	}
}

// Parsing usage out of an image response; current image models report it in
// tokens.
func TestParseImageUsage(t *testing.T) {
	u := proxy.ParseImageUsage([]byte(`{"data":[{"b64_json":"..."}],
		"usage":{"input_tokens":120,"output_tokens":1580,"total_tokens":1700}}`))
	if !u.Present || u.In != 120 || u.Out != 1580 {
		t.Fatalf("parsed usage does not match: %+v", u)
	}

	// No usage reported means Present is false and the caller estimates.
	if got := proxy.ParseImageUsage([]byte(`{"data":[]}`)); got.Present {
		t.Errorf("with no usage, Present must stay false: %+v", got)
	}
	if got := proxy.ParseImageUsage([]byte(`not json`)); got.Present {
		t.Errorf("a malformed response must not set Present: %+v", got)
	}
}

// The image breakdown is carried, not dropped.
//
// It used to be parsed and then thrown away, so image input was billed at the
// model's text rate -- for gpt-image-2 that is 5 against a real 8 per million,
// on the bucket an edit request is mostly made of.
func TestParseImageUsageCarriesTheImageBreakdown(t *testing.T) {
	u := proxy.ParseImageUsage([]byte(`{"data":[{"b64_json":"..."}],
		"usage":{"input_tokens":1000,"output_tokens":1580,"total_tokens":2580,
		"input_tokens_details":{"image_tokens":800,"text_tokens":200}}}`))
	if u.In != 1000 || u.ImageIn != 800 {
		t.Fatalf("in=%d image=%d, want 1000 and 800", u.In, u.ImageIn)
	}

	// A response without the breakdown leaves the field at zero, which bills
	// exactly as it did before the field existed.
	plain := proxy.ParseImageUsage([]byte(`{"usage":{"input_tokens":1000,"output_tokens":10}}`))
	if plain.ImageIn != 0 {
		t.Errorf("no breakdown reported, so nothing should be attributed to image: %d", plain.ImageIn)
	}

	// The subset relation is what makes the subtraction in BuildCharge correct,
	// so an upstream that contradicts it is clamped rather than allowed to turn
	// a served request into a billing error.
	odd := proxy.ParseImageUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":0,
		"input_tokens_details":{"image_tokens":900}}}`))
	if odd.ImageIn != 100 {
		t.Errorf("image tokens above the input total should clamp to it, got %d", odd.ImageIn)
	}
	negative := proxy.ParseImageUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":0,
		"input_tokens_details":{"image_tokens":-5}}}`))
	if negative.ImageIn != 0 {
		t.Errorf("a negative count is meaningless and should floor at zero, got %d", negative.ImageIn)
	}
}

// The read-ahead must not swallow the upload.
//
// Part.Close drains what it closes, so a scan that kept looking for the
// remaining small fields past the file part would read the whole upload up to
// the cap -- on every ordinary edit request, not just a pathological one.
// Measured at exactly maxModelPeek (1 MiB) when that bound was missing, against
// roughly two hundred bytes with it.
func TestPeekStopsAtTheFilePartOnceTheModelIsKnown(t *testing.T) {
	body, contentType := editRequest(t, map[string]string{"model": "openai/gpt-image-2"}, 4<<20)
	src := &countingReader{r: bytes.NewReader(body)}

	peeked, err := proxy.PeekMultipartModel(src, contentType)
	if err != nil {
		t.Fatal(err)
	}
	if peeked.Model != "openai/gpt-image-2" {
		t.Fatalf("model=%q", peeked.Model)
	}
	// A generous bound: the point is the order of magnitude, not a byte count
	// that would break every time the boundary string changes length.
	const budget = 16 << 10 // observed ~1 KiB; the assertion is the order of magnitude
	if src.n > budget {
		t.Errorf("read %d bytes from the client to find the billing fields; the whole upload "+
			"is being buffered (budget %d, cap %d)", src.n, budget, 1<<20)
	}
	// And the body still forwards byte for byte, which is the property the
	// early stop must not cost.
	out, err := io.ReadAll(mustBody(t, &peeked, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("forwarded body differs: %d bytes, want %d", len(out), len(body))
	}
}

// The fields a client sends before the file are still picked up, so the common
// shape is billed at the row it asked for rather than at the widest one.
func TestPeekTakesTheBillingFieldsWrittenBeforeTheFile(t *testing.T) {
	body, contentType := editRequest(t, map[string]string{
		"model": "bytedance/seedream-4-0", "n": "3", "size": "2048x2048", "quality": "high",
	}, 1<<20)

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), contentType)
	if err != nil {
		t.Fatal(err)
	}
	if peeked.Size != "2048x2048" || peeked.Quality != "high" {
		t.Fatalf("billing fields lost: size=%q quality=%q", peeked.Size, peeked.Quality)
	}
}

// A model written after the file is still found: without one there is no
// request at all, so that is the case the cap was written for and the scan
// keeps going past the upload.
func TestPeekStillReadsPastAFileToFindTheModel(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("image", "room.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), 4<<10)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("model", "openai/gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(buf.Bytes()), w.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
	if peeked.Model != "openai/gpt-image-2" {
		t.Fatalf("model after the file was not found: %q", peeked.Model)
	}
}

// countingReader records how much of the client's body was consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// editRequest builds an edits-shaped multipart body: the named small fields, in
// the order given, then a file part of the requested size.
func editRequest(t *testing.T, fields map[string]string, fileBytes int) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// model first, then the rest: that is the order every client library writes
	// them in, and the order this function is optimised for.
	for _, name := range []string{"model", "n", "size", "quality"} {
		if v, ok := fields[name]; ok {
			if err := w.WriteField(name, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.WriteField("prompt", "repaint the walls green"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("image", "room.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), fileBytes)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// mustBody rebuilds the forwarded body, failing the test if it cannot.
//
// An empty upstream name means "leave the model as the caller wrote it", which
// is what the byte-for-byte assertions want: they are about the peek not
// consuming the stream, not about the substitution.
func mustBody(t *testing.T, p *proxy.PeekedMultipart, upstreamModel string) io.Reader {
	t.Helper()
	r, err := p.BodyFor(upstreamModel)
	if err != nil {
		t.Fatalf("rebuilding the multipart body: %v", err)
	}
	return r
}

// The model field is rewritten to the name the upstream knows, and nothing else
// moves.
//
// This is the substitution every other endpoint gets from RewriteRequest. Doing
// it here removes the one rule in the gateway that held on a single endpoint:
// that the route's upstream model id had to equal the second segment of the
// public slug, because this body could not be rewritten.
func TestBodyForRewritesTheModelField(t *testing.T) {
	body, contentType := editRequest(t, map[string]string{
		"model": "bytedance/seedream-4-0",
	}, 8<<10)

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), contentType)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(mustBody(t, &peeked, "doubao-seedream-4-0-250828"))
	if err != nil {
		t.Fatal(err)
	}

	// The upstream must see its own name, and must not see the public slug.
	if !bytes.Contains(out, []byte("doubao-seedream-4-0-250828")) {
		t.Error("the upstream model id is not in the forwarded body")
	}
	if bytes.Contains(out, []byte("bytedance/seedream-4-0")) {
		t.Error("the public slug is still in the forwarded body")
	}
	// Everything else survives, including the upload. Length is the blunt check
	// that matters: a rewrite that corrupted the stream would still contain
	// both strings.
	if !bytes.Contains(out, []byte("repaint the walls green")) {
		t.Error("the prompt did not survive the rewrite")
	}
	wantLen := len(body) - len("bytedance/seedream-4-0") + len("doubao-seedream-4-0-250828")
	if len(out) != wantLen {
		t.Errorf("forwarded body is %d bytes, want %d: something other than the model moved",
			len(out), wantLen)
	}
	// And it is still a body a multipart reader can read.
	mr := multipart.NewReader(bytes.NewReader(out), mustBoundary(t, contentType))
	seen := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FileName() == "" {
			var sb strings.Builder
			_, _ = io.Copy(&sb, part)
			seen[part.FormName()] = sb.String()
		}
		_ = part.Close()
	}
	if seen["model"] != "doubao-seedream-4-0-250828" {
		t.Errorf("re-parsed model field is %q", seen["model"])
	}
	if seen["prompt"] != "repaint the walls green" {
		t.Errorf("re-parsed prompt is %q", seen["prompt"])
	}
}

// A value that happens to contain the model's name is not the model field.
//
// The substitution walks the parts rather than searching for the string,
// because a prompt naming the model is an ordinary thing to write and replacing
// the wrong occurrence would corrupt the request in a way nothing downstream
// could notice.
func TestBodyForDoesNotRewriteAPromptThatNamesTheModel(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", "bytedance/seedream-4-0"); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("prompt", "in the style of bytedance/seedream-4-0, repaint the walls"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("image", "room.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), 4<<10)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	body, contentType := buf.Bytes(), w.FormDataContentType()

	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), contentType)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(mustBody(t, &peeked, "doubao-seedream-4-0-250828"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("in the style of bytedance/seedream-4-0, repaint")) {
		t.Errorf("the prompt was rewritten too:\n%s", out)
	}
}

// mustBoundary pulls the boundary back out of a Content-Type.
func mustBoundary(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	return params["boundary"]
}

// The body can only be handed out once, and the second attempt says so.
//
// The remainder is a stream: the first body drains it, so a second call would
// return the small fields followed by an exhausted reader -- a well-formed
// prefix with no upload, which an upstream answers with a confusing 400 rather
// than anything pointing back here.
func TestBodyForRefusesASecondCall(t *testing.T) {
	body, contentType := editRequest(t, map[string]string{"model": "m"}, 2<<10)
	peeked, err := proxy.PeekMultipartModel(bytes.NewReader(body), contentType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peeked.BodyFor("upstream-m"); err != nil {
		t.Fatalf("the first call should succeed: %v", err)
	}
	if _, err := peeked.BodyFor("upstream-m"); err == nil {
		t.Fatal("the second call must fail rather than return a body missing the upload")
	}
}
