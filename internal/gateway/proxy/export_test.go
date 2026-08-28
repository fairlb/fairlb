package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// ExportUpstreamTransport exposes the transport the pipeline forwards on, so a
// test can assert what the default one is configured with. The transport is
// otherwise private because nothing outside this package has any business
// swapping it after construction.
func ExportUpstreamTransport(p *Pipeline) http.RoundTripper { return p.client.Transport }

// ExportConnectTimeout and ExportClientTimeout expose the two upstream time
// bounds so a test can check them against the numbers the documentation
// publishes.
func ExportConnectTimeout() time.Duration           { return httpx.ConnectTimeout }
func ExportClientTimeout(p *Pipeline) time.Duration { return p.client.Timeout }

// ExportSentUpstreamPaths is every path this layer can send, read off the
// handler's own table rather than restated. It exists so a test can hold that
// set against the set a transport profile may override: a path that can be sent
// but not overridden is an upstream nobody can reach, and one that can be
// overridden but never sent is a setting that saves and does nothing.
func ExportSentUpstreamPaths() []string {
	out := []string{
		imageEditEndpoint.path,
		catalog.PathResponsesResource, catalog.PathResponsesCancel, catalog.PathResponsesInputItems,
		catalog.PathGeminiInteractions, catalog.PathGeminiInteraction, catalog.PathGeminiInteractionCancel,
	}
	for _, ep := range endpoints {
		out = append(out, ep.path)
	}
	for _, ep := range geminiEndpoints {
		out = append(out, ep.path)
	}
	return out
}

// ExportConnectTimeoutFor reports the bound the dialer would use for a request
// carrying this target's profile.
func ExportConnectTimeoutFor(ctx context.Context, t Target) time.Duration {
	return httpx.ConnectTimeoutFrom(withConnectTimeout(ctx, t))
}

// ExportUpstreamStreamBody exposes the choice of stream framing so a test can
// assert that the profile is what selects it.
//
// Selecting on the profile rather than on the upstream's Content-Type is a
// decision worth pinning: an upstream header must not be able to switch how the
// gateway decodes a stream. The choice itself is unobservable from outside --
// pick the wrong branch and the symptom is a stream that produces nothing,
// which is also what a dead upstream produces.
func ExportUpstreamStreamBody(tp catalog.Transport, body io.ReadCloser) io.Reader {
	return upstreamStreamBody(tp, body)
}

// AccumulateForTest feeds a series of SSE frames to the streaming usage
// accumulator and returns the normalised result. It is exported purely so the
// external test package can assert that the streaming and non-streaming paths
// normalise to the same result -- each parses usage separately, which is the
// easiest place for the two to drift apart.
func AccumulateForTest(surface catalog.Surface, frames []string) Usage {
	var acc usageAccumulator
	var text bytes.Buffer
	for _, f := range frames {
		acc.consume(surface, []byte(f), &text)
	}
	return acc.result()
}

// ExportImageUnits is the billable quantity vector for a per-image request: a
// rate row and a count. Exported so a test can hold the arithmetic without
// standing up a pipeline -- getting it wrong charges for the wrong number of
// images with nothing to say so.
func ExportImageUnits(body []byte, unit catalog.Unit, images int64) catalog.Units {
	p := imageParamsOf(body)
	return imageUnits(catalog.UnitKey{Unit: unit, Resolution: p.Size, Variant: p.Quality}, images)
}

// ExportHeldRateKey is the rate row a request was admitted against, which
// settlement must keep rather than resolve a second time.
func ExportHeldRateKey(held catalog.Units) (catalog.UnitKey, bool) { return heldRateKey(held) }

// ExportImagesInResponse is the count settlement charges on: how many images
// the upstream's response actually carried. Exported because it is the whole
// answer to "how many images is this caller paying for", and the request cannot
// give that answer on every vendor.
func ExportImagesInResponse(body []byte) (int64, bool) { return imagesInResponse(body) }

// ExportMaxImagesOf is the number of images the hold is taken against: the
// largest any candidate route declared it can return.
func ExportMaxImagesOf(routes []catalog.Route) int64 { return maxImagesOf(routes) }

// ExportPeekedProbeBody is the JSON admission and pricing are handed for a
// multipart edit request: the fields read ahead out of the stream, and nothing
// else. The upstream still receives the original multipart body.
func ExportPeekedProbeBody(p PeekedMultipart) []byte { return peekedProbeBody(p) }
