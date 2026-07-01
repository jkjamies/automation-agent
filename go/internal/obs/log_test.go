package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func TestLogHandlerAddsTraceContext(t *testing.T) {
	installRecording(t)
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	ctx, span := otel.GetTracerProvider().Tracer("obs-test").Start(context.Background(), "work")
	sc := span.SpanContext()
	logger.InfoContext(ctx, "did work")
	span.End()

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if rec["trace_id"] != sc.TraceID().String() {
		t.Errorf("trace_id = %v, want %s", rec["trace_id"], sc.TraceID())
	}
	if rec["span_id"] != sc.SpanID().String() {
		t.Errorf("span_id = %v, want %s", rec["span_id"], sc.SpanID())
	}
}

func TestLogHandlerNoSpanIsClean(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	// No active span: the record must not gain trace_id/span_id.
	logger.InfoContext(context.Background(), "no span here")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("log line carries trace_id with no active span: %s", buf.String())
	}
}

func TestLogHandlerSurvivesWith(t *testing.T) {
	installRecording(t)
	var buf bytes.Buffer
	// logger.With(...) builds a derived handler via WithAttrs; correlation must survive it.
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("component", "test")

	ctx, span := otel.GetTracerProvider().Tracer("obs-test").Start(context.Background(), "work")
	want := span.SpanContext().TraceID().String()
	logger.InfoContext(ctx, "did work")
	span.End()

	if !strings.Contains(buf.String(), want) {
		t.Errorf("derived logger (With) dropped trace correlation; line: %s", buf.String())
	}
}

func TestLogHandlerSurvivesWithGroup(t *testing.T) {
	installRecording(t)
	var buf bytes.Buffer
	// logger.WithGroup(...) builds a derived handler via WithGroup; correlation must survive.
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil))).WithGroup("run")

	ctx, span := otel.GetTracerProvider().Tracer("obs-test").Start(context.Background(), "work")
	want := span.SpanContext().TraceID().String()
	logger.InfoContext(ctx, "did work")
	span.End()

	if !strings.Contains(buf.String(), want) {
		t.Errorf("derived logger (WithGroup) dropped trace correlation; line: %s", buf.String())
	}
}

// invalidSpanContext is a defensive check: a record under a no-op span (invalid context)
// gains no ids.
func TestLogHandlerInvalidSpanContextSkipped(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := trace.ContextWithSpanContext(context.Background(), trace.SpanContext{})
	logger.InfoContext(ctx, "invalid span")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("invalid span context still added trace_id: %s", buf.String())
	}
}
