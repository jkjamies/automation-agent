/*
 * Span-exporter construction for the obs package.
 *
 * Builds the one exporter selected by config. The application names no vendor: every OTLP backend is
 * reached through [EXPORTER_OTLP] + endpoint, and [EXPORTER_GCP] is the one convenience path (Cloud
 * Trace via Application Default Credentials).
 */
package com.automation.agent.obs

import com.google.cloud.opentelemetry.trace.TraceExporter
import io.opentelemetry.exporter.logging.LoggingSpanExporter
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter
import io.opentelemetry.sdk.trace.export.SpanExporter

/**
 * Build the span exporter for [cfg] exporter. The caller ([init]) has already rejected
 * [EXPORTER_NONE] (no exporter) and any unknown value, so this handles only the three real sinks.
 *
 * @throws IllegalArgumentException on an [EXPORTER_OTLP] config with no endpoint, or an unknown
 *   exporter.
 */
internal fun newExporter(cfg: Config): SpanExporter = when (cfg.exporter) {
    EXPORTER_CONSOLE -> LoggingSpanExporter.create()
    EXPORTER_OTLP -> {
        val endpoint = cfg.otlpEndpoint.trim()
        // config validates this, but guard so a direct caller fails loudly rather than silently
        // exporting nowhere.
        require(endpoint.isNotEmpty()) { "obs: exporter \"$EXPORTER_OTLP\" requires an OTLP endpoint" }
        val builder = OtlpHttpSpanExporter.builder().setEndpoint(endpoint)
        for ((key, value) in parseOtlpHeaders(cfg.otlpHeaders)) {
            builder.addHeader(key, value)
        }
        builder.build()
    }
    // No project id: the Cloud Trace exporter detects it from Application Default Credentials / the
    // metadata server, matching how the rest of the cloud path authenticates.
    EXPORTER_GCP -> TraceExporter.createWithDefaultConfiguration()
    else -> throw IllegalArgumentException("obs: unknown OTEL_TRACES_EXPORTER \"${cfg.exporter}\"")
}

/**
 * Parse the standard OTEL_EXPORTER_OTLP_HEADERS form — comma-separated key=value pairs (e.g.
 * "api-key=secret,env=prod") — into a header map. Blank entries and entries without a key are
 * skipped; only the first "=" splits, so a value may contain "=".
 */
fun parseOtlpHeaders(raw: String): Map<String, String> {
    val out = LinkedHashMap<String, String>()
    for (pairRaw in raw.split(",")) {
        val pair = pairRaw.trim()
        if (pair.isEmpty()) continue
        val eq = pair.indexOf('=')
        // No "=", or an empty key: skip rather than record a keyless header.
        if (eq <= 0) continue
        val key = pair.substring(0, eq).trim()
        if (key.isEmpty()) continue
        out[key] = pair.substring(eq + 1).trim()
    }
    return out
}
