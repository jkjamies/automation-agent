package obs

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler decorates a slog.Handler so every record emitted while a span is active also
// carries that span's trace_id and span_id. This is the log<->trace correlation seam: it
// lets a backend pivot from a log line to the trace it belongs to (and, on the GCP path,
// Cloud Logging auto-links the two). When no span is active — or tracing is disabled, in
// which case the active span is the framework's no-op span with an invalid context — it
// adds nothing, so logging is unchanged.
type logHandler struct {
	slog.Handler
}

// NewLogHandler wraps base so records gain trace_id/span_id from the active span on the
// record's context. The entrypoint wraps its slog handler with this once; correlation then
// applies to any context-aware log call (slog's *Context methods) made under a span.
func NewLogHandler(base slog.Handler) slog.Handler {
	return logHandler{Handler: base}
}

// Handle adds the active span's ids to the record before delegating. The span comes from
// the record's context, so only context-aware log calls correlate — a deliberate, zero-cost
// design: a log call with no span (or with tracing off) is delegated untouched.
func (h logHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs and WithGroup preserve the decoration across derived handlers: slog calls these
// to build child handlers (logger.With(...), logger.WithGroup(...)), and without overriding
// them the wrapper would be unwrapped back to the bare base handler, dropping correlation.
func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{Handler: h.Handler.WithGroup(name)}
}
