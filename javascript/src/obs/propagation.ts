/**
 * Backend-aware trace-context propagation for the obs package.
 *
 * The trace must cross the enqueue -> dispatch hop so the workflow trace continues from the
 * ingress span. This module is the {@link inject} / {@link extract} seam that abstracts both
 * transport backends: the Cloud Tasks backend injects the context as a W3C `traceparent` header on
 * the task, and the in-process backend inherits the context directly (the detached dispatch runs
 * within the active async context, so the span rides along with no carrier — mirroring how it
 * already skips the envelope codec).
 */

import { type Context, context, propagation } from '@opentelemetry/api';

/**
 * Return the trace-context carrier (the W3C `traceparent` header, and `tracestate` when present)
 * for the active context, suitable for attaching to an outbound HTTP request. The Cloud Tasks
 * transport merges it into the task's headers so the dispatch that runs the task continues the
 * ingress trace. When tracing is disabled — or the context carries no valid span — the propagator
 * injects nothing, so the returned map is empty and no header is added to the task.
 */
export function inject(carrier: Record<string, string> = {}, ctx: Context = context.active()): Record<string, string> {
  propagation.inject(ctx, carrier);
  return carrier;
}

/**
 * Return a context carrying the trace context found in `carrier`, rooting a new span as a child of
 * the upstream trace. The HTTP middleware extracts from inbound request headers; this explicit
 * helper backs the propagation round-trip tests and any non-HTTP carrier. A carrier with no trace
 * context yields the base context unchanged.
 */
export function extract(carrier: Record<string, string>, ctx: Context = context.active()): Context {
  return propagation.extract(ctx, carrier);
}
