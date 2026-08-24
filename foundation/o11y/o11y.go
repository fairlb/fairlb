// Package o11y wires up the three observability signals, OpenTelemetry first:
//   - tracing: an OTLP exporter, enabled by setting its endpoint, so the backend
//     can be swapped. With no exporter configured, trace ids are still generated
//     so that log correlation keeps working.
//   - metrics: a Prometheus-compatible /metrics endpoint.
//   - logging: trace and span ids are attached automatically (see slog.go).
package o11y

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/otel/propagation"
)

// Telemetry holds the globally registered components; Shutdown tears them down
// in reverse order.
type Telemetry struct {
	metricsHandler http.Handler
	shutdowns      []func(context.Context) error
}

// Setup initializes tracing and metrics and registers them globally.
func Setup(ctx context.Context, env, version string) (*Telemetry, error) {
	t := &Telemetry{}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "fairlb"),
		attribute.String("service.version", version),
		attribute.String("deployment.environment.name", env),
	))
	if err != nil {
		return nil, err
	}

	// Tracing: with no endpoint configured, no processor is attached — spans
	// are dropped, but trace ids are still generated.
	//
	// "Dropped" has to mean *not recorded*, not merely not exported. The SDK's
	// default sampler is AlwaysSample, so an unconfigured deployment was paying
	// the full cost of every span — allocating the attributes, holding the
	// events — and then discarding the lot at a batcher that does not exist.
	// On a gateway that wraps its data plane in an HTTP handler that is a
	// per-request cost bought for nothing.
	//
	// Not sampling is the right off switch rather than skipping the provider,
	// because the promise above still has to hold: a non-recording span still
	// carries a valid SpanContext, so the trace and span ids that slog.go
	// attaches are unchanged. Log correlation keeps working; only the recording
	// stops. Setting the endpoint switches sampling back on with it.
	//
	// It has to be *parent-based*. A bare NeverSample is unconditional: it
	// overrides an incoming traceparent that says sampled, and because the
	// local SpanContext is what gets propagated onward, every downstream
	// service using the standard parent-based sampler would stop sampling too.
	// A deployment sitting in the middle of somebody else's traced call chain
	// would silently truncate it. ParentBased keeps the saving for traffic that
	// starts here, which is all of it in the case this branch exists for, and
	// leaves a caller's decision alone.
	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		traceOpts = append(traceOpts, sdktrace.WithBatcher(exp))
	} else {
		traceOpts = append(traceOpts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())))
	}
	tp := sdktrace.NewTracerProvider(traceOpts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.shutdowns = append(t.shutdowns, tp.Shutdown)

	// Metrics: its own registry, with the runtime and process collectors.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(promExp))
	otel.SetMeterProvider(mp)
	t.shutdowns = append(t.shutdowns, mp.Shutdown)

	t.metricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return t, nil
}

// MetricsHandler returns the Prometheus-compatible handler for /metrics.
func (t *Telemetry) MetricsHandler() http.Handler { return t.metricsHandler }

// Shutdown closes every component in reverse order, flushing anything not yet
// exported.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdowns) - 1; i >= 0; i-- {
		errs = append(errs, t.shutdowns[i](ctx))
	}
	return errors.Join(errs...)
}
