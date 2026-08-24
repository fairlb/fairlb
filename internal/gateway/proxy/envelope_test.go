package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The two hosted platforms want the Anthropic Messages request cut differently:
// the model in the address, the api-version in the body. Both refuse a request
// that still carries the model, so getting this wrong is not a degradation --
// it is a provider that answers nothing.

func rewrite(t *testing.T, body string, stream bool, tp catalog.Transport) map[string]any {
	t.Helper()
	out, err := proxy.RewriteRequest(catalog.SurfaceMessages, []byte(body), "up-model", stream, tp)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestEnvelopeMovesTheModelIntoTheAddress(t *testing.T) {
	for _, env := range []string{catalog.EnvelopeBedrock, catalog.EnvelopeVertex} {
		t.Run(env, func(t *testing.T) {
			doc := rewrite(t, `{"model":"claude","max_tokens":4}`, false,
				catalog.Transport{Envelope: env})
			if _, has := doc["model"]; has {
				t.Errorf("the model must not remain in the body: %v", doc["model"])
			}
			if doc["max_tokens"] == nil {
				t.Error("the rest of the request must be untouched")
			}
		})
	}
}

// Each platform has its own required version string, and leaving it out is not
// a soft failure: the request is refused. So the default has to produce a
// request that works without the operator knowing the constant.
func TestEnvelopeSuppliesTheVersionTheBodyRequires(t *testing.T) {
	for _, tc := range []struct{ env, pinned, want string }{
		{catalog.EnvelopeBedrock, "", "bedrock-2023-05-31"},
		{catalog.EnvelopeVertex, "", "vertex-2023-10-16"},
		{catalog.EnvelopeVertex, "vertex-2099-01-01", "vertex-2099-01-01"},
	} {
		doc := rewrite(t, `{"model":"claude"}`, false,
			catalog.Transport{Envelope: tc.env, AnthropicVersion: tc.pinned})
		if got := doc["anthropic_version"]; got != tc.want {
			t.Errorf("%s pinned=%q: anthropic_version = %v, want %q",
				tc.env, tc.pinned, got, tc.want)
		}
	}
}

// The two platforms disagree about where streaming is chosen, and each refuses
// the other's answer: one takes it from the endpoint and rejects a body flag,
// the other needs the flag set. Copying the caller's flag through unchanged
// would be wrong for both, in opposite directions.
func TestEnvelopeDecidesWhereStreamingIsDeclared(t *testing.T) {
	for _, stream := range []bool{false, true} {
		bedrock := rewrite(t, `{"model":"c","stream":true}`, stream,
			catalog.Transport{Envelope: catalog.EnvelopeBedrock})
		if _, has := bedrock["stream"]; has {
			t.Errorf("stream=%v: the bedrock envelope must not carry a stream flag", stream)
		}
		vertex := rewrite(t, `{"model":"c"}`, stream,
			catalog.Transport{Envelope: catalog.EnvelopeVertex})
		if vertex["stream"] != stream {
			t.Errorf("stream=%v: the vertex envelope must state it in the body, got %v",
				stream, vertex["stream"])
		}
	}
}

// No envelope means no change. This is the assertion that keeps every existing
// provider byte-identical to what it was, and it is the one that would break
// silently if the envelope were ever inferred rather than declared.
func TestNoEnvelopeChangesNothing(t *testing.T) {
	doc := rewrite(t, `{"model":"claude","max_tokens":4,"stream":true}`, true, catalog.Transport{})
	if doc["model"] != "up-model" {
		t.Errorf("the model rename is still expected: %v", doc["model"])
	}
	if _, has := doc["anthropic_version"]; has {
		t.Error("without an envelope the version belongs in a header, not the body")
	}
	if doc["stream"] != true {
		t.Error("the caller's own stream flag must be left alone")
	}
	// An auth mode alone must not bring an envelope with it: the same signature
	// also fronts an endpoint that wants no envelope at all.
	signed := rewrite(t, `{"model":"claude"}`, false, catalog.Transport{
		Auth: catalog.AuthAWSSigV4, SigV4: &catalog.SigV4Profile{Region: "us-east-1"},
	})
	if signed["model"] != "up-model" {
		t.Errorf("signing must not imply an envelope: %v", signed["model"])
	}
}

// The envelope touches exactly the three keys it is documented to touch and
// nothing else.
//
// Written as a set comparison rather than as three lookups on purpose: a
// per-key assertion cannot see a *fourth* key appearing, and a fourth key is
// how this stops being a re-cut and starts being a translation layer.
func TestEnvelopeTouchesOnlyTheKeysItDeclares(t *testing.T) {
	const in = `{"model":"claude","max_tokens":4,"temperature":0.5,"messages":[],"tools":[],"system":"s"}`
	before := rewrite(t, in, false, catalog.Transport{})
	after := rewrite(t, in, false, catalog.Transport{Envelope: catalog.EnvelopeVertex})

	added, removed := diffKeys(before, after)
	if !slices.Equal(added, []string{"anthropic_version", "stream"}) {
		t.Errorf("the envelope added %v", added)
	}
	if !slices.Equal(removed, []string{"model"}) {
		t.Errorf("the envelope removed %v", removed)
	}
	for k, v := range before {
		if k == "model" {
			continue
		}
		if !jsonEqual(v, after[k]) {
			t.Errorf("the envelope changed %q: %v -> %v", k, v, after[k])
		}
	}
}

func diffKeys(before, after map[string]any) (added, removed []string) {
	for k := range after {
		if _, had := before[k]; !had {
			added = append(added, k)
		}
	}
	for k := range before {
		if _, still := after[k]; !still {
			removed = append(removed, k)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Streaming and non-streaming live at different addresses on both platforms,
// and the difference is a suffix on the same path rather than a flag. Sending
// the non-streaming address for a streamed request does not fail -- it returns
// the whole answer at once, which the streaming pipeline then waits on until
// its first-byte deadline fires.
func TestStreamingUsesItsOwnAddress(t *testing.T) {
	tp := catalog.Transport{
		PathOverrides: map[string]string{
			catalog.PathMessages: "/v1/projects/p/locations/l/publishers/anthropic/models/{model}:rawPredict",
		},
		StreamPathOverrides: map[string]string{
			catalog.PathMessages: "/v1/projects/p/locations/l/publishers/anthropic/models/{model}:streamRawPredict",
		},
	}
	for _, tc := range []struct {
		stream bool
		want   string
	}{
		{false, "https://up.test/v1/projects/p/locations/l/publishers/anthropic/models/claude-x:rawPredict"},
		{true, "https://up.test/v1/projects/p/locations/l/publishers/anthropic/models/claude-x:streamRawPredict"},
	} {
		req, err := proxy.BuildRequest(context.Background(), proxy.Target{
			Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", APIKey: "k",
			Path: catalog.PathMessages, UpstreamModel: "claude-x", Stream: tc.stream,
			Transport: tp,
		}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := req.URL.String(); got != tc.want {
			t.Errorf("stream=%v:\n got=%s\nwant=%s", tc.stream, got, tc.want)
		}
	}
}

// A streamed request against a provider with only the ordinary override falls
// back to it, so an upstream whose two endpoints coincide -- which is nearly
// all of them -- needs one map rather than two identical ones.
func TestStreamingFallsBackToTheOrdinaryOverride(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test/v1beta/openai", APIKey: "k",
		Path: catalog.PathChat, Stream: true,
		Transport: catalog.Transport{PathOverrides: map[string]string{
			catalog.PathChat: "/chat/completions",
		}},
	}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://up.test/v1beta/openai/chat/completions"; req.URL.String() != want {
		t.Errorf("url = %q, want %q", req.URL.String(), want)
	}
}

// One platform answers a streamed request in a binary framing of its own and
// refuses a request that asks for SSE, so what this gateway declares it will
// accept has to follow the envelope rather than the fact of streaming.
func TestAcceptFollowsTheEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		env    string
		stream bool
		want   string
	}{
		{"ordinary stream", catalog.EnvelopeNone, true, "text/event-stream"},
		{"ordinary unary", catalog.EnvelopeNone, false, "application/json"},
		{"bedrock stream", catalog.EnvelopeBedrock, true, "application/vnd.amazon.eventstream"},
		{"bedrock unary", catalog.EnvelopeBedrock, false, "application/json"},
		{"vertex stream", catalog.EnvelopeVertex, true, "text/event-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := proxy.BuildRequest(context.Background(), proxy.Target{
				Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", APIKey: "k",
				Path: catalog.PathMessages, Stream: tc.stream,
				Transport: catalog.Transport{Envelope: tc.env},
			}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Accept"); got != tc.want {
				t.Errorf("Accept = %q, want %q", got, tc.want)
			}
		})
	}
}

// With an envelope the version travels in the body, so the header must not go
// out as well. Two copies of one setting, one of them read by nobody, is a
// gateway fingerprint on every request.
func TestEnvelopeSuppressesTheVersionHeader(t *testing.T) {
	target := proxy.Target{
		Protocol: proxy.ProtocolAnthropic, BaseURL: "https://up.test", APIKey: "k",
		Path:      catalog.PathMessages,
		Transport: catalog.Transport{Envelope: catalog.EnvelopeVertex},
	}
	req, err := proxy.BuildRequest(context.Background(), target, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version = %q, want no header at all", got)
	}
	if slices.Contains(proxy.OutboundAllowlist(target), "Anthropic-Version") {
		t.Error("the allowlist still permits a header the request no longer sends")
	}
}

// A pagination cursor and a method have to be in place before the request is
// built, because a signature covers both. This asserts the mechanism the
// discovery path depends on.
func TestMethodAndExtraQueryReachTheRequest(t *testing.T) {
	req, err := proxy.BuildRequest(context.Background(), proxy.Target{
		Protocol: proxy.ProtocolOpenAI, BaseURL: "https://up.test", APIKey: "k",
		Path: catalog.PathModels, Method: "GET",
		Transport:  catalog.Transport{Query: map[string]string{"api-version": "2024-10-21"}},
		ExtraQuery: map[string]string{"after_id": "m-9"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "GET" {
		t.Errorf("method = %s", req.Method)
	}
	q := req.URL.Query()
	// Both, not one: the profile's parameter is mandatory for the upstream and
	// the cursor is what makes the second page different from the first.
	if q.Get("api-version") != "2024-10-21" || q.Get("after_id") != "m-9" {
		t.Errorf("query = %q", req.URL.RawQuery)
	}
	if got := slices.Sorted(maps.Keys(q)); !slices.Equal(got, []string{"after_id", "api-version"}) {
		t.Errorf("unexpected query parameters: %v", got)
	}
}

// The whole streamed chain, end to end: binary frames in, SSE out of the pump,
// usage read off it by the settlement path.
//
// The three pieces are tested apart elsewhere, and apart is not enough here.
// What this catches is the pieces *agreeing* about the SSE shape. Measured
// rather than asserted: running the event name into the data line -- output
// that still looks like SSE at a glance -- makes the usage parser find nothing,
// and "no usage" is not an error, it is a silent fall back to an estimated
// bill. Dropping the space after `data:` is caught too, but by the byte-shape
// assertion rather than the usage one, because the usage parser trims and does
// not mind. Two assertions, two different failure modes; neither covers the
// other.
func TestBedrockStreamReachesSettlementAsUsage(t *testing.T) {
	raw := encodeAnthropicFrames(t,
		`{"type":"message_start","message":{"usage":{"input_tokens":800,"output_tokens":0}}}`,
		`{"type":"content_block_delta","delta":{"text":"Hi"}}`,
		`{"type":"message_delta","usage":{"output_tokens":300}}`,
	)

	body := proxy.ExportUpstreamStreamBody(
		catalog.Transport{Envelope: catalog.EnvelopeBedrock},
		io.NopCloser(bytes.NewReader(raw)))

	rec := httptest.NewRecorder()
	out, err := proxy.NewStreamer().Pump(context.Background(), rec, body, catalog.SurfaceMessages)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("pumping the re-framed stream: %v", err)
	}
	if !out.FirstByteSent {
		t.Fatal("nothing reached the client")
	}
	if !out.Usage.Present {
		t.Fatal("usage was not read off the re-framed stream; settlement would fall back to an estimate")
	}
	if out.Usage.In != 800 || out.Usage.Out != 300 {
		t.Errorf("usage = in %d / out %d, want 800 / 300", out.Usage.In, out.Usage.Out)
	}
	if !strings.Contains(out.Text, "Hi") {
		t.Errorf("the forwarded text was not accumulated: %q", out.Text)
	}
	// And the client really did receive SSE, not the frames.
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "event: message_start\ndata: {") {
		t.Errorf("the client did not receive SSE:\n%q", rec.Body.String())
	}
}

// A provider with no envelope gets its body untouched. This is the half that
// keeps every existing streaming provider byte-identical, and it is also what
// pins the *selector*: the framing follows the declared profile, never the
// upstream's own content type, so no upstream header can change how its stream
// is decoded here.
func TestNoEnvelopeLeavesTheStreamAlone(t *testing.T) {
	const sse = "event: ping\ndata: {}\n\n"
	body := proxy.ExportUpstreamStreamBody(catalog.Transport{}, io.NopCloser(strings.NewReader(sse)))
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sse {
		t.Fatalf("got=%q want=%q", got, sse)
	}
}

// encodeAnthropicFrames builds the binary frames a streamed answer arrives in,
// with the protocol's own encoder so the checksums are the real ones.
func encodeAnthropicFrames(t *testing.T, inners ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, inner := range inners {
		payload, err := json.Marshal(map[string]any{"bytes": []byte(inner)})
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(&buf, eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue("event")},
				{Name: ":event-type", Value: eventstream.StringValue("chunk")},
			},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}
