/*
 * Log <-> trace correlation for the obs package.
 *
 * Wraps the injected logger so every call made while a span is active also carries that span's
 * trace_id and span_id. This lets a backend pivot from a log line to the trace it belongs to (and,
 * on the cloud path, the trace console auto-links the two). It reads the active span from the
 * current context, so it is zero-cost when no span is active or tracing is off — the message is then
 * passed through untouched.
 *
 * The injected logger is the JDK platform logger, which is a plain (not structured) sink: its log
 * calls carry no key/value fields, so the correlation ids are appended to the message text rather
 * than attached as separate attributes. That is the one place this port's correlation differs from a
 * structured-logger port — the data (trace_id / span_id under an active span) is the same.
 */
package com.automation.agent.obs

import io.opentelemetry.api.trace.Span
import java.util.ResourceBundle

/**
 * Wrap [base] so records emitted under an active span gain trace_id / span_id. The entrypoint wraps
 * the one injected logger once and hands the result to the request-path subsystems; correlation then
 * applies to any log call made while a span is active. A call with no active span (or with tracing
 * off) delegates with its message untouched.
 */
fun newLogHandler(base: System.Logger): System.Logger = TraceCorrelatingLogger(base)

/** A [System.Logger] that appends the active span's ids to each message it forwards. */
private class TraceCorrelatingLogger(private val delegate: System.Logger) : System.Logger {
    override fun getName(): String = delegate.name

    override fun isLoggable(level: System.Logger.Level): Boolean = delegate.isLoggable(level)

    override fun log(level: System.Logger.Level, bundle: ResourceBundle?, msg: String?, thrown: Throwable?) {
        // With a bundle, msg is a lookup key, not a message — passing it through correlate() would
        // corrupt the key. Correlate only plain (bundle-less) records.
        delegate.log(level, bundle, if (bundle == null) correlate(msg) else msg, thrown)
    }

    override fun log(level: System.Logger.Level, bundle: ResourceBundle?, format: String?, vararg params: Any?) {
        // Same bundle-key guard as above: only a bundle-less format string is a real message.
        delegate.log(level, bundle, if (bundle == null) correlate(format) else format, *params)
    }

    /**
     * Append trace_id / span_id from the active span. When no span is active — or tracing is
     * disabled, in which case the active span has an invalid context — the message is returned
     * unchanged. Appending after the format text is safe: it references no positional params.
     */
    private fun correlate(msg: String?): String? {
        val sc = Span.current().spanContext
        if (!sc.isValid) return msg
        return "${msg ?: ""} trace_id=${sc.traceId} span_id=${sc.spanId}"
    }
}
