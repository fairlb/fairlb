package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	gwusage "github.com/fairlb/fairlb/internal/gateway/usage"
)

// How every request that reached a decision is counted and timed.
//
// *Why this exists alongside the usage log and the rollups.* The rollups
// already carry a latency histogram per org, key, model and provider, and they
// are the right source for "which organization is slow" and for anything billed. They
// are not a source for "is the gateway healthy right now": they are written by
// a periodic aggregator, so they lag by up to an hour, they do not exist at all
// for a deployment whose aggregation has stalled, and a stalled aggregator
// looks exactly like an idle one. An operational signal has to come from the
// serving path itself, and it has to still be there when the database side of
// the system is the thing that broke.
//
// *The bucket bounds are shared with the rollup* (gwusage.LatencyBoundsSeconds)
// so a percentile read here and a percentile read from the rollups are the same
// computation over the same bounds. Where they then disagree, the disagreement
// means something -- which is only true if the buckets were never allowed to
// differ in the first place.
//
// *Label discipline, unchanged from the counters above*: surface (five),
// terminal status (four), and whether the response streamed (two). No org, key,
// model or provider -- those are unbounded or near-unbounded, and they are
// exactly what the rollups already split by. Forty series is the whole cost.

// requestDuration is the wall-clock time from the pipeline receiving a request
// to it reaching a terminal outcome, successful or not.
//
// Failures are in the same histogram as successes rather than a separate one,
// and that is deliberate: a latency graph that silently excludes failures gets
// *better* during an incident, because the slow requests are the ones timing
// out and leaving the sample set. The status label is there to split them back
// apart when that is the question.
var requestDuration = func() metric.Float64Histogram {
	h, err := otel.Meter("fairlb/gateway").Float64Histogram(
		"gateway_request_duration_seconds",
		metric.WithDescription("End-to-end time the dataplane spent on a request, including every failover attempt"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(gwusage.LatencyBoundsSeconds()...),
	)
	if err != nil {
		slog.Error("dataplane: the request-duration histogram could not be created; this metric will be missing", "error", err)
		return noopHistogram{}
	}
	return h
}()

// streamTTFB is time-to-first-byte, recorded only for streamed responses.
//
// It is a separate instrument rather than a label on the duration histogram
// because it answers a different question and has a different distribution. A
// streamed request's total duration is dominated by how long the model chose to
// talk, which is not a property of this gateway; the wait before the first
// token is. Mixing them produces a graph where a chatty model looks like an
// outage.
var streamTTFB = func() metric.Float64Histogram {
	h, err := otel.Meter("fairlb/gateway").Float64Histogram(
		"gateway_stream_ttfb_seconds",
		metric.WithDescription("Time from receiving a streaming request to the first byte delivered to the client"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(gwusage.LatencyBoundsSeconds()...),
	)
	if err != nil {
		slog.Error("dataplane: the stream TTFB histogram could not be created; this metric will be missing", "error", err)
		return noopHistogram{}
	}
	return h
}()

// requestCounter counts terminal outcomes.
//
// The histogram already carries a count per bucket, so this is not redundant
// bookkeeping: a request that reached a terminal outcome without a usable
// duration still has to be counted, and the error rate must not depend on
// whether a duration happened to be measurable.
var requestCounter = func() metric.Int64Counter {
	c, err := otel.Meter("fairlb/gateway").Int64Counter(
		"gateway_requests_total",
		metric.WithDescription("Requests that reached a terminal outcome, by surface, status and whether the response streamed"),
	)
	if err != nil {
		slog.Error("dataplane: the request counter could not be created; this metric will be missing", "error", err)
		return noopCounter{}
	}
	return c
}()

// outcomeKey identifies one attribute combination.
type outcomeKey struct {
	surface string
	status  string
	stream  bool
}

// outcomeAttrs memoises the attribute set per combination.
//
// This records on every proxied request, so building the attributes each time
// would add an allocation per request to the hottest path in the product for a
// label space that never changes. The combinations are bounded by construction
// -- surface comes from the catalogue's closed set, status from statusOrOK and
// failureStatus, which between them return four values -- so the map reaches
// its final size within the first minutes of traffic and stops.
//
// That bound is the load-bearing part. Memoising attributes derived from
// request data (an org, a model, a key) would be an unbounded map keyed by
// something the caller controls, which is a leak wearing a cache's clothes.
var outcomeAttrs sync.Map // outcomeKey -> metric.MeasurementOption

func outcomeAttributes(k outcomeKey) metric.MeasurementOption {
	if v, ok := outcomeAttrs.Load(k); ok {
		return v.(metric.MeasurementOption)
	}
	opt := metric.WithAttributeSet(attribute.NewSet(
		attribute.String("surface", k.surface),
		attribute.String("status", k.status),
		attribute.Bool("stream", k.stream),
	))
	outcomeAttrs.Store(k, opt)
	return opt
}

// recordOutcome records one terminal request outcome.
//
// status is the usage row's status column, so the metric and the row always
// agree on what happened; passing anything else would create a second, subtly
// different vocabulary for the same event.
func recordOutcome(ctx context.Context, surface, status string, stream bool, elapsed time.Duration) {
	attrs := outcomeAttributes(outcomeKey{surface: surface, status: status, stream: stream})
	requestCounter.Add(ctx, 1, attrs)
	requestDuration.Record(ctx, elapsed.Seconds(), attrs)
}

// surfaceAttrs memoises the single-attribute set, for the same reason and under
// the same bound as outcomeAttrs: this records once per streamed request, and
// surface is a closed set.
var surfaceAttrs sync.Map // string -> metric.MeasurementOption

func surfaceAttribute(surface string) metric.MeasurementOption {
	if v, ok := surfaceAttrs.Load(surface); ok {
		return v.(metric.MeasurementOption)
	}
	opt := metric.WithAttributeSet(attribute.NewSet(attribute.String("surface", surface)))
	surfaceAttrs.Store(surface, opt)
	return opt
}

// recordStreamTTFB records the first-byte latency of one streamed response. A
// non-positive value means no first byte was ever delivered, which is not a
// TTFB observation and must not be recorded as a fast one.
func recordStreamTTFB(ctx context.Context, surface string, ttfb time.Duration) {
	if ttfb <= 0 {
		return
	}
	streamTTFB.Record(ctx, ttfb.Seconds(), surfaceAttribute(surface))
}

// noopHistogram is the fallback when no meter is available.
type noopHistogram struct{ metric.Float64Histogram }

func (noopHistogram) Record(context.Context, float64, ...metric.RecordOption) {}
