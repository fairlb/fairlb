package proxy_test

import (
	"bytes"
	"io"
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
	got, err := io.ReadAll(peeked.Body)
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
	got, _ := io.ReadAll(peeked.Body)
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
