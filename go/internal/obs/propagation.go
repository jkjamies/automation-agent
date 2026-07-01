package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Inject returns the trace-context carrier (the W3C "traceparent" header, and "tracestate"
// when present) for ctx, suitable for attaching to an outbound HTTP request. The Cloud
// Tasks transport merges it into the task's headers so the dispatch that runs the task
// continues the ingress trace. When tracing is disabled — or ctx carries no span — the
// global propagator is a no-op and this returns an empty map, so no header is added.
//
// This is the cloudtasks half of the backend-aware propagation contract. The inprocess
// backend has no HTTP hop, so it carries the span on the Go context directly (via
// context.WithoutCancel) instead of a carrier — mirroring how it already skips the
// envelope JSON codec.
func Inject(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

// Extract returns a context carrying the trace context found in carrier, rooting a new
// span as a child of the upstream trace. The HTTP middleware extracts automatically from
// inbound request headers; this explicit helper backs the propagation round-trip tests
// and any non-HTTP carrier. A carrier with no trace context yields ctx unchanged.
func Extract(ctx context.Context, carrier map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
