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
