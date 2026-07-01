/*
 * The HTTP tracing middleware for the obs package.
 *
 * Installs a request interceptor so every inbound request (except the health probe) gets a server
 * span, and the span buffer is force-flushed before the response completes.
 *
 * The server span is the trace root on the ingress path and continues the ingress trace on the Cloud
 * Tasks dispatch path: it is started from the W3C trace context read out of the inbound headers via
 * the global propagator, so a task carrying a traceparent header (injected by the transport on
 * enqueue) makes the dispatch span a child of the ingress span automatically.
 *
 * The flush is load-bearing, not a tuning knob: the batch span processor exports asynchronously, but
 * Cloud Run throttles CPU the instant a response is sent, so an un-flushed trailing batch is lost
 * when the instance is reclaimed. Flushing uniformly here — including on the fast 202 ingress path —
 * costs one export per request (negligible at webhook volume) and removes the scale-to-zero
 * span-loss path entirely. When tracing is disabled the interceptor is a true no-op, so the wrapped
 * app behaves identically.
 */
package com.automation.agent.obs

import io.ktor.server.application.Application
import io.ktor.server.application.ApplicationCall
import io.ktor.server.application.ApplicationCallPipeline
import io.ktor.server.application.call
import io.ktor.server.request.httpMethod
import io.ktor.server.request.path
import io.opentelemetry.api.trace.SpanKind
import io.opentelemetry.api.trace.StatusCode
import io.opentelemetry.extension.kotlin.asContextElement
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.withContext
import kotlin.coroutines.cancellation.CancellationException

/**
 * The liveness endpoint excluded from tracing: it is polled constantly and carries no causal
 * interest, so a span per probe would be pure noise — and flushing on it would ship other requests'
 * buffered batches early on the hottest path.
 */
const val HEALTH_PATH = "/healthz"

/**
 * Install the server-span interceptor on an [Application]. Mount it before the routes so the framework's
 * spans parent to the server span. It is a no-op when tracing is disabled and skips the
 * constantly-polled health probe. The entrypoint wires it into the webhook server; a test installs it
 * directly on a test application.
 */
fun Application.installObsTracing() {
    intercept(ApplicationCallPipeline.Setup) {
        val path = call.request.path()
        // Non-traced paths get no span and no flush: tracing off (the default), or the health probe
        // (which carries no causal interest, and a flush on it would ship other requests' batches
        // early on the hottest path).
        if (!isEnabled() || path == HEALTH_PATH) {
            proceed()
            return@intercept
        }

        val parent = extract(headerMap(call))
        val span = tracer()
            .spanBuilder("${call.request.httpMethod.value} $path")
            .setSpanKind(SpanKind.SERVER)
            .setParent(parent)
            .startSpan()
        val ctx = parent.with(span)

        var hadException = false
        try {
            // Run the rest of the pipeline with the server span active so the framework's spans parent
            // to it (the coroutine element carries the context across suspension).
            withContext(ctx.asContextElement()) { proceed() }
        } catch (ce: CancellationException) {
            // A client disconnect / coroutine cancellation is not a handler failure: rethrow without
            // tagging the span as an error (the finally still ends and flushes it).
            throw ce
        } catch (t: Throwable) {
            hadException = true
            // A thrown handler failure: record it on the span, then re-raise so the framework's own
            // error handling is unchanged.
            span.setStatus(StatusCode.ERROR, t.message ?: t.toString())
            span.recordException(t)
            throw t
        } finally {
            val status = call.response.status()?.value
            if (status != null) {
                span.setAttribute("http.response.status_code", status.toLong())
            }
            // A 5xx marks a request failed on the trace, but skip it when the throw path already set a
            // message-bearing ERROR status, so the exception message is not overwritten with a bare one.
            if (status != null && status >= 500 && !hadException) {
                span.setStatus(StatusCode.ERROR)
            }
            span.end()
            flushAfterRequest()
        }
    }
}

/**
 * Export buffered spans while CPU is still allocated for this request. The blocking force-flush runs
 * off the request thread and uncancellable, so a client disconnecting right after the response cannot
 * cancel the flush — it must complete to guard against scale-to-zero span loss. The force-flush's own
 * timeout is the hang backstop (see [FLUSH_TIMEOUT_MS]). A no-op when tracing is disabled.
 */
private suspend fun flushAfterRequest() {
    if (!isEnabled()) return
    withContext(NonCancellable + Dispatchers.IO) { flush() }
}

/** Collapse the inbound request headers to a traceparent-friendly map (first value of each header). */
private fun headerMap(call: ApplicationCall): Map<String, String> {
    val out = HashMap<String, String>()
    for (name in call.request.headers.names()) {
        val value = call.request.headers[name]
        if (value != null) out[name] = value
    }
    return out
}
