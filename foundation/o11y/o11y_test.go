package o11y_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/fairlb/fairlb/foundation/o11y"
)

func TestLoggerAttachesTraceID(t *testing.T) {
	ctx := context.Background()
	tel, err := o11y.Setup(ctx, "production", "test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(ctx) })

	var buf bytes.Buffer
	logger := o11y.NewLogger("production", &buf)

	spanCtx, span := otel.Tracer("test").Start(ctx, "op")
	defer span.End()

	logger.InfoContext(spanCtx, "hello")
	line := buf.String()
	if !strings.Contains(line, `"trace_id":"`+span.SpanContext().TraceID().String()+`"`) {
		t.Errorf("the log line should carry the current trace id: %s", line)
	}
	if !strings.Contains(line, `"span_id"`) {
		t.Errorf("the log line should carry a span id: %s", line)
	}

	// A log line with no span must not carry a trace id.
	buf.Reset()
	logger.InfoContext(ctx, "plain")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("with no span there should be no trace id: %s", buf.String())
	}

	// With no exporter configured, spans must not be recorded -- an
	// unconfigured deployment should not pay to build spans nobody collects --
	// while still carrying the ids the assertions above depend on. Both halves
	// matter: sampling off without a valid SpanContext would silently take log
	// correlation with it, and that loss looks like nothing at all until
	// somebody needs to follow a request through the logs.
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		t.Skip("an exporter is configured in this environment, so sampling is on by design")
	}
	if span.IsRecording() {
		t.Error("with no exporter configured the span should not be recorded")
	}
	if !span.SpanContext().IsValid() {
		t.Error("a non-recording span must still carry a valid trace and span id")
	}
}

func TestMetricsHandlerServesPrometheus(t *testing.T) {
	ctx := context.Background()
	tel, err := o11y.Setup(ctx, "production", "test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(ctx) })

	rec := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics should include the runtime collectors, got %d bytes", len(body))
	}
}
