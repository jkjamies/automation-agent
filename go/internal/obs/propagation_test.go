package obs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// startSpan roots a sampled span and returns its context and span context. installRecording
// registers a parentbased-always-on provider, so the root span is sampled and therefore
// propagatable (an unsampled span injects no traceparent).
func startSpan(ctx context.Context) (context.Context, trace.SpanContext) {
	ctx, span := otel.GetTracerProvider().Tracer("obs-test").Start(ctx, "ingress")
	return ctx, span.SpanContext()
}

func TestInjectExtractRoundTripCloudTasks(t *testing.T) {
	installRecording(t)
	ingressCtx, ingress := startSpan(context.Background())

	// Enqueue side: inject the trace context into the (would-be Cloud Tasks) headers.
	carrier := Inject(ingressCtx)
	if carrier["traceparent"] == "" {
		t.Fatal("Inject produced no traceparent header for a sampled span")
	}

	// Dispatch side: a fresh process/request with only the carrier reconstructs the trace.
	dispatchCtx := Extract(context.Background(), carrier)
	extracted := trace.SpanContextFromContext(dispatchCtx)
	if extracted.TraceID() != ingress.TraceID() {
		t.Errorf("extracted trace id %s != ingress %s", extracted.TraceID(), ingress.TraceID())
	}
	if !extracted.IsRemote() {
		t.Error("extracted span context should be marked remote (it crossed a process hop)")
	}

	// The dispatch root span continues the ingress trace.
	_, dispatch := otel.GetTracerProvider().Tracer("obs-test").Start(dispatchCtx, "dispatch")
	if dispatch.SpanContext().TraceID() != ingress.TraceID() {
		t.Errorf("dispatch span trace id %s != ingress %s — trace did not continue", dispatch.SpanContext().TraceID(), ingress.TraceID())
	}
	if dispatch.SpanContext().SpanID() == ingress.SpanID() {
		t.Error("dispatch span should be a new span, not reuse the ingress span id")
	}
}

func TestInProcessPassthroughSharesTrace(t *testing.T) {
	installRecording(t)
	ingressCtx, ingress := startSpan(context.Background())

	// The inprocess backend carries the span on the Go context (context.WithoutCancel),
	// with no HTTP carrier. Cancellation is stripped, but the span — and thus the trace —
	// rides along unchanged.
	dispatchCtx := context.WithoutCancel(ingressCtx)
	carried := trace.SpanContextFromContext(dispatchCtx)
	if carried.TraceID() != ingress.TraceID() || carried.SpanID() != ingress.SpanID() {
		t.Error("inprocess passthrough lost the span context")
	}

	// Both backends yield the same logical trace: the cloudtasks header round-trip and the
	// inprocess passthrough resolve to one trace id.
	viaHeader := trace.SpanContextFromContext(Extract(context.Background(), Inject(ingressCtx)))
	if viaHeader.TraceID() != carried.TraceID() {
		t.Errorf("cloudtasks (%s) and inprocess (%s) produced different traces", viaHeader.TraceID(), carried.TraceID())
	}
}

func TestInjectDisabledIsEmpty(t *testing.T) {
	restoreGlobals(t)
	// With no propagator/provider registered (tracing off), Inject adds nothing — no
	// traceparent leaks onto a task when the feature is disabled.
	if got := Inject(context.Background()); len(got) != 0 {
		t.Errorf("Inject with tracing disabled returned %v, want empty", got)
	}
}
