/**
 * Tests for the HTTP tracing middleware. Deterministic: no live network, no LLM. The middleware is
 * driven directly with mock request/response objects so the response lifecycle (and thus the
 * end-of-request flush) is controlled explicitly.
 */
import { EventEmitter } from 'node:events';
import { type NextFunction, type Request, type Response } from 'express';

import { SpanKind, SpanStatusCode, context, propagation, trace } from '@opentelemetry/api';
import { type ReadableSpan, InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { afterEach, describe, expect, it } from 'vitest';

import { HEALTH_PATH, httpMiddleware } from './middleware';
import { TracesExporter, flush, install } from './obs';
import { inject } from './propagation';

function resetOtel(): void {
  trace.disable();
  propagation.disable();
  context.disable();
}

function recording(): { exporter: InMemorySpanExporter; shutdown: () => Promise<void> } {
  const exporter = new InMemorySpanExporter();
  const shutdown = install(exporter, { exporter: TracesExporter.Console, serviceName: 'automation-agent' });
  return { exporter, shutdown };
}

function mockReq(method: string, path: string, headers: Record<string, string> = {}): Request {
  return { method, path, headers } as unknown as Request;
}

/** A response stub that carries a status code and emits lifecycle events like the real one. */
function mockRes(statusCode = 202): Response & EventEmitter {
  const res = new EventEmitter() as Response & EventEmitter;
  res.statusCode = statusCode;
  return res;
}

/** Emit an ended agent span so the batch buffer has something to (not) flush. */
function bufferSpan(): void {
  trace.getTracer('obs-test').startSpan('invoke_agent automation_agent').end();
}

function findSpan(exporter: InMemorySpanExporter, name: string): ReadableSpan {
  const span = exporter.getFinishedSpans().find((s) => s.name === name);
  if (!span) {
    throw new Error(`span not found: ${name}`);
  }
  return span;
}

const noopNext: NextFunction = () => {};

describe('obs httpMiddleware', () => {
  afterEach(resetOtel);

  it('creates one server span per request without altering the response', async () => {
    const { exporter, shutdown } = recording();
    try {
      const mw = httpMiddleware();
      const res = mockRes(202);
      let called = 0;
      mw(mockReq('POST', '/webhooks/lint'), res, () => {
        called += 1;
      });
      res.emit('finish');
      await flush();

      expect(called).toBe(1);
      const spans = exporter.getFinishedSpans();
      expect(spans).toHaveLength(1);
      expect(spans[0]!.name).toBe('POST /webhooks/lint');
      expect(spans[0]!.kind).toBe(SpanKind.SERVER);
      expect(spans[0]!.attributes['http.response.status_code']).toBe(202);
    } finally {
      await shutdown();
    }
  });

  it('excludes the health probe from tracing', async () => {
    const { exporter, shutdown } = recording();
    try {
      let called = 0;
      httpMiddleware()(mockReq('GET', HEALTH_PATH), mockRes(200), () => {
        called += 1;
      });
      await flush();
      expect(called).toBe(1);
      expect(exporter.getFinishedSpans()).toHaveLength(0);
    } finally {
      await shutdown();
    }
  });

  it('does not flush on the health probe', async () => {
    const { exporter, shutdown } = recording();
    try {
      bufferSpan();
      const mw = httpMiddleware();

      // The health probe leaves the buffered span un-exported (no span, no flush).
      mw(mockReq('GET', HEALTH_PATH), mockRes(200), noopNext);
      expect(exporter.getFinishedSpans()).toHaveLength(0);

      // A traced request flushes them (its own server span plus the buffered one).
      const res = mockRes(202);
      mw(mockReq('POST', '/webhooks/lint'), res, noopNext);
      res.emit('finish');
      await flush();
      expect(exporter.getFinishedSpans().length).toBeGreaterThan(0);
    } finally {
      await shutdown();
    }
  });

  it('records an error status on a 5xx response', async () => {
    const { exporter, shutdown } = recording();
    try {
      const res = mockRes(500);
      httpMiddleware()(mockReq('POST', '/webhooks/lint'), res, noopNext);
      res.emit('finish');
      await flush();
      expect(findSpan(exporter, 'POST /webhooks/lint').status.code).toBe(SpanStatusCode.ERROR);
    } finally {
      await shutdown();
    }
  });

  it('records an exception when the chain throws synchronously', async () => {
    const { exporter, shutdown } = recording();
    try {
      const boom = new Error('boom');
      // The framework's error handling turns the throw into a 500 and completes the response.
      const res = mockRes(500);
      expect(() =>
        httpMiddleware()(mockReq('POST', '/webhooks/lint'), res, () => {
          throw boom;
        }),
      ).toThrow('boom');
      // The span records the exception on throw but is completed by the response event (not in the
      // catch), so the recorded status reflects the real response the framework ultimately sends.
      res.emit('close');
      await flush();

      const span = findSpan(exporter, 'POST /webhooks/lint');
      expect(span.status.code).toBe(SpanStatusCode.ERROR);
      expect(span.events.some((e) => e.name === 'exception')).toBe(true);
    } finally {
      await shutdown();
    }
  });

  it('continues an incoming trace from a task traceparent header', async () => {
    const { exporter, shutdown } = recording();
    try {
      // Model a task carrying a traceparent injected by the transport on enqueue.
      const ingress = trace.getTracer('obs-test').startSpan('ingress');
      const carrier = inject({}, trace.setSpan(context.active(), ingress));
      ingress.end();

      const res = mockRes(200);
      httpMiddleware()(mockReq('POST', '/internal/dispatch', carrier), res, noopNext);
      res.emit('finish');
      await flush();

      const dispatch = findSpan(exporter, 'POST /internal/dispatch');
      expect(dispatch.spanContext().traceId).toBe(ingress.spanContext().traceId);
      expect(dispatch.parentSpanContext?.spanId).toBe(ingress.spanContext().spanId);
    } finally {
      await shutdown();
    }
  });

  it('is a true no-op when tracing is disabled', () => {
    // No provider installed: the middleware calls next and attaches no listeners, so a request
    // with no lifecycle events still completes and nothing is traced.
    let called = 0;
    httpMiddleware()(mockReq('POST', '/webhooks/lint'), mockRes(202), () => {
      called += 1;
    });
    expect(called).toBe(1);
  });
});
