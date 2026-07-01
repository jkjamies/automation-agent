/**
 * Tests for the observability package (tracer registration, exporters, propagation, middleware,
 * log correlation).
 *
 * Deterministic: no live network, no LLM. We assert on span names / attributes / structure, never
 * on model output. Each test resets the process-global OTel registrations in afterEach so the
 * provider, propagator, and context manager do not leak across tests.
 */
import { context, propagation, trace } from '@opentelemetry/api';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { afterEach, describe, expect, it } from 'vitest';

import { newExporter, parseOtlpHeaders } from './exporters';
import { type Config, TracesExporter, flush, init, install, isEnabled, parseSampler } from './obs';

/** Reset every OTel global so the next test starts from a clean, disabled state. */
function resetOtel(): void {
  trace.disable();
  propagation.disable();
  context.disable();
}

/** Register a recording exporter and return it plus the shutdown to release the provider. */
function recording(cfg: Partial<Config> = {}): { exporter: InMemorySpanExporter; shutdown: () => Promise<void> } {
  const exporter = new InMemorySpanExporter();
  const shutdown = install(exporter, { exporter: TracesExporter.Console, serviceName: 'automation-agent', ...cfg });
  return { exporter, shutdown };
}

/**
 * Create the agent framework's native span shape — invoke_agent -> call_llm -> execute_tool — with
 * representative GenAI-semconv attribute keys, without any model call. It stands in for the
 * framework's auto-emitted tree so tests assert on span structure and attribute keys.
 */
function emitFakeAgentTree(): void {
  const tracer = trace.getTracer('obs-test');
  const invoke = tracer.startSpan('invoke_agent automation_agent');
  const invokeCtx = trace.setSpan(context.active(), invoke);
  const llm = tracer.startSpan('call_llm gemma', {}, invokeCtx);
  llm.setAttribute('gen_ai.operation.name', 'chat');
  llm.setAttribute('gen_ai.request.model', 'gemma4:12b');
  llm.setAttribute('gen_ai.usage.input_tokens', 12);
  const llmCtx = trace.setSpan(invokeCtx, llm);
  const tool = tracer.startSpan('execute_tool apply_fix', {}, llmCtx);
  tool.setAttribute('gen_ai.tool.name', 'apply_fix');
  tool.end();
  llm.end();
  invoke.end();
}

describe('obs init / provider registration', () => {
  afterEach(resetOtel);

  it('init(none) installs nothing and returns a no-op shutdown', async () => {
    const shutdown = await init({ exporter: TracesExporter.None, serviceName: 'automation-agent' });
    expect(isEnabled()).toBe(false);
    await shutdown(); // the no-op shutdown must not throw
  });

  it('treats an empty exporter as none', async () => {
    await init({ exporter: '', serviceName: 'automation-agent' });
    expect(isEnabled()).toBe(false);
  });

  it('rejects an unknown exporter', async () => {
    await expect(init({ exporter: 'jaeger', serviceName: 'automation-agent' })).rejects.toThrow(
      /unknown OTEL_TRACES_EXPORTER/,
    );
  });

  it('registers a provider, propagator, and context manager for console', async () => {
    const shutdown = await init({ exporter: TracesExporter.Console, serviceName: 'automation-agent' });
    try {
      expect(isEnabled()).toBe(true);
      // A W3C traceparent must round-trip through the registered global propagator.
      const span = trace.getTracer('t').startSpan('s');
      const carrier: Record<string, string> = {};
      propagation.inject(trace.setSpan(context.active(), span), carrier);
      span.end();
      expect(carrier.traceparent).toBeTruthy();
    } finally {
      await shutdown();
    }
  });

  it('rejects otlp with no endpoint', async () => {
    await expect(init({ exporter: TracesExporter.Otlp, serviceName: 'automation-agent' })).rejects.toThrow(
      /OTLP endpoint/,
    );
  });

  it('builds an otlp provider with an endpoint (no live collector needed)', async () => {
    const shutdown = await init({
      exporter: TracesExporter.Otlp,
      serviceName: 'automation-agent',
      otlpEndpoint: 'http://localhost:4318/v1/traces',
      otlpHeaders: 'api-key=secret',
    });
    try {
      expect(isEnabled()).toBe(true);
    } finally {
      await shutdown();
    }
  });

  it('builds a gcp provider (or surfaces only a missing-credentials error)', async () => {
    // The Cloud Trace exporter authenticates lazily, so init typically builds a provider even with
    // no Application Default Credentials. Accept only a credentials error if one is raised — an
    // import error or any other regression must still fail the test rather than be swallowed.
    let shutdown;
    try {
      shutdown = await init({ exporter: TracesExporter.Gcp, serviceName: 'automation-agent' });
    } catch (err) {
      expect((err as Error).message).toMatch(/credential/i);
      return;
    }
    try {
      expect(isEnabled()).toBe(true);
    } finally {
      await shutdown();
    }
  });

  it('shutdown resets the enabled state', async () => {
    const shutdown = await init({ exporter: TracesExporter.Console, serviceName: 'automation-agent' });
    expect(isEnabled()).toBe(true);
    await shutdown();
    expect(isEnabled()).toBe(false);
  });
});

describe('obs span tree + flush', () => {
  afterEach(resetOtel);

  it('records the agent span tree with GenAI attribute keys', async () => {
    const { exporter, shutdown } = recording();
    try {
      emitFakeAgentTree();
      await flush();

      const spans = exporter.getFinishedSpans();
      expect(spans).toHaveLength(3);
      const byName = new Map(spans.map((s) => [s.name, s]));
      const invoke = byName.get('invoke_agent automation_agent')!;
      const llm = byName.get('call_llm gemma')!;
      const tool = byName.get('execute_tool apply_fix')!;

      // Tree shape: call_llm is a child of invoke_agent; execute_tool a child of call_llm; all
      // three share one trace.
      expect(llm.parentSpanContext?.spanId).toBe(invoke.spanContext().spanId);
      expect(tool.parentSpanContext?.spanId).toBe(llm.spanContext().spanId);
      expect(invoke.spanContext().traceId).toBe(llm.spanContext().traceId);
      expect(llm.spanContext().traceId).toBe(tool.spanContext().traceId);

      // GenAI-semconv attribute keys are preserved (assert on keys/structure, not LLM text).
      expect(llm.attributes['gen_ai.request.model']).toBeDefined();
      expect(llm.attributes['gen_ai.usage.input_tokens']).toBeDefined();
      expect(tool.attributes['gen_ai.tool.name']).toBeDefined();
    } finally {
      await shutdown();
    }
  });

  it('exports buffered spans only after a flush', async () => {
    const { exporter, shutdown } = recording();
    try {
      emitFakeAgentTree();
      // The batch span processor buffers ended spans and exports on a background timer. Without an
      // explicit flush nothing has shipped yet — this is the scale-to-zero loss window.
      expect(exporter.getFinishedSpans()).toHaveLength(0);
      await flush();
      expect(exporter.getFinishedSpans().length).toBeGreaterThan(0);
    } finally {
      await shutdown();
    }
  });

  it('flush is a no-op when tracing is disabled', async () => {
    // With no provider registered, flush must be a safe no-op rather than throw.
    await expect(flush()).resolves.toBeUndefined();
  });
});

describe('obs sampler / header parsing', () => {
  it('maps standard sampler values, defaulting on unknown/empty', () => {
    // Unknown/empty fall back to the parent-based always-on default rather than failing.
    for (const name of ['', 'parentbased_always_on', 'nonsense']) {
      expect(parseSampler(name).toString()).toMatch(/^ParentBased\{root=AlwaysOnSampler/);
    }
    expect(parseSampler('always_on').toString()).toBe('AlwaysOnSampler');
    expect(parseSampler('always_off').toString()).toBe('AlwaysOffSampler');
    expect(parseSampler('parentbased_always_off').toString()).toMatch(/^ParentBased\{root=AlwaysOffSampler/);
  });

  it('parses OTLP headers, skipping blanks and keyless entries', () => {
    expect(parseOtlpHeaders('api-key=secret , env=prod,bad,=novalue,k=a=b')).toEqual({
      'api-key': 'secret',
      env: 'prod',
      k: 'a=b',
    });
  });

  it('newExporter rejects an unknown exporter', async () => {
    // init pre-validates, but newExporter guards defensively so a direct caller fails loudly.
    await expect(newExporter({ exporter: 'jaeger', serviceName: 'automation-agent' })).rejects.toThrow(
      /unknown OTEL_TRACES_EXPORTER/,
    );
  });

  it('newExporter rejects otlp with no endpoint', async () => {
    await expect(newExporter({ exporter: TracesExporter.Otlp, serviceName: 'automation-agent' })).rejects.toThrow(
      /requires an OTLP endpoint/,
    );
  });
});
