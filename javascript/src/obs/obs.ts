/**
 * The tracer-provider registration at the heart of the obs package.
 *
 * It builds and globally registers an OpenTelemetry tracer provider so the agent framework's
 * native span tree (the `invoke_agent` -> `call_llm` -> `execute_tool` tree it already emits
 * under the GenAI semantic conventions) is exported instead of discarded. We own the provider;
 * the agent framework inherits it via the OTel global — its tracer resolves the global provider
 * lazily on first span, so registering ours first is all it takes, and this package never calls
 * the framework's own telemetry setup. Everything is off by default: with the exporter set to
 * `none` (the default) {@link init} is a no-op and nothing about the running service changes.
 *
 * Deterministic tooling — it imports no agent modules, and only `config` reads the `OTEL_*`
 * environment (this package takes a typed {@link Config}). See
 * `.agents/standards/observability.md`.
 */

import { context, propagation, trace } from '@opentelemetry/api';
import { AsyncLocalStorageContextManager } from '@opentelemetry/context-async-hooks';
import { W3CTraceContextPropagator } from '@opentelemetry/core';
import { resourceFromAttributes } from '@opentelemetry/resources';
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  BasicTracerProvider,
  BatchSpanProcessor,
  ParentBasedSampler,
  type Sampler,
  type SpanExporter,
} from '@opentelemetry/sdk-trace-base';

import { newExporter } from './exporters';

/**
 * The trace sink. The application speaks exactly these four; it never names a vendor. Any
 * OTLP-native backend is reached with {@link TracesExporter.Otlp} plus an endpoint (and optional
 * headers), so switching vendors is a config change, not a code change.
 */
export const TracesExporter = {
  /** The no-op default: init installs nothing, so merging changes nothing until an operator opts in. */
  None: 'none',
  /** Spans as text on stdout — local dev and the playground. */
  Console: 'console',
  /** OTLP over HTTP to an endpoint (any OTLP-native backend or a local Collector). */
  Otlp: 'otlp',
  /** Google Cloud Trace via Application Default Credentials. */
  Gcp: 'gcp',
} as const;
export type TracesExporter = (typeof TracesExporter)[keyof typeof TracesExporter];

/** Labels spans when {@link Config.serviceVersion} is unset. */
export const DEFAULT_SERVICE_VERSION = 'dev';

/**
 * The typed observability configuration. `config` reads the `OTEL_*` environment into these
 * fields; this package never touches the environment, so the "only config reads env" boundary
 * holds.
 */
export interface Config {
  /** One of the {@link TracesExporter} values. Empty is treated as {@link TracesExporter.None}. */
  exporter: string;
  /** The resource `service.name` attribute on every span. */
  serviceName: string;
  /** The resource `service.version` attribute; empty uses {@link DEFAULT_SERVICE_VERSION}. */
  serviceVersion?: string;
  /**
   * The OTLP/HTTP target URL ({@link TracesExporter.Otlp} only). config rejects an empty
   * endpoint for that exporter, so by the time init runs it is set.
   */
  otlpEndpoint?: string;
  /**
   * The standard `OTEL_EXPORTER_OTLP_HEADERS` value: comma-separated key=value pairs, used as
   * OTLP request headers ({@link TracesExporter.Otlp} only).
   */
  otlpHeaders?: string;
  /** A standard `OTEL_TRACES_SAMPLER` value (e.g. `parentbased_always_on`). */
  sampler?: string;
}

/**
 * Flushes and releases the tracer provider. Always safe to call (a no-op when tracing is
 * disabled) and returned from {@link init} for the entrypoint's shutdown path.
 */
export type Shutdown = () => Promise<void>;

const noopShutdown: Shutdown = () => Promise.resolve();

// The provider we registered, kept so flush/shutdown can reach it directly. The OTel global is a
// proxy that does not expose forceFlush, so resolving it here (rather than through the global) is
// both simpler and correct. Undefined means tracing is disabled — every entry point is a no-op.
let activeProvider: BasicTracerProvider | undefined;

/** Whether tracing is enabled (a provider was registered). The middleware uses it to stay a
 * true no-op on the default path. */
export function isEnabled(): boolean {
  return activeProvider !== undefined;
}

/**
 * Build the tracer provider for `cfg`, register it as the OTel global, and set the global W3C
 * TraceContext propagator. The agent framework then attaches its native spans to our provider.
 * With {@link TracesExporter.None} (the default) it installs nothing and returns a no-op
 * {@link Shutdown}, leaving the process exactly as it was. The returned Shutdown should be
 * called in the entrypoint's shutdown path; it force-flushes buffered spans before releasing the
 * provider (the scale-to-zero span-loss guard, mirrored by {@link flush} on the request path).
 *
 * @throws Error if `cfg.exporter` is not one of none|console|otlp|gcp.
 */
export async function init(cfg: Config): Promise<Shutdown> {
  const name = cfg.exporter || TracesExporter.None;
  if (name === TracesExporter.None) {
    return noopShutdown;
  }
  if (name !== TracesExporter.Console && name !== TracesExporter.Otlp && name !== TracesExporter.Gcp) {
    throw new Error(`obs: unknown OTEL_TRACES_EXPORTER ${JSON.stringify(name)} (want none|console|otlp|gcp)`);
  }
  return install(await newExporter(cfg), cfg);
}

/**
 * Build our SDK tracer provider over `exporter`, set it as the OTel global, and register the W3C
 * propagator and an async-context manager. This is the shared tail of {@link init} and the test
 * seam that injects a recording exporter. The provider uses a batch span processor (async
 * export, efficient for the many spans an agent run emits); {@link flush} forces it out
 * in-request.
 *
 * The async-context manager is required so a span set active in one async frame is still active
 * in the awaited continuations beneath it — without it the framework's nested spans would not
 * parent correctly and the in-process transport could not carry the trace to its detached
 * dispatch.
 */
export function install(exporter: SpanExporter, cfg: Config): Shutdown {
  const version = cfg.serviceVersion || DEFAULT_SERVICE_VERSION;
  const provider = new BasicTracerProvider({
    resource: resourceFromAttributes({ 'service.name': cfg.serviceName, 'service.version': version }),
    sampler: parseSampler(cfg.sampler ?? ''),
    spanProcessors: [new BatchSpanProcessor(exporter)],
  });
  // setGlobalTracerProvider refuses (returns false) if a provider is already registered. Fail loudly
  // rather than tracking a provider that is not the global one: that would leave flush/shutdown
  // acting on a provider whose spans the framework never uses. init runs once per process, so a
  // second call is a wiring bug.
  if (!trace.setGlobalTracerProvider(provider)) {
    throw new Error('obs: a tracer provider is already registered (init must run once per process)');
  }
  propagation.setGlobalPropagator(new W3CTraceContextPropagator());
  context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable());
  activeProvider = provider;

  return async () => {
    // provider.shutdown() force-flushes buffered spans, then releases the processor.
    await provider.shutdown();
    activeProvider = undefined;
  };
}

/**
 * Map a standard `OTEL_TRACES_SAMPLER` value to a Sampler. The default `parentbased_always_on`
 * records every locally-started trace and honors an upstream sampling decision — correct here
 * because trace volume is one-per-webhook (the cost is spans-per-trace, not trace rate). An
 * unrecognized value falls back to the default rather than failing: the sampler is advisory, not
 * a correctness gate.
 */
export function parseSampler(name: string): Sampler {
  switch (name.trim()) {
    case 'always_on':
      return new AlwaysOnSampler();
    case 'always_off':
      return new AlwaysOffSampler();
    case 'parentbased_always_off':
      return new ParentBasedSampler({ root: new AlwaysOffSampler() });
    default:
      return new ParentBasedSampler({ root: new AlwaysOnSampler() });
  }
}

/**
 * Force any buffered spans out through the exporter now. The HTTP middleware calls it before a
 * traced response completes: the batch span processor exports on a background timer, but Cloud
 * Run throttles CPU the instant a response is sent, so an un-flushed trailing batch would be lost
 * on scale-to-zero. It is a no-op when tracing is disabled.
 */
export async function flush(): Promise<void> {
  if (activeProvider !== undefined) {
    await activeProvider.forceFlush();
  }
}
