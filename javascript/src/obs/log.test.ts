/**
 * Tests for log <-> trace correlation. Deterministic: no live network, no LLM.
 */
import { context, propagation, trace } from '@opentelemetry/api';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { afterEach, describe, expect, it } from 'vitest';

import { type Logger, newLogHandler } from './log';
import { install, TracesExporter } from './obs';

function resetOtel(): void {
  trace.disable();
  propagation.disable();
  context.disable();
}

/** A logger that records the fields of the last call at each level. */
function captureLogger(): { logger: Logger; calls: Array<{ msg: string; fields?: Record<string, unknown> }> } {
  const calls: Array<{ msg: string; fields?: Record<string, unknown> }> = [];
  const record = (msg: string, fields?: Record<string, unknown>): void => {
    calls.push({ msg, fields });
  };
  return { logger: { info: record, warn: record, error: record }, calls };
}

describe('obs log correlation', () => {
  afterEach(resetOtel);

  it('stamps trace_id / span_id on records emitted under a span', async () => {
    const shutdown = install(new InMemorySpanExporter(), {
      exporter: TracesExporter.Console,
      serviceName: 'automation-agent',
    });
    try {
      const { logger, calls } = captureLogger();
      const rlog = newLogHandler(logger);

      const span = trace.getTracer('obs-test').startSpan('work');
      const sc = span.spanContext();
      context.with(trace.setSpan(context.active(), span), () => {
        rlog.info('under a span', { repo: 'a/b' });
      });
      span.end();

      expect(calls).toHaveLength(1);
      expect(calls[0]!.fields).toMatchObject({ repo: 'a/b', trace_id: sc.traceId, span_id: sc.spanId });
    } finally {
      await shutdown();
    }
  });

  it('passes fields through unchanged when no span is active', () => {
    const { logger, calls } = captureLogger();
    const rlog = newLogHandler(logger);
    rlog.warn('no span', { k: 1 });
    expect(calls[0]!.fields).toEqual({ k: 1 });
    expect(calls[0]!.fields).not.toHaveProperty('trace_id');
  });

  it('handles a call with no fields', () => {
    const { logger, calls } = captureLogger();
    newLogHandler(logger).error('boom');
    expect(calls[0]).toEqual({ msg: 'boom', fields: undefined });
  });
});
