package proxy

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// The counter of refused requests.
//
// *Why a metric rather than a log line per request*: a 4xx is the caller's
// input problem, not a server fault, and logging each one at ERROR would drown
// the real faults. Worse, it can be flooded -- hammering with one wrong key
// fills the log, and an invalid API key is by far the most frequent case. The
// question actually worth answering is "are there many of these, and are they
// increasing", which is a counter's job.
//
// Tracing an individual request is the usage log's job; every failure after
// authentication carries an org. An authentication failure has no org and
// cannot go in that table, so this metric is its only outlet.
//
// *The labels are bounded dimensions only*: the error code, of which there are
// a couple of dozen, and the surface, of which there are five. No org, key or
// model id -- those are unbounded cardinality and would overwhelm a time-series
// database, and slicing by organization belongs in the usage log anyway.
var rejectedCounter = func() metric.Int64Counter {
	c, err := otel.Meter("fairlb/gateway").Int64Counter(
		"gateway_requests_rejected_total",
		metric.WithDescription("Requests the dataplane refused before forwarding upstream: authentication, admission, budget, rate limiting and so on"),
	)
	if err != nil {
		// Failing to obtain a meter must not stop the dataplane from starting:
		// degrade to a no-op and leave one record at start-up.
		slog.Error("dataplane: the refused-request counter could not be created; this metric will be missing", "error", err)
		return noopCounter{}
	}
	return c
}()

// recordRejected counts one request refused before it reached an upstream.
func recordRejected(ctx context.Context, surface, errCode string) {
	rejectedCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("error_code", errCode),
		attribute.String("surface", surface),
	))
}

// The counter of failed upstream attempts.
//
// This is the other half of the attempt trail. The trail on a usage row answers
// "what happened to *this* request", which is the question a support ticket
// starts from; a counter answers "how often is this happening lately", which is
// the question that gets an upstream taken out of rotation. Neither one answers
// the other well: querying a jsonb column across a partitioned table to build a
// rate is the wrong tool, and a counter cannot be traced back to a request id.
//
// Same label discipline as above -- provider slug, surface and failure class
// are all small closed sets. No route id: routes are created freely and their
// cardinality is not bounded by anything.
var upstreamAttemptCounter = func() metric.Int64Counter {
	c, err := otel.Meter("fairlb/gateway").Int64Counter(
		"gateway_upstream_attempts_failed_total",
		metric.WithDescription("Upstream attempts that failed, by provider and failure class. A request that failed over twice counts twice here and once in the request counters"),
	)
	if err != nil {
		slog.Error("dataplane: the upstream-attempt counter could not be created; this metric will be missing", "error", err)
		return noopCounter{}
	}
	return c
}()

// recordAttempt counts one failed upstream attempt.
func recordAttempt(ctx context.Context, providerSlug string, cls Classification) {
	upstreamAttemptCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", providerSlug),
		attribute.String("class", cls.Class.String()),
	))
}

// noopCounter is the fallback when no meter is available.
type noopCounter struct{ metric.Int64Counter }

func (noopCounter) Add(context.Context, int64, ...metric.AddOption) {}
