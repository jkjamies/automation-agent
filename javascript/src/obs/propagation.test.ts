/**
 * Tests for backend-aware trace-context propagation (the Cloud Tasks header round-trip and the
 * in-process context passthrough). Deterministic: no live network, no LLM.
 */
import { type Span, context, propagation, trace } from '@opentelemetry/api';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { afterEach, describe, expect, it } from 'vitest';

import { install, TracesExporter } from './obs';
import { extract, inject } from './propagation';

function resetOtel(): void {
  trace.disable();
  propagation.disable();
  context.disable();
}

/** Register a recording provider (so spans are sampled and the propagator/context manager are set)
 * and return the shutdown. */
function setup(): () => Promise<void> {
  return install(new InMemorySpanExporter(), { exporter: TracesExporter.Console, serviceName: 'automation-agent' });
}

/** Start a span and return it plus a context with it active. */
function startSpan(name = 'ingress'): { span: Span; ctx: ReturnType<typeof trace.setSpan> } {
  const span = trace.getTracer('obs-test').startSpan(name);
  return { span, ctx: trace.setSpan(context.active(), span) };
}

describe('obs propagation', () => {
  afterEach(resetOtel);

  it('round-trips the trace context through a Cloud Tasks header', async () => {
    const shutdown = setup();
    try {
      const { span, ctx } = startSpan();
      const ingress = span.spanContext();

      // Enqueue side: inject the trace context into the (would-be Cloud Tasks) headers.
      const carrier = inject({}, ctx);
      expect(carrier.traceparent).toBeTruthy();
      span.end();

      // Dispatch side: a fresh request with only the carrier reconstructs the trace.
      const dispatchCtx = extract(carrier, context.active());
      const extracted = trace.getSpan(dispatchCtx)!.spanContext();
      expect(extracted.traceId).toBe(ingress.traceId);

      // The dispatch root span continues the ingress trace with a new span id.
      const dispatch = trace.getTracer('obs-test').startSpan('dispatch', {}, dispatchCtx);
      expect(dispatch.spanContext().traceId).toBe(ingress.traceId);
      expect(dispatch.spanContext().spanId).not.toBe(ingress.spanId);
      dispatch.end();
    } finally {
      await shutdown();
    }
  });

  it('shares the trace via in-process context passthrough', async () => {
    const shutdown = setup();
    try {
      const { span, ctx } = startSpan();
      const ingress = span.spanContext();

      // The in-process backend carries the span on the active context: running within context.with
      // makes the active span the same one, so the trace rides along with no carrier.
      const carriedTraceId = context.with(ctx, () => trace.getActiveSpan()!.spanContext().traceId);
      expect(carriedTraceId).toBe(ingress.traceId);

      // Both backends yield the same logical trace: the header round-trip resolves to one trace id.
      const viaHeader = trace.getSpan(extract(inject({}, ctx), context.active()))!.spanContext();
      expect(viaHeader.traceId).toBe(ingress.traceId);
      span.end();
    } finally {
      await shutdown();
    }
  });

  it('injects nothing when tracing is disabled', () => {
    // With no active span (tracing effectively off), inject adds nothing — no traceparent leaks
    // onto a task when the feature is disabled.
    expect(inject()).toEqual({});
  });
});
