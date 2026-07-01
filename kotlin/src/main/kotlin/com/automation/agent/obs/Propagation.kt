/*
 * Backend-aware trace-context propagation for the obs package.
 *
 * The trace must cross the enqueue -> dispatch hop so the workflow trace continues from the ingress
 * span. This module is the [inject] / [extract] seam that abstracts both transport backends: the
 * Cloud Tasks backend injects the context as a W3C traceparent header on the task, and the
 * in-process backend inherits the context directly through [currentTraceContextElement] (the
 * detached dispatch coroutine carries the active trace context, so the span rides along with no
 * carrier — mirroring how it already skips the envelope codec).
 */
package com.automation.agent.obs

import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.context.Context
import io.opentelemetry.context.propagation.TextMapGetter
import io.opentelemetry.context.propagation.TextMapSetter
import io.opentelemetry.extension.kotlin.asContextElement
import kotlin.coroutines.CoroutineContext
import kotlin.coroutines.EmptyCoroutineContext

/**
 * Return the trace-context carrier (the W3C traceparent header, and tracestate when present) for
 * [context], suitable for attaching to an outbound HTTP request. The Cloud Tasks transport merges it
 * into the task's headers so the dispatch that runs the task continues the ingress trace. When
 * tracing is disabled — or the context carries no valid span — the propagator injects nothing, so
 * the returned map is empty and no header is added to the task.
 */
fun inject(
    carrier: MutableMap<String, String> = mutableMapOf(),
    context: Context = Context.current(),
): MutableMap<String, String> {
    GlobalOpenTelemetry.getPropagators().textMapPropagator.inject(context, carrier, MapSetter)
    return carrier
}

/**
 * Return a context carrying the trace context found in [carrier], rooting a new span as a child of
 * the upstream trace. The HTTP middleware extracts from inbound request headers; this explicit
 * helper backs the propagation round-trip tests and any non-HTTP carrier. A carrier with no trace
 * context yields [context] unchanged.
 */
fun extract(carrier: Map<String, String>, context: Context = Context.current()): Context =
    GlobalOpenTelemetry.getPropagators().textMapPropagator.extract(context, carrier, MapGetter)

/**
 * A coroutine-context element that carries the active trace [Context] onto the threads a launched
 * coroutine runs on. The in-process transport captures it at enqueue time and launches the dispatch
 * with it, so the workflow's spans continue the ingress trace even though the dispatch outlives the
 * request (the analogue of dropping the trace context onto a detached execution context). It is
 * [EmptyCoroutineContext] when tracing is disabled, so the transport pays nothing on the default
 * path.
 */
fun currentTraceContextElement(): CoroutineContext =
    if (isEnabled()) Context.current().asContextElement() else EmptyCoroutineContext

/** Writes carrier entries for the propagator (the outbound task-header side of [inject]). */
private object MapSetter : TextMapSetter<MutableMap<String, String>> {
    override fun set(carrier: MutableMap<String, String>?, key: String, value: String) {
        carrier?.put(key, value)
    }
}

/** Reads carrier entries for the propagator, case-insensitively (inbound headers may vary in case). */
private object MapGetter : TextMapGetter<Map<String, String>> {
    override fun keys(carrier: Map<String, String>): Iterable<String> = carrier.keys
    override fun get(carrier: Map<String, String>?, key: String): String? =
        carrier?.entries?.firstOrNull { it.key.equals(key, ignoreCase = true) }?.value
}
