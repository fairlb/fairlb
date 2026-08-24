package o11y

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/fairlb/fairlb/foundation/config"
)

// NewLogger builds the process logger: text format at debug level in
// development, JSON at info level elsewhere. When the context carries a span,
// the trace and span ids are attached automatically.
func NewLogger(env string, w io.Writer) *slog.Logger {
	if env == config.EnvDev {
		return NewLoggerFormat(FormatText, w)
	}
	return NewLoggerFormat(FormatJSON, w)
}

// Log output formats.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// NewLoggerFormat builds the process logger for an explicitly chosen format,
// for builds that have no deployment tier to infer it from.
//
// Text implies debug level and JSON implies info: the format is really a
// statement about who is reading. Text means a person is watching a terminal
// and wants detail; JSON means a log pipeline is collecting, where debug lines
// are volume nobody asked for.
func NewLoggerFormat(format string, w io.Writer) *slog.Logger {
	var h slog.Handler
	if format == FormatJSON {
		h = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		h = slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	return slog.New(traceHandler{h})
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
