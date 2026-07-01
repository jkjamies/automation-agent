package obs

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// healthPath is the liveness endpoint excluded from tracing: it is polled constantly and
// carries no causal interest, so a span per probe would be pure noise.
const healthPath = "/healthz"

// flushTimeout is a backstop on the end-of-request span flush: it exists only to release a
// pathologically wedged exporter, so it sits *above* the exporters' own request timeouts
// (OTLP / Cloud Trace default to ~10s). A tighter bound would be counterproductive — it
// could cancel a slow-but-working export and lose the very trailing batch this flush exists
// to guarantee. The real bound is the exporter's own timeout; this only catches a hang.
const flushTimeout = 30 * time.Second

// HTTPMiddleware wraps an HTTP handler so every inbound request (except the health probe)
// gets a server span, and the span buffer is force-flushed before the response returns.
//
// The server span is the trace root on the ingress path and continues the ingress trace
// on the Cloud Tasks dispatch path: the instrumentation reads the W3C trace context from
// the inbound headers via the global propagator, so a task carrying a "traceparent" header
// (injected by the transport on enqueue) makes the dispatch span a child of the ingress
// span automatically — no manual extract here.
//
// The flush is load-bearing, not a tuning knob: BatchSpanProcessor exports asynchronously,
// but Cloud Run throttles CPU the instant a response is sent, so an un-flushed trailing
// batch is lost when the instance is reclaimed. Flushing uniformly here — including on the
// fast 202 ingress path — costs one export per request (negligible at webhook volume) and
// removes the scale-to-zero span-loss path entirely. When tracing is disabled, both the
// instrumentation and the flush are no-ops, so the wrapped handler behaves identically.
func HTTPMiddleware(next http.Handler) http.Handler {
	instrumented := otelhttp.NewHandler(
		next, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != healthPath
		}),
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instrumented.ServeHTTP(w, r)
		if r.URL.Path == healthPath {
			// The health probe is excluded from tracing (WithFilter above), so it creates no
			// span and has nothing of its own to flush — and it is polled constantly, so a
			// ForceFlush here would be pure overhead on the hottest path (and would ship other
			// requests' batches early, defeating batching). Skip it.
			return
		}
		// Export buffered spans while CPU is still allocated for this request. Detach from
		// the request context first: a client that disconnects the instant the response is
		// written would otherwise cancel r.Context() and abort the flush — defeating the
		// scale-to-zero span-loss guard this flush exists for. The timeout is only a hang
		// backstop above the exporter's own timeout (see flushTimeout).
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), flushTimeout)
		defer cancel()
		_ = Flush(flushCtx)
	})
}
